package events

import (
	"encoding/json"
)

// maxS2RecordBytes is the maximum record size for the S2 event pipeline (1 MB).
const maxS2RecordBytes = 1_000_000

// EventCategory is a first-class envelope field that determines log file routing.
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

// SourceKind identifies the provenance of an event — which subsystem produced it.
type SourceKind string

const (
	SourceCDP          SourceKind = "cdp"
	SourceKernelAPI    SourceKind = "kernel_api"
	SourceExtension    SourceKind = "extension"
	SourceLocalProcess SourceKind = "local_process"
)

// DetailLevel controls the verbosity of the event payload.
type DetailLevel string

const (
	DetailMinimal DetailLevel = "minimal"
	DetailDefault DetailLevel = "default"
	DetailVerbose DetailLevel = "verbose"
	DetailRaw     DetailLevel = "raw"
)

// BrowserEvent is the canonical event structure for the browser capture pipeline.
//
// The envelope is designed so that capture config and subscription selectors
// can operate on stable, first-class fields (Category, SourceKind, DetailLevel)
// without parsing the Type string. Type carries semantic identity (e.g.
// "console.log", "network.request"); SourceEvent carries the raw upstream
// event name (e.g. "Runtime.consoleAPICalled") for diagnostics.
//
// DetailLevel is always serialised (no omitempty). Pipeline.Publish defaults it
// to DetailDefault; callers constructing events outside a Pipeline should set it
// explicitly.
type BrowserEvent struct {
	CaptureSessionID string          `json:"capture_session_id"`
	Seq              uint64          `json:"seq"`
	Ts               int64           `json:"ts"`
	Type             string          `json:"type"`
	Category         EventCategory   `json:"category"`
	SourceKind       SourceKind      `json:"source_kind"`
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

// truncateIfNeeded returns a copy of ev with Data replaced with json.RawMessage("null")
// and Truncated set to true if the marshaled size exceeds maxS2RecordBytes.
// Never attempt byte-slice truncation of the Data field — partial JSON is invalid.
func truncateIfNeeded(ev BrowserEvent) BrowserEvent {
	candidate, err := json.Marshal(ev)
	if err != nil {
		// Marshal should never fail for BrowserEvent (all fields are JSON-safe),
		// but if it does return ev unchanged rather than silently nulling Data.
		return ev
	}
	if len(candidate) <= maxS2RecordBytes {
		return ev
	}
	ev.Data = json.RawMessage("null")
	ev.Truncated = true
	return ev
}
