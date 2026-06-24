package events

//go:generate go run github.com/kernel/kernel-images/server/scripts/categorygen -openapi ../../openapi.yaml -out category_gen.go

import (
	"encoding/json"
	"log/slog"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// maxS2RecordBytes is the maximum record size for the S2 event pipeline (1 MB).
const maxS2RecordBytes = 1_000_000

// Below this conservative size estimate, JSON overhead cannot push a record
// near the S2 limit, so Publish can avoid a full marshal on the hot path.
const truncateMarshalThreshold = maxS2RecordBytes - 64*1024

const (
	jsonEnvelopeOverhead      = 512
	jsonMetadataEntryOverhead = 16
)

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
// per-category settings: the lightweight operational signals. CDP categories
// (console/network/page/interaction) and screenshot are excluded so the default
// never starts the CDP collector or emits high-volume streams; they are opt-in.
var DefaultCategories = []oapi.TelemetryEventCategory{
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
	// Ts is the event time in Unix microseconds. It must be wall-clock
	// (time.Now()) captured at emit/observe, never a monotonic or other
	// source-derived clock (e.g. a kmsg envelope timestamp), which skews
	// on VM suspend. HTTP-published events are stamped by the API handler;
	// in-process producers must set it themselves.
	Ts        int64                       `json:"ts"`
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
// source.metadata), it is returned as-is with data set to null.
func truncateIfNeeded(env Envelope) (Envelope, []byte) {
	if estimatedEnvelopeBytes(env) < truncateMarshalThreshold {
		return env, nil
	}
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
		slog.Warn("truncateIfNeeded: envelope exceeds limit even without data", "size", len(data))
	}
	return env, data
}

func estimatedEnvelopeBytes(env Envelope) int {
	n := jsonEnvelopeOverhead + len(env.Event.Data)
	n += maxEscapedJSONLen(env.Event.Type)
	n += maxEscapedJSONLen(string(env.Event.Category))
	n += maxEscapedJSONLen(string(env.Event.Source.Kind))
	if env.Event.Source.Event != nil {
		n += maxEscapedJSONLen(*env.Event.Source.Event)
	}
	if env.Event.Source.Metadata != nil {
		for k, v := range *env.Event.Source.Metadata {
			n += jsonMetadataEntryOverhead + maxEscapedJSONLen(k) + maxEscapedJSONLen(v)
		}
	}
	return n
}

func maxEscapedJSONLen(s string) int {
	return len(s) * 6
}
