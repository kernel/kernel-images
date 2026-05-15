package events

import (
	"encoding/json"
	"log/slog"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// maxS2RecordBytes is the maximum record size for the S2 event pipeline (1 MB).
const maxS2RecordBytes = 1_000_000

const (
	Console     = oapi.TelemetryEventCategory("console")
	Network     = oapi.TelemetryEventCategory("network")
	Page        = oapi.TelemetryEventCategory("page")
	Interaction = oapi.TelemetryEventCategory("interaction")
	System      = oapi.TelemetryEventCategory("system")
)

// AllCategories is the canonical list of all configurable event categories.
// System events are always captured regardless of telemetry config.
var AllCategories = []oapi.TelemetryEventCategory{
	Console,
	Network,
	Page,
	Interaction,
	System,
}

// Event is the portable event schema. It contains only producer-emitted content;
// pipeline metadata (seq) lives on the Envelope.
type Event struct {
	Ts        int64                       `json:"ts"` // Unix microseconds (µs since epoch)
	Type      string                      `json:"type"`
	Category  oapi.TelemetryEventCategory `json:"category"`
	Source    oapi.BrowserEventSource     `json:"source"`
	Data      json.RawMessage             `json:"data,omitempty"`
	Truncated bool                        `json:"truncated,omitempty"`
}

// Envelope wraps an Event with pipeline-assigned metadata.
type Envelope struct {
	Seq   uint64 `json:"seq"`
	Event Event  `json:"event"`
}

// truncateIfNeeded marshals env and returns the (possibly truncated) envelope.
// If the envelope still exceeds maxS2RecordBytes after nulling data (e.g. huge
// source.metadata), it is returned as-is, callers must handle nil data.
func truncateIfNeeded(env Envelope) (Envelope, []byte) {
	data, err := json.Marshal(env)
	if err != nil {
		return env, nil
	}
	if len(data) <= maxS2RecordBytes {
		return env, data
	}
	env.Event.Data = json.RawMessage("null")
	env.Event.Truncated = true
	data, err = json.Marshal(env)
	if err != nil {
		return env, nil
	}
	if len(data) > maxS2RecordBytes {
		slog.Warn("truncateIfNeeded: envelope exceeds limit even without data", "seq", env.Seq, "size", len(data))
	}
	return env, data
}
