package events

//go:generate go run github.com/kernel/kernel-images/server/scripts/categorygen -openapi ../../openapi.yaml -out category_gen.go

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
	Control     = oapi.TelemetryEventCategory("control")
	Connection  = oapi.TelemetryEventCategory("connection")
	System      = oapi.TelemetryEventCategory("system")
	Screenshot  = oapi.TelemetryEventCategory("screenshot")
	Captcha     = oapi.TelemetryEventCategory("captcha")
	Monitor     = oapi.TelemetryEventCategory("monitor")
)

// UserCategories are the categories a caller can configure via the telemetry
// config. Monitor is excluded: it is CDP-collector health metadata that flows
// automatically whenever a CDP category is captured, not a configurable knob.
var UserCategories = []oapi.TelemetryEventCategory{
	Console,
	Network,
	Page,
	Interaction,
	Control,
	Connection,
	System,
	Screenshot,
	Captcha,
}

// DefaultCategories is captured when the caller enables telemetry without
// per-category settings: every configurable category except Screenshot, which
// is high-volume base64 image data and therefore opt-in.
var DefaultCategories = []oapi.TelemetryEventCategory{
	Console,
	Network,
	Page,
	Interaction,
	Control,
	Connection,
	System,
	Captcha,
}

// cdpCategories are produced by the CDP collector. Enabling any of them starts
// the collector, and Monitor (collector health) rides along while it runs.
var cdpCategories = map[oapi.TelemetryEventCategory]struct{}{
	Console:     {},
	Network:     {},
	Page:        {},
	Interaction: {},
	Screenshot:  {},
}

// HasCDPCategory reports whether the set contains any CDP-collector category.
// It is the single predicate gating both the collector start and Monitor
// inclusion, so the two can never diverge.
func HasCDPCategory(cats []oapi.TelemetryEventCategory) bool {
	for _, c := range cats {
		if _, ok := cdpCategories[c]; ok {
			return true
		}
	}
	return false
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
