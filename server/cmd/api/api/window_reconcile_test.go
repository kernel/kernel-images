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

// newWindowGuard builds a guard against a fake devtools, the way
// guardWindowCoverage builds one per browser start.
func newWindowGuard(t *testing.T, url string, logs *bytes.Buffer) *windowGuard {
	t.Helper()
	g := &windowGuard{url: url, budget: windowCorrectionBudget, log: slog.New(slog.NewTextHandler(logs, nil))}
	t.Cleanup(g.close)
	return g
}

func TestWindowGuardCorrectsAnUncoveredWindow(t *testing.T) {
	f := &fakeDevTools{pageTargetID: "target-1", windowState: "normal"}
	var logs bytes.Buffer
	g := newWindowGuard(t, f.start(t), &logs)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	assert.True(t, g.onCoverage(ctx, false), "the guard keeps watching while it has budget")
	assert.Equal(t, 1, windowRestoreCalls(f))
}

func TestWindowGuardIgnoresACoveredWindow(t *testing.T) {
	// A maximized window reports a state change of its own when it is restored,
	// and reconnecting to the browser to confirm what the atoms already say
	// would double every correction.
	f := &fakeDevTools{pageTargetID: "target-1", windowState: "normal"}
	var logs bytes.Buffer
	g := newWindowGuard(t, f.start(t), &logs)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	assert.True(t, g.onCoverage(ctx, true))
	assert.Zero(t, windowRestoreCalls(f))
}

func TestWindowGuardStopsAfterTheBudget(t *testing.T) {
	// A client that keeps taking the window back wants it that size. The guard
	// stands down rather than fighting it for the life of the session.
	f := &fakeDevTools{pageTargetID: "target-1", windowState: "normal"}
	var logs bytes.Buffer
	g := newWindowGuard(t, f.start(t), &logs)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 1; i < windowCorrectionBudget; i++ {
		require.True(t, g.onCoverage(ctx, false), "correction %d is within budget", i)
	}
	assert.False(t, g.onCoverage(ctx, false), "the last correction ends the watch")
	assert.Equal(t, windowCorrectionBudget, windowRestoreCalls(f))
	assert.Contains(t, logs.String(), "budget spent")

	// Spent is spent: a later change costs nothing and starts no work.
	assert.False(t, g.onCoverage(ctx, false))
	assert.Equal(t, windowCorrectionBudget, windowRestoreCalls(f))
}

func TestWindowGuardKeepsWatchingWhenTheWindowIsAlreadyMaximized(t *testing.T) {
	// The atoms can lag a window that CDP already reports as maximized. That
	// is not a correction, so it does not spend budget.
	f := &fakeDevTools{pageTargetID: "target-1", windowState: "maximized"}
	var logs bytes.Buffer
	g := newWindowGuard(t, f.start(t), &logs)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	assert.True(t, g.onCoverage(ctx, false))
	assert.Zero(t, windowRestoreCalls(f))
	assert.Equal(t, windowCorrectionBudget, g.budget)
}
