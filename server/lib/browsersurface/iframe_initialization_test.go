package browsersurface

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExplicitIframeInitializationDoesNotWaitForParent(t *testing.T) {
	tracker := New(newFakeProtocol())
	defer tracker.stop()
	page := targetInfo{TargetID: "page-a", Type: "page"}
	tabID, _ := tracker.registerPage(page)
	tracker.stateMu.Lock()
	// The page's frame is known, but its session initialization is still pending.
	tracker.sessions["session-a"] = &session{id: "session-a", target: page, tabID: tabID, initializing: true}
	tracker.upsertFrameLocked(tabID, frameInfo{ID: "page-a"})
	tracker.stateMu.Unlock()
	tracker.addSession("oopif-session", "", targetInfo{
		TargetID: "oopif", Type: "iframe", ParentFrameID: "page-a",
	})
	defer tracker.removeSession("oopif-session")

	require.Eventually(t, func() bool {
		tracker.stateMu.RLock()
		defer tracker.stateMu.RUnlock()
		sess := tracker.sessions["oopif-session"]
		return sess != nil && sess.initialized
	}, time.Second, 10*time.Millisecond)

	tracker.removeSession("session-a")
	require.True(t, tracker.SessionExists("oopif-session"), "an explicitly attached iframe initializes independently")
}
