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
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTelemetryURL = "http://127.0.0.1:10001/telemetry/events"
	httpTimeout         = 2 * time.Second
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	telemetryURL := os.Getenv("KERNEL_IMAGES_TELEMETRY_URL")
	if telemetryURL == "" {
		telemetryURL = defaultTelemetryURL
	}

	pub := &publisher{
		url:    telemetryURL,
		client: &http.Client{Timeout: httpTimeout},
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
			if perr := pub.publish(context.Background(), ev); perr != nil {
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

// writeResultOK writes the supervisord eventlistener "RESULT" frame
// indicating success. The body is exactly "OK" (2 bytes) with NO trailing
// newline — supervisord reads exactly `len` bytes after the header
// newline, and a trailing newline would leak into the buffer and corrupt
// the subsequent READY token.
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

// telemetryEventBody mirrors oapi.TelemetryEvent but is duplicated here
// so the shim does not pull in the entire server module — keeps the
// binary tiny. Field names track openapi.yaml by convention.
type telemetryEventBody struct {
	Type     string                `json:"type"`
	Category string                `json:"category"`
	Source   telemetryEventSource  `json:"source"`
	Data     serviceCrashedPayload `json:"data"`
}

type telemetryEventSource struct {
	Kind  string `json:"kind"`
	Event string `json:"event"`
}

type serviceCrashedPayload struct {
	ServiceName string `json:"service_name"`
	Phase       string `json:"phase"`
	Pid         *int   `json:"pid,omitempty"`
}

// phaseFromSupervisordState maps the process manager's pre-exit state to
// the neutral lifecycle phase exposed in the public event schema. The
// goal is to keep the supervisord vocabulary out of the API contract.
func phaseFromSupervisordState(fromState string) string {
	switch fromState {
	case "RUNNING":
		return "running"
	case "STARTING":
		return "startup"
	case "BACKOFF":
		// PROCESS_STATE_FATAL transitions out of BACKOFF after the
		// process manager exhausts its restart attempts.
		return "gave_up"
	default:
		return ""
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
func mapEvent(header, payload map[string]string) (telemetryEventBody, bool) {
	eventName := header["eventname"]
	switch eventName {
	case "PROCESS_STATE_EXITED":
		// expected=0 means the exit was not in `exitcodes` — i.e. a
		// crash. expected=1 means clean shutdown (operator-initiated
		// stop, or a configured exit code). Skip the latter.
		if payload["expected"] != "0" {
			return telemetryEventBody{}, false
		}
	case "PROCESS_STATE_FATAL":
		// FATAL: the process manager exhausted startretries. Always a
		// crash from the user's perspective.
	default:
		return telemetryEventBody{}, false
	}

	name := payload["processname"]
	if name == "" {
		return telemetryEventBody{}, false
	}
	phase := phaseFromSupervisordState(payload["from_state"])
	if phase == "" {
		return telemetryEventBody{}, false
	}

	body := telemetryEventBody{
		Type:     "service_crashed",
		Category: "system",
		Source: telemetryEventSource{
			Kind:  "local_process",
			Event: "service.crashed",
		},
		Data: serviceCrashedPayload{
			ServiceName: name,
			Phase:       phase,
		},
	}
	if pidStr := payload["pid"]; pidStr != "" {
		if pid, err := strconv.Atoi(pidStr); err == nil {
			body.Data.Pid = &pid
		}
	}
	return body, true
}

type publisher struct {
	url    string
	client *http.Client
}

func (p *publisher) publish(ctx context.Context, body telemetryEventBody) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}
