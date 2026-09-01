package browsersurface

import (
	"encoding/json"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

func (t *Tracker) handleProtocolEvent(message cdpclient.Message) {
	switch message.Method {
	case "Target.targetCreated":
		var event struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			switch event.TargetInfo.Type {
			case "page":
				t.trackPage(event.TargetInfo, true)
			case "iframe":
				t.trackFrameTarget(event.TargetInfo)
			}
		}
	case "Target.targetInfoChanged":
		var event struct {
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			t.updateTarget(event.TargetInfo)
		}
	case "Target.targetDestroyed":
		var event struct {
			TargetID string `json:"targetId"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			t.removeTarget(event.TargetID)
		}
	case "Target.attachedToTarget":
		var event struct {
			SessionID  string     `json:"sessionId"`
			TargetInfo targetInfo `json:"targetInfo"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			t.addSession(event.SessionID, message.SessionID, event.TargetInfo)
		}
	case "Target.detachedFromTarget":
		var event struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			t.removeSession(event.SessionID)
		}
	case "Page.frameAttached":
		var event struct {
			FrameID       string `json:"frameId"`
			ParentFrameID string `json:"parentFrameId"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			t.attachFrame(message.SessionID, event.FrameID, event.ParentFrameID)
		}
	case "Page.frameStartedLoading":
		var event struct {
			FrameID string `json:"frameId"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			t.publish(Event{Kind: EventDocumentInvalidated, SessionID: message.SessionID, FrameID: event.FrameID})
		}
	case "Page.frameNavigated":
		var event struct {
			Frame frameInfo `json:"frame"`
		}
		if json.Unmarshal(message.Params, &event) == nil {
			t.navigateFrame(message.SessionID, event.Frame)
		}
	case "Page.frameDetached":
		var event struct {
			FrameID string `json:"frameId"`
			Reason  string `json:"reason"`
		}
		if json.Unmarshal(message.Params, &event) == nil && event.Reason != "swap" {
			t.removeFrame(event.FrameID)
		}
	}
	t.publish(Event{Kind: EventProtocol, SessionID: message.SessionID, Message: message})
}
