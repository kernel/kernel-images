// Command supervisord-shim is a tiny supervisord eventlistener that
// translates PROCESS_STATE_EXITED (expected=0) and PROCESS_STATE_FATAL
// events into BrowserServiceCrashedEvent payloads and POSTs them to the
// local kernel-images-api telemetry endpoint.
//
// All schema-mapping and event publishing logic lives here; lib/sysmon
// does not handle supervisord events. Keeping the shim as the sole owner
// of the supervisord protocol means lib/sysmon stays single-purpose
// (kmsg only).
//
// Wire protocol per supervisord docs (http://supervisord.org/events.html):
//
//	stdout: "READY\n"
//	stdin:  header line ("ver:3.0 ... eventname:PROCESS_STATE_EXITED len:54\n")
//	stdin:  payload of `len` bytes (no trailing newline)
//	stdout: "RESULT 2\nOK"          (always; ACK regardless of downstream success)
//
// The result frame intentionally has NO trailing newline: supervisord
// reads exactly the declared number of bytes after the header newline,
// and a trailing newline would leak into the buffer and corrupt the
// subsequent READY token, deadlocking the listener after one event.
//
// We always ACK with OK so supervisord doesn't quarantine us when the
// downstream HTTP target is briefly unavailable. The events are
// best-effort; if the API is down, we drop and log.
//
// All logging goes to stderr — stdout is the supervisord protocol channel.
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

const (
	defaultAPIBaseURL = "http://127.0.0.1:10001"
	httpTimeout       = 2 * time.Second
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = os.Stdin.Close()
	}()

	baseURL := os.Getenv("KERNEL_IMAGES_API_BASE_URL")
	if baseURL == "" {
		baseURL = defaultAPIBaseURL
	}

	client, err := oapi.NewClientWithResponses(baseURL, oapi.WithHTTPClient(&http.Client{Timeout: httpTimeout}))
	if err != nil {
		log.Fatalf("init oapi client: %v", err)
	}

	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)

	for {
		if _, err := out.WriteString("READY\n"); err != nil {
			log.Fatalf("write READY: %v", err)
		}
		if err := out.Flush(); err != nil {
			log.Fatalf("flush READY: %v", err)
		}

		header, payload, err := readEvent(in)
		if err != nil {
			if err == io.EOF {
				return
			}
			log.Fatalf("read event: %v", err)
		}

		// Try to publish but always ACK supervisord.
		ev, ok := mapEvent(header, payload)
		switch {
		case ok:
			if perr := publish(ctx, client, ev); perr != nil {
				log.Printf("publish telemetry event: %v", perr)
			}
		case isCrashEvent(header["eventname"]):
			// We subscribed to this event type but couldn't map it.
			// Most likely cause: supervisord emitted a from_state we
			// don't have a public phase for. Logging means a future
			// supervisord behavior change shows up in stderr instead
			// of silent telemetry loss.
			log.Printf("skipped crash event: eventname=%q from_state=%q processname=%q expected=%q",
				header["eventname"], payload["from_state"], payload["processname"], payload["expected"])
		}

		if err := writeResultOK(out); err != nil {
			log.Fatalf("write RESULT: %v", err)
		}
	}
}

// writeResultOK ACKs a single event. See the file header for why the
// frame body has no trailing newline.
func writeResultOK(out *bufio.Writer) error {
	if _, err := out.WriteString("RESULT 2\nOK"); err != nil {
		return err
	}
	return out.Flush()
}

// readEvent reads one supervisord event: a header line followed by a
// payload of declared length.
func readEvent(in *bufio.Reader) (map[string]string, map[string]string, error) {
	headerLine, err := in.ReadString('\n')
	if err != nil {
		return nil, nil, err
	}
	header := parseFields(strings.TrimRight(headerLine, "\n"))

	lenStr, ok := header["len"]
	if !ok {
		return nil, nil, fmt.Errorf("missing len in header: %q", headerLine)
	}
	n, err := strconv.Atoi(lenStr)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid len %q: %w", lenStr, err)
	}

	buf := make([]byte, n)
	if _, err := io.ReadFull(in, buf); err != nil {
		return nil, nil, fmt.Errorf("read payload: %w", err)
	}
	payload := parseFields(string(buf))
	return header, payload, nil
}

