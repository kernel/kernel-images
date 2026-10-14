package ambientmouse

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePointer struct {
	mu    sync.Mutex
	x, y  int
	moves [][2]int
}

func (p *fakePointer) Location(context.Context) (int, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.x, p.y, nil
}

func (p *fakePointer) MoveRelative(_ context.Context, dx, dy int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.x += dx
	p.y += dy
	p.moves = append(p.moves, [2]int{dx, dy})
	return nil
}

func (p *fakePointer) ScreenSize(context.Context) (int, int, error) {
	return 1920, 1080, nil
}

func (p *fakePointer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.moves)
}

func (p *fakePointer) jump(x, y int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.x, p.y = x, y
}

func testConfig() Config {
	return Config{
		MovesPerPage:      20,
		StartDelay:        10 * time.Millisecond,
		Settle:            50 * time.Millisecond,
		AgentCooldown:     150 * time.Millisecond,
		MaxHold:           3 * time.Second,
		IdleWindow:        time.Minute,
		IdleMin:           time.Hour,
		IdleMax:           time.Hour,
		HeldButtonTimeout: time.Minute,
	}
}

// newTestController returns an enabled controller with a pointer at the top
// left of the screen, outside the region, and a navigation trigger.
func newTestController(t *testing.T, cfg Config) (*Controller, *fakePointer, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pointer := &fakePointer{}
	navigate := make(chan struct{}, 1)
	watch := func(ctx context.Context, onNavigation func()) {
		for {
			select {
			case <-ctx.Done():
				return
			case <-navigate:
				onNavigation()
			}
		}
	}
	c := New(ctx, cfg, pointer, &sync.Mutex{}, watch, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetEnabled(true)
	return c, pointer, func() { navigate <- struct{}{} }
}

func awaitWithin(t *testing.T, c *Controller, d time.Duration) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	start := time.Now()
	c.Await(ctx)
	require.NoError(t, ctx.Err(), "Await did not release")
	return time.Since(start)
}

func TestAwaitReturnsAtOnceBeforeAnyNavigation(t *testing.T) {
	c, pointer, _ := newTestController(t, testConfig())
	assert.Less(t, awaitWithin(t, c, time.Second), 20*time.Millisecond)
	assert.Zero(t, pointer.count())
}

func TestNavigationOwesMovesInsideTheRegion(t *testing.T) {
	cfg := testConfig()
	c, pointer, navigate := newTestController(t, cfg)
	navigate()
	require.Eventually(t, func() bool { return pointer.count() > 0 }, time.Second, time.Millisecond)

	awaitWithin(t, c, 3*time.Second)

	reg := contentRegion(1920, 1080)
	x, y := 0, 0
	inside := 0
	for _, m := range pointer.moves {
		assert.False(t, m[0] == 0 && m[1] == 0, "a move must move the pointer")
		x, y = x+m[0], y+m[1]
		if reg.contains(x, y) {
			inside++
		}
	}
	assert.GreaterOrEqual(t, inside, cfg.MovesPerPage)
	assert.True(t, reg.contains(pointer.x, pointer.y), "pointer left at %d,%d", pointer.x, pointer.y)
}

func TestAwaitHoldsUntilTheMovesHaveSettled(t *testing.T) {
	cfg := testConfig()
	cfg.Settle = 300 * time.Millisecond
	c, pointer, navigate := newTestController(t, cfg)
	navigate()
	require.Eventually(t, func() bool { return pointer.count() > 0 }, time.Second, time.Millisecond)

	held := awaitWithin(t, c, 3*time.Second)
	assert.GreaterOrEqual(t, held, cfg.Settle-20*time.Millisecond)
	assert.Less(t, awaitWithin(t, c, time.Second), 20*time.Millisecond, "a settled page holds nothing")
}

func TestAgentPointerPausesMovementUntilACommandIsHeld(t *testing.T) {
	cfg := testConfig()
	cfg.AgentCooldown = time.Hour
	c, pointer, navigate := newTestController(t, cfg)
	c.AgentPointer(PointerMove)
	navigate()
	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, pointer.count(), "moved during the agent's cooldown")

	// The agent is waiting rather than using the pointer, so the owed moves
	// are made despite the cooldown.
	awaitWithin(t, c, 3*time.Second)
	assert.GreaterOrEqual(t, pointer.count(), cfg.MovesPerPage)
}

