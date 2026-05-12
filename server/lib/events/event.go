package events

import (
	"encoding/json"
	"log/slog"
)

// maxS2RecordBytes is the maximum record size for the S2 event pipeline (1 MB).
const maxS2RecordBytes = 1_000_000

// EventCategory determines type of logging
type EventCategory string

const (
	CategoryConsole     EventCategory = "console"
	CategoryNetwork     EventCategory = "network"
	CategoryPage        EventCategory = "page"
	CategoryInteraction EventCategory = "interaction"
	CategoryLiveview    EventCategory = "liveview"
	CategoryCaptcha     EventCategory = "captcha"
	CategorySystem      EventCategory = "system"
)

// AllCategories is the canonical list of all known event categories.
var AllCategories = []EventCategory{
	CategoryConsole, CategoryNetwork, CategoryPage, CategoryInteraction,
	CategoryLiveview, CategoryCaptcha, CategorySystem,
}

var validCategories = func() map[EventCategory]struct{} {
	m := make(map[EventCategory]struct{}, len(AllCategories))
	for _, c := range AllCategories {
		m[c] = struct{}{}
	}
	return m
}()

// ValidCategory reports whether c is a known EventCategory.
func ValidCategory(c EventCategory) bool {
	_, ok := validCategories[c]
	return ok
}

type SourceKind string

const (
	KindCDP          SourceKind = "cdp"
	KindKernelAPI    SourceKind = "kernel_api"
	KindExtension    SourceKind = "extension"
	KindLocalProcess SourceKind = "local_process"
)

// Source captures provenance: which producer emitted the event and any
// producer-specific context (e.g. CDP target/session/frame IDs).
type Source struct {
	Kind     SourceKind        `json:"kind"`
	Event    string            `json:"event,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Event is the portable event schema. It contains only producer-emitted content;
// pipeline metadata (seq) lives on the Envelope.
type Event struct {
	Ts        int64           `json:"ts"` // Unix microseconds (µs since epoch)
	Type      string          `json:"type"`
	Category  EventCategory   `json:"category"`
	Source    Source          `json:"source"`
	Data      json.RawMessage `json:"data,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
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
