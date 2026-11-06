package api

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dialFakeDevTools dials a fakeDevTools fixture, the same CDP surface
// reconcileWindow talks to in production.
func dialFakeDevTools(t *testing.T, url string) *cdpclient.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := cdpclient.Dial(ctx, url)
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	return client
}

// windowRestoreCalls reads the fixture's counter without racing its handler.
func windowRestoreCalls(f *fakeDevTools) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setMaximized
}

func TestReconcileWindowRestoresDemotedWindow(t *testing.T) {
	f := &fakeDevTools{pageTargetID: "target-1", windowState: "normal"}
	client := dialFakeDevTools(t, f.start(t))

	var logs bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	assert.True(t, reconcileWindow(ctx, client, slog.New(slog.NewTextHandler(&logs, nil))),
		"a window taken out of maximized is restored")
	assert.Equal(t, 1, windowRestoreCalls(f))

	// The replaced bounds are what make a demotion attributable after the fact,
	// so the line has to carry them.
	for _, want := range []string{"window was not maximized", "normal", "800", "600"} {
		assert.Contains(t, logs.String(), want)
	}
}

func TestReconcileWindowLeavesMaximizedWindow(t *testing.T) {
	f := &fakeDevTools{pageTargetID: "target-1", windowState: "maximized"}
	client := dialFakeDevTools(t, f.start(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	assert.False(t, reconcileWindow(ctx, client, slog.Default()))
	assert.Zero(t, windowRestoreCalls(f))
}

func TestReconcileWindowLeavesFullscreenWindow(t *testing.T) {
	// A kiosk session is fullscreen, and demoting it to maximized would break
	// kiosk mode.
	f := &fakeDevTools{pageTargetID: "target-1", windowState: "fullscreen"}
	client := dialFakeDevTools(t, f.start(t))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	assert.False(t, reconcileWindow(ctx, client, slog.Default()))
	assert.Zero(t, windowRestoreCalls(f))
}
