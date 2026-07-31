// Package neterror tallies Chromium network failures (net::ERR_*) observed on
// the DevTools traffic relayed by the CDP proxy.
//
// Chrome records net errors in the UMA histogram Net.ErrorCodesForMainFrame4,
// but that is written from the renderer process and so is invisible to
// Browser.getHistograms on these images. The CDP proxy is the only always-on
// vantage point that sees failures for every session, so the counts are
// derived from the Network.loadingFailed events already flowing across it.
//
// The tap is passive: it reads relayed frames and never injects or rewrites
// CDP traffic, so it is invisible to automation running in the browser. It
// observes what the client's own CDP session sees, which means a session that
// never sends Network.enable contributes nothing. Playwright, Puppeteer, and
// the CUA app all enable the Network domain by default.
//
// Deliberate client aborts are only partly separable from real failures.
// Cancellations and net::ERR_ABORTED are dropped, but a resource blocker
// calling Playwright route.abort() with its default reason lands on
// net::ERR_FAILED with canceled false and is indistinguishable from a genuine
// generic failure. Rather than drop a real error class, net::ERR_FAILED is
// counted and kept in its own series, so a session that blocks images inflates
// only that series and never the specific ones (ERR_HTTP2_PROTOCOL_ERROR and
// friends) these counts exist to track.
package neterror

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// loadingFailedMethod is the CDP event carrying the net error. It doubles as
// the prescreen needle: every relayed frame is scanned for it, so the JSON
// decode below only runs on the rare frames that actually report a failure.
var loadingFailedMethod = []byte(`"Network.loadingFailed"`)

// netErrorPrefix is the namespace Chromium stamps on the errorText of a
// network failure. errorText is only loosely specified by the protocol, so
// requiring the prefix keeps anything unexpected from consuming a label slot.
const netErrorPrefix = "net::ERR_"

// abortedError is how a client's own cancellation is reported: navigating
// away, closing a tab, or Playwright route.abort({errorCode: 'aborted'}).
const abortedError = "net::ERR_ABORTED"

// maxKeys bounds the tracked (error, resource type) pairs. Both fields are
// Chromium enums, so the real ceiling is well under this; the cap only exists
// so a malformed or hostile upstream cannot grow the map without limit and
// blow up scrape cardinality. Failures past the cap still increment overflow.
const maxKeys = 256

// logInterval throttles the per-key log line. Net errors arrive in bursts —
// one failing page can fail every subresource — and the metric already
// carries the volume, so the log only needs to show that a given failure is
// happening and roughly when.
const logInterval = time.Minute

// Key identifies one class of network failure.
type Key struct {
	// Error is Chromium's error text, e.g. "net::ERR_HTTP2_PROTOCOL_ERROR".
	Error string
	// ResourceType is the CDP resource type, e.g. "Document" or "XHR".
	ResourceType string
}

type entry struct {
	count     int64
	lastLogAt time.Time
}

// Tracker counts network failures. Counts are cumulative for the lifetime of
// the process, matching Prometheus counter semantics. Safe for concurrent use.
type Tracker struct {
	logger *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	entries  map[Key]*entry
	overflow int64
}

func NewTracker(logger *slog.Logger) *Tracker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Tracker{
		logger:  logger,
		now:     time.Now,
		entries: make(map[Key]*entry),
	}
}

// Observe inspects one relayed CDP frame and records a failure if it is a
// Network.loadingFailed event worth counting. Frames that are not failures
// cost a single substring scan.
func (t *Tracker) Observe(msg []byte) {
	if !bytes.Contains(msg, loadingFailedMethod) {
		return
	}
	var frame struct {
		Method string `json:"method"`
		Params struct {
			Type      string `json:"type"`
			ErrorText string `json:"errorText"`
			Canceled  bool   `json:"canceled"`
		} `json:"params"`
	}
	// The needle can also appear inside an unrelated payload (a response body
	// quoting the event name, say), so the decoded method still has to match.
	if err := json.Unmarshal(msg, &frame); err != nil || frame.Method != "Network.loadingFailed" {
		return
	}
	p := frame.Params
	if p.Canceled || p.ErrorText == abortedError || !strings.HasPrefix(p.ErrorText, netErrorPrefix) {
		return
	}
	t.record(Key{Error: p.ErrorText, ResourceType: p.Type})
}

func (t *Tracker) record(k Key) {
	t.mu.Lock()
	e, ok := t.entries[k]
	if !ok {
		if len(t.entries) >= maxKeys {
			t.overflow++
			t.mu.Unlock()
			return
		}
		e = &entry{}
		t.entries[k] = e
	}
	e.count++
	now := t.now()
	shouldLog := now.Sub(e.lastLogAt) >= logInterval
	if shouldLog {
		e.lastLogAt = now
	}
	count := e.count
	t.mu.Unlock()

	if shouldLog {
		t.logger.Warn("chromium network request failed",
			slog.String("error_text", k.Error),
			slog.String("resource_type", k.ResourceType),
			slog.Int64("count", count))
	}
}

// Snapshot returns the current counts sorted by key, plus the number of
// failures dropped because the key cap was reached.
func (t *Tracker) Snapshot() (counts map[Key]int64, overflow int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	counts = make(map[Key]int64, len(t.entries))
	for k, e := range t.entries {
		counts[k] = e.count
	}
	return counts, t.overflow
}

// SortedKeys returns the keys of counts in a stable order so scrapes of an
// unchanged tracker are byte-identical.
func SortedKeys(counts map[Key]int64) []Key {
	keys := make([]Key, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Error != keys[j].Error {
			return keys[i].Error < keys[j].Error
		}
		return keys[i].ResourceType < keys[j].ResourceType
	})
	return keys
}