func TestHeldButtonBlocksMovementAndIsNotHeld(t *testing.T) {
	c, pointer, navigate := newTestController(t, testConfig())
	c.AgentPointer(PointerPress)
	navigate()
	assert.Less(t, awaitWithin(t, c, time.Second), 20*time.Millisecond, "a drag in progress must not be held")
	time.Sleep(250 * time.Millisecond)
	assert.Zero(t, pointer.count(), "moved while a button was held")

	c.AgentPointer(PointerRelease)
	require.Eventually(t, func() bool { return pointer.count() > 0 }, time.Second, time.Millisecond)
}

func TestAwaitIsCappedByMaxHold(t *testing.T) {
	cfg := testConfig()
	cfg.StartDelay = time.Hour
	cfg.MaxHold = 100 * time.Millisecond
	c, _, navigate := newTestController(t, cfg)
	navigate()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.owed > 0
	}, time.Second, time.Millisecond)
	held := awaitWithin(t, c, time.Second)
	assert.GreaterOrEqual(t, held, cfg.MaxHold)
	assert.Less(t, held, cfg.MaxHold+200*time.Millisecond)
}

func TestDisablingReleasesHeldCommands(t *testing.T) {
	cfg := testConfig()
	cfg.StartDelay = time.Hour
	c, pointer, navigate := newTestController(t, cfg)
	navigate()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.owed > 0
	}, time.Second, time.Millisecond)

	go func() {
		time.Sleep(50 * time.Millisecond)
		c.SetEnabled(false)
	}()
	assert.Less(t, awaitWithin(t, c, time.Second), cfg.MaxHold)
	assert.Zero(t, pointer.count())
}

func TestForeignPointerMovementPausesWander(t *testing.T) {
	cfg := testConfig()
	cfg.MovesPerPage = 10_000
	cfg.AgentCooldown = time.Hour
	c, pointer, navigate := newTestController(t, cfg)
	navigate()
	require.Eventually(t, func() bool { return pointer.count() > 0 }, time.Second, time.Millisecond)

	pointer.jump(900, 700)
	time.Sleep(300 * time.Millisecond)
	moved := pointer.count()
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, moved, pointer.count(), "kept moving after someone else took the pointer")
	c.mu.Lock()
	defer c.mu.Unlock()
	assert.True(t, c.agentUntil.After(time.Now()))
}

func TestDisabledControllerIgnoresNavigation(t *testing.T) {
	c, pointer, _ := newTestController(t, testConfig())
	c.SetEnabled(false)
	c.NoteNavigation()
	assert.Less(t, awaitWithin(t, c, time.Second), 20*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, pointer.count())
}

func targetEvent(t *testing.T, method, id, typ, url string) cdpclient.Message {
	t.Helper()
	params, err := json.Marshal(map[string]any{
		"targetId":   id,
		"targetInfo": map[string]any{"targetId": id, "type": typ, "url": url},
	})
	require.NoError(t, err)
	return cdpclient.Message{Method: method, Params: params}
}

func TestPageURLsReportsTopLevelNavigations(t *testing.T) {
	p := pageURLs{}
	assert.False(t, p.navigated(targetEvent(t, "Target.targetCreated", "A", "page", "about:blank")))
	assert.True(t, p.navigated(targetEvent(t, "Target.targetInfoChanged", "A", "page", "https://example.com/")))
	assert.False(t, p.navigated(targetEvent(t, "Target.targetInfoChanged", "A", "page", "https://example.com/")), "title-only change")
	assert.False(t, p.navigated(targetEvent(t, "Target.targetInfoChanged", "A", "page", "https://example.com/#top")), "fragment-only change")
	assert.True(t, p.navigated(targetEvent(t, "Target.targetInfoChanged", "A", "page", "https://example.com/s?k=a")))
	assert.False(t, p.navigated(targetEvent(t, "Target.targetInfoChanged", "A", "page", "chrome://newtab/")))
	assert.False(t, p.navigated(targetEvent(t, "Target.targetInfoChanged", "W", "service_worker", "https://example.com/sw.js")))
	assert.False(t, p.navigated(targetEvent(t, "Target.targetInfoChanged", "F", "iframe", "https://ads.example.com/")))

	p.navigated(targetEvent(t, "Target.targetDestroyed", "A", "", ""))
	assert.NotContains(t, p, "A")
}
