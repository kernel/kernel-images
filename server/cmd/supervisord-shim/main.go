// Command supervisord-shim is a tiny supervisord eventlistener that
// translates PROCESS_STATE_EXITED (expected=0) and PROCESS_STATE_FATAL events
// into BrowserServiceCrashedEvent payloads and POSTs them to the local
// kernel-images-api telemetry endpoint.
//
// All schema-mapping and event publishing logic lives here; lib/sysmon does
// not handle supervisord events. Keeping the shim as the sole owner of the
// supervisord protocol means lib/sysmon stays single-purpose (kmsg only).
//
// Wire protocol per supervisord docs:
//
//	stdout: "READY\n"
//	stdin:  header line ("ver:3.0 ... eventname:PROCESS_STATE_EXITED len:54\n")
//	stdin:  payload of `len` bytes (no trailing newline)
//	stdout: "RESULT 2\nOK\n"  (always; ACK regardless of downstream success)
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
		if ev, ok := mapEvent(header, payload); ok {
			if perr := pub.publish(context.Background(), ev); perr != nil {
				log.Printf("publish telemetry event: %v", perr)
			}
		}

		if _, err := out.WriteString("RESULT 2\nOK\n"); err != nil {
			log.Fatalf("write RESULT: %v", err)
		}
		if err := out.Flush(); err != nil {
			log.Fatalf("flush RESULT: %v", err)
		}
	}
}

// readEvent reads one supervisord event: a header line followed by a payload
// of declared length.
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
// Values are split on the first colon; supervisord does not escape colons in
// values, but in practice the values we care about (process names, states,
// ints) never contain them.
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

// telemetryEventBody mirrors oapi.TelemetryEvent but is duplicated here so the
// shim does not pull in the entire server module — keeps the binary tiny.
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
	FromState   string `json:"from_state"`
	Pid         *int   `json:"pid,omitempty"`
}

// mapEvent decides whether to publish and constructs the event payload.
// Returns ok=false for events we deliberately skip (intentional stops,
// non-crash event types).
func mapEvent(header, payload map[string]string) (telemetryEventBody, bool) {
	eventName := header["eventname"]
	switch eventName {
	case "PROCESS_STATE_EXITED":
		// expected=0 means the exit was not in `exitcodes` — i.e. a crash.
		// expected=1 means clean shutdown (supervisorctl stop, or a configured
		// exitcode). Skip the latter.
		if payload["expected"] != "0" {
			return telemetryEventBody{}, false
		}
	case "PROCESS_STATE_FATAL":
		// FATAL: supervisord exhausted startretries. Always a crash.
	default:
		return telemetryEventBody{}, false
	}

	name := payload["processname"]
	if name == "" {
		return telemetryEventBody{}, false
	}
	fromState := payload["from_state"]
	if fromState == "" {
		return telemetryEventBody{}, false
	}

	body := telemetryEventBody{
		Type:     "service_crashed",
		Category: "system",
		Source: telemetryEventSource{
			Kind:  "local_process",
			Event: "supervisord.process_" + strings.ToLower(strings.TrimPrefix(eventName, "PROCESS_STATE_")),
		},
		Data: serviceCrashedPayload{
			ServiceName: name,
			FromState:   fromState,
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