// parseFields parses supervisord's "key:value key:value" tokenization.
// Values are split on the first colon; supervisord does not escape colons
// in values, but in practice the values we care about (process names,
// states, ints) never contain them.
func parseFields(s string) map[string]string {
	out := make(map[string]string)
	for _, tok := range strings.Fields(s) {
		i := strings.IndexByte(tok, ':')
		if i < 0 {
			continue
		}
		out[tok[:i]] = tok[i+1:]
	}
	return out
}

// phaseForExited maps the supervisord state a process exited from to the
// public lifecycle phase. EXITED in supervisord always originates from
// RUNNING (post-startsecs); STARTING-during-startsecs-violation routes
// through BACKOFF→FATAL, not EXITED. We still defend against STARTING
// here in case a future supervisord version changes the state machine,
// and we treat anything else as "unknown" so the caller logs and skips
// rather than inventing a phase.
func phaseForExited(fromState string) (oapi.BrowserServiceCrashedEventDataPhase, bool) {
	switch fromState {
	case "RUNNING":
		return oapi.BrowserServiceCrashedEventDataPhaseRunning, true
	case "STARTING":
		return oapi.BrowserServiceCrashedEventDataPhaseStartup, true
	default:
		return "", false
	}
}

// isCrashEvent reports whether the supervisord eventname is one we
// subscribed to. Used by the main loop to log when a target event was
// dropped instead of silently skipping it.
func isCrashEvent(eventName string) bool {
	return eventName == "PROCESS_STATE_EXITED" || eventName == "PROCESS_STATE_FATAL"
}

// mapEvent decides whether to publish and constructs the event payload.
// Returns ok=false for events we deliberately skip (intentional stops,
// non-crash event types, or unknown lifecycle transitions).
func mapEvent(header, payload map[string]string) (oapi.PublishEventRequest, bool) {
	var phase oapi.BrowserServiceCrashedEventDataPhase
	switch header["eventname"] {
	case "PROCESS_STATE_EXITED":
		// expected=0 means the exit was not in `exitcodes` — i.e. a
		// crash. expected=1 means clean shutdown (operator-initiated
		// stop, or a configured exit code). Skip the latter.
		if payload["expected"] != "0" {
			return oapi.PublishEventRequest{}, false
		}
		p, ok := phaseForExited(payload["from_state"])
		if !ok {
			return oapi.PublishEventRequest{}, false
		}
		phase = p
	case "PROCESS_STATE_FATAL":
		// FATAL is reached exclusively by the BACKOFF→FATAL edge after
		// supervisord exhausts startretries. The from_state is always
		// BACKOFF here, and the semantic is "gave up trying to start".
		phase = oapi.BrowserServiceCrashedEventDataPhaseGaveUp
	default:
		return oapi.PublishEventRequest{}, false
	}

	name := payload["processname"]
	if name == "" {
		return oapi.PublishEventRequest{}, false
	}

	data := oapi.BrowserServiceCrashedEventData{
		ServiceName: name,
		Phase:       phase,
	}
	if pidStr := payload["pid"]; pidStr != "" {
		if pid, err := strconv.Atoi(pidStr); err == nil {
			data.Pid = &pid
		}
	}

	category := oapi.PublishEventRequestCategory(oapi.TelemetryEventCategorySystem)
	sourceEvent := "service.crashed"
	return oapi.PublishEventRequest{
		Type:     string(oapi.ServiceCrashed),
		Category: &category,
		Source: &oapi.BrowserEventSource{
			Kind:  oapi.LocalProcess,
			Event: &sourceEvent,
		},
		Data: data,
	}, true
}

func publish(ctx context.Context, client *oapi.ClientWithResponses, body oapi.PublishEventRequest) error {
	resp, err := client.PublishTelemetryEventWithResponse(ctx, body)
	if err != nil {
		return err
	}
	if resp.StatusCode() >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode(), bytes.TrimSpace(resp.Body))
	}
	return nil
}
