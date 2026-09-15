package browsersurface

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIndependentWorkersOutliveCreatingTab(t *testing.T) {
	tracker := New(newFakeProtocol(), WithoutLocations(),
		WithAdditionalTargets("worker", "shared_worker", "service_worker", "background_page"))
	defer tracker.stop()
	page := targetInfo{TargetID: "page-a", Type: "page"}
	tracker.registerPage(page)
	tracker.addSession("page", "", page)
	for _, targetType := range []string{"iframe", "worker", "shared_worker", "service_worker", "background_page"} {
		tracker.addSession(targetType, "page", targetInfo{
			TargetID: targetType, Type: targetType, ParentFrameID: "page-a",
		})
	}
	// A dedicated worker spawned by a shared worker follows that worker's
	// lifetime, even if parentFrameId still identifies the original page.
	tracker.addSession("shared-child", "shared_worker", targetInfo{
		TargetID: "shared-child", Type: "worker", ParentFrameID: "page-a",
	})

	tracker.removeTarget("page-a")
	for _, sessionID := range []string{"page", "iframe", "worker"} {
		require.False(t, tracker.SessionExists(sessionID), sessionID)
	}
	for _, sessionID := range []string{"shared_worker", "service_worker", "background_page", "shared-child"} {
		require.True(t, tracker.SessionExists(sessionID), sessionID)
	}
	tracker.removeTarget("shared_worker")
	require.False(t, tracker.SessionExists("shared_worker"))
	require.False(t, tracker.SessionExists("shared-child"))
	require.True(t, tracker.SessionExists("service_worker"))
}
