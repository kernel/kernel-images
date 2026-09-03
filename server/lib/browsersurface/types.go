package browsersurface

import (
	"context"
	"encoding/json"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

type Protocol interface {
	Send(ctx context.Context, method string, params any, sessionID string) (json.RawMessage, error)
	Events() <-chan cdpclient.Message
	IsClosed() bool
}

type EventKind int

const (
	EventProtocol EventKind = iota
	EventSessionReady
	EventSessionRemoved
	EventDocumentInvalidated
	EventDocumentChanged
	EventFrameInvalidated
	EventFrameRemoved
)

type Event struct {
	Kind      EventKind
	SessionID string
	FrameID   string
	Message   cdpclient.Message
}

type WindowInfo struct {
	ID int
}

type TabInfo struct {
	ID        int
	WindowID  int
	PageTitle string
	PageURL   string
}

type FrameInfo struct {
	ID            int
	TabID         int
	ParentFrameID int
	URL           string
}

type Snapshot struct {
	Windows []WindowInfo
	Tabs    []TabInfo
	Frames  []FrameInfo
}

type FrameLocation struct {
	ID  int
	URL string
}

type Location struct {
	WindowID  int
	TabID     int
	PageTitle string
	PageURL   string
	Frame     *FrameLocation
}

type targetInfo struct {
	TargetID      string `json:"targetId"`
	Type          string `json:"type"`
	Title         string `json:"title"`
	URL           string `json:"url"`
	ParentFrameID string `json:"parentFrameId,omitempty"`
}

type frameInfo struct {
	ID       string `json:"id"`
	ParentID string `json:"parentId,omitempty"`
	LoaderID string `json:"loaderId"`
	URL      string `json:"url"`
}

type frameTree struct {
	Frame       frameInfo   `json:"frame"`
	ChildFrames []frameTree `json:"childFrames,omitempty"`
}

type session struct {
	id           string
	parentID     string
	target       targetInfo
	tabID        int
	initializing bool
	initialized  bool
}

type window struct {
	id    int
	rawID int64
}

type tab struct {
	id          int
	targetID    string
	windowID    int
	rawWindowID int64
	title       string
	url         string
	rootFrameID string
}

type frame struct {
	id       int
	rawID    string
	parentID string
	tabID    int
	url      string
}
