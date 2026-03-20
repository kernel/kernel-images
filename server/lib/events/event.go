package events

import (
	"encoding/json"
	"strings"
)

// maxS2RecordBytes is the S2 record size limit (SCHEMA-04).
const maxS2RecordBytes = 1_000_000

// EventCategory maps event type prefixes to log file names.
type EventCategory string

const (
	CategoryCDP      EventCategory = "cdp"
	CategoryConsole  EventCategory = "console"
	CategoryNetwork  EventCategory = "network"
	CategoryLiveview EventCategory = "liveview"
	CategoryCaptcha  EventCategory = "captcha"
)

// BrowserEvent is the canonical event structure for the browser capture pipeline.
type BrowserEvent struct {
	CaptureSessionID string          `json:"capture_session_id"`
	Seq              uint64          `json:"seq"`
	Ts               int64           `json:"ts"`
	Type             string          `json:"type"`
	TargetID         string          `json:"target_id,omitempty"`
	CDPSessionID     string          `json:"cdp_session_id,omitempty"`
	FrameID          string          `json:"frame_id,omitempty"`
	ParentFrameID    string          `json:"parent_frame_id,omitempty"`
	URL              string          `json:"url,omitempty"`
	Data             json.RawMessage `json:"data,omitempty"`
	Truncated        bool            `json:"truncated,omitempty"`
}

// CategoryFor returns the log category for a given event type.
// Event types follow the pattern "<category>_<subtype>", e.g. "console_log",
// "network_request", "cdp_navigation". Types not matching a known prefix
// fall through to CategoryCDP as a safe default.
func CategoryFor(eventType string) EventCategory {
	prefix, _, _ := strings.Cut(eventType, "_")
	switch prefix {
	case "console":
		return CategoryConsole
	case "network":
		return CategoryNetwork
	case "liveview":
		return CategoryLiveview
	case "captcha":
		return CategoryCaptcha
	default:
		return CategoryCDP
	}
}

// truncateIfNeeded returns a copy of ev with Data replaced with json.RawMessage("null")
// and Truncated set to true if the marshaled size exceeds maxS2RecordBytes.
// Per RESEARCH pitfall 3: never attempt byte-slice truncation of the Data field.
func truncateIfNeeded(ev BrowserEvent) BrowserEvent {
	candidate, err := json.Marshal(ev)
	if err != nil || len(candidate) <= maxS2RecordBytes {
		return ev
	}
	ev.Data = json.RawMessage("null")
	ev.Truncated = true
	return ev
}
