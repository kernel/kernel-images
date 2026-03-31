package events

import (
	"encoding/json"
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

type Source string

const (
	SourceCDP          Source = "cdp"
	SourceKernelAPI    Source = "kernel_api"
	SourceExtension    Source = "extension"
	SourceLocalProcess Source = "local_process"
)

type DetailLevel string

const (
	DetailMinimal DetailLevel = "minimal"
	DetailStandard DetailLevel = "standard"
	DetailVerbose DetailLevel = "verbose"
	DetailRaw     DetailLevel = "raw"
)

// Event is the canonical event structure for the capture pipeline.
type Event struct {
	CaptureSessionID string          `json:"capture_session_id"`
	Seq              uint64          `json:"seq"`
	Ts               int64           `json:"ts"`
	Type             string          `json:"type"`
	Category         EventCategory   `json:"category"`
	Source           Source          `json:"source"`
	SourceEvent      string          `json:"source_event,omitempty"`
	DetailLevel      DetailLevel     `json:"detail_level"`
	TargetID         string          `json:"target_id,omitempty"`
	CDPSessionID     string          `json:"cdp_session_id,omitempty"`
	FrameID          string          `json:"frame_id,omitempty"`
	ParentFrameID    string          `json:"parent_frame_id,omitempty"`
	URL              string          `json:"url,omitempty"`
	Data             json.RawMessage `json:"data,omitempty"`
	Truncated        bool            `json:"truncated,omitempty"`
}

// truncateIfNeeded marshals ev and returns the (possibly truncated) event together
func truncateIfNeeded(ev Event) (Event, []byte) {
	data, err := json.Marshal(ev)
	if err != nil {
		return ev, data
	}
	if len(data) <= maxS2RecordBytes {
		return ev, data
	}
	ev.Data = json.RawMessage("null")
	ev.Truncated = true
	data, err = json.Marshal(ev)
	if err != nil {
		return ev, nil
	}
	return ev, data
}
