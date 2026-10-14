// Package ambientmouse moves the real X pointer in small, human-shaped wanders
// while an agent drives the browser, so pages that score visitors on pointer
// activity see some.
//
// The moves are OS input. The page receives them as trusted mousemove events,
// but no CDP client sees a command or an event for them, and nothing is
// published as telemetry because they never pass through the input API.
//
// Every top-level navigation owes the new page a number of moves. Until they
// are made and the page has had time to score them, Await holds the agent's
// next input or navigation, so an agent that moves on quickly cannot leave a
// page before it has seen any pointer activity. While the agent is using the
// pointer itself, ambient movement stays out of its way.
package ambientmouse

import (
	"context"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kernel/kernel-images/server/lib/mousetrajectory"
)

// Pointer drives the X pointer.
type Pointer interface {
	Location(ctx context.Context) (x, y int, err error)
	MoveRelative(ctx context.Context, dx, dy int) error
	ScreenSize(ctx context.Context) (width, height int, err error)
}

// NavigationWatcher calls onNavigation for every top-level navigation until
// ctx ends.
type NavigationWatcher func(ctx context.Context, onNavigation func())

// PointerEvent is what the agent did with the pointer.
type PointerEvent int

const (
	PointerMove PointerEvent = iota
	PointerPress
	PointerRelease
)

type Config struct {
	// MovesPerPage is how many moves each navigation owes the new page.
	MovesPerPage int
	// StartDelay gives the new document time to install its listeners before
	// the owed moves start.
	StartDelay time.Duration
	// Settle is how long after the last owed move the page is held, so sensors
	// that batch on roughly one-second intervals flush and rescore before the
	// agent navigates away.
	Settle time.Duration
	// AgentCooldown keeps ambient movement paused after the agent's own
	// pointer input.
	AgentCooldown time.Duration
	// MaxHold caps how long Await holds one agent command.
	MaxHold time.Duration
	// IdleWindow is how long after the last navigation or agent pointer input
	// occasional idle wanders continue.
	IdleWindow time.Duration
	IdleMin    time.Duration
	IdleMax    time.Duration
	// HeldButtonTimeout releases a press whose release was never seen.
	HeldButtonTimeout time.Duration
}

func DefaultConfig() Config {
	return Config{
		MovesPerPage:      40,
		StartDelay:        300 * time.Millisecond,
		Settle:            2 * time.Second,
		AgentCooldown:     1500 * time.Millisecond,
		MaxHold:           5 * time.Second,
		IdleWindow:        time.Minute,
		IdleMin:           4 * time.Second,
		IdleMax:           12 * time.Second,
		HeldButtonTimeout: 30 * time.Second,
	}
}

type Controller struct {
	ctx     context.Context
	cfg     Config
	pointer Pointer
	// input is held for each move, so a move never lands in the middle of
	// another input action that holds it.
	input sync.Locker
	watch NavigationWatcher
	log   *slog.Logger

	enabled atomic.Bool
	wake    chan struct{}

	mu        sync.Mutex
	stopWatch context.CancelFunc
	// gen changes on every navigation, so moves made for one page are never
	// counted toward the next.
	gen   uint64
	owed  int
	navAt time.Time
	// settled is closed while the current page owes nothing.
	settled chan struct{}
	// settleAt is when settled closes after the last owed move.
	settleAt    time.Time
	agentUntil  time.Time
	pressedAt   time.Time
	waiters     int
	activeUntil time.Time
	nextIdle    time.Time
}

// New returns a disabled controller whose loop runs until ctx ends.
func New(ctx context.Context, cfg Config, pointer Pointer, input sync.Locker, watch NavigationWatcher, log *slog.Logger) *Controller {
	settled := make(chan struct{})
	close(settled)
	c := &Controller{
		ctx:     ctx,
		cfg:     cfg,
		pointer: pointer,
		input:   input,
		watch:   watch,
		log:     log,
		wake:    make(chan struct{}, 1),
		settled: settled,
	}
	go c.run()
	return c
}

func (c *Controller) Enabled() bool {
	return c != nil && c.enabled.Load()
}

// SetEnabled starts or stops ambient movement and the navigation watcher.
// Disabling releases any held agent command.
func (c *Controller) SetEnabled(on bool) {
	c.mu.Lock()
	if c.enabled.Load() == on {
		c.mu.Unlock()
		return
	}
	c.enabled.Store(on)
	if on {
		watchCtx, cancel := context.WithCancel(c.ctx)
		c.stopWatch = cancel
		go c.watch(watchCtx, c.NoteNavigation)
	} else {
		c.stopWatch()
		c.stopWatch = nil
		c.owed = 0
		c.settleAt = time.Time{}
		c.pressedAt = time.Time{}
		c.activeUntil = time.Time{}
		c.closeSettledLocked()
	}
	c.mu.Unlock()
	c.poke()
}

// NoteNavigation records a top-level navigation: the new page is owed moves,
// and agent commands are held until it has them.
func (c *Controller) NoteNavigation() {
	now := time.Now()
	c.mu.Lock()
	if !c.enabled.Load() {
		c.mu.Unlock()
		return
	}
	c.gen++
	c.owed = c.cfg.MovesPerPage
	c.navAt = now
	c.settleAt = time.Time{}
	select {
	case <-c.settled:
		c.settled = make(chan struct{})
	default:
	}
	c.activeUntil = now.Add(c.cfg.IdleWindow)
	c.nextIdle = time.Time{}
	c.mu.Unlock()
	c.poke()
}

// AgentPointer records the agent's own pointer input, which pauses ambient
// movement for the cooldown, or for as long as a button is held.
func (c *Controller) AgentPointer(ev PointerEvent) {
	if !c.Enabled() {
		return
	}
	now := time.Now()
	c.mu.Lock()
	c.agentUntil = now.Add(c.cfg.AgentCooldown)
	switch ev {
	case PointerPress:
		c.pressedAt = now
	case PointerRelease:
		c.pressedAt = time.Time{}
	}
	c.activeUntil = now.Add(c.cfg.IdleWindow)
	c.mu.Unlock()
	c.poke()
}

// Await holds an agent command until the current page has been given its owed
// moves and had time to score them, for at most MaxHold. A held command
// overrides the agent cooldown, since the agent is not using the pointer while
// it waits. It returns at once while a button is held, because no ambient move
// can be made then.
func (c *Controller) Await(ctx context.Context) {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	settled := c.settled
	select {
	case <-settled:
		c.mu.Unlock()
		return
	default:
	}
	if !c.pressedAt.IsZero() {
		c.mu.Unlock()
		return
	}
	c.waiters++
	c.mu.Unlock()
	c.poke()

	timer := time.NewTimer(c.cfg.MaxHold)
	defer timer.Stop()
	select {
	case <-settled:
	case <-timer.C:
	case <-ctx.Done():
	}
	c.mu.Lock()
	c.waiters--
	c.mu.Unlock()
}

func (c *Controller) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Controller) closeSettledLocked() {
	select {
	case <-c.settled:
	default:
		close(c.settled)
	}
}

// mayMoveLocked reports whether ambient movement may touch the pointer now.
func (c *Controller) mayMoveLocked(now time.Time) bool {
	if !c.pressedAt.IsZero() {
		if now.Sub(c.pressedAt) < c.cfg.HeldButtonTimeout {
			return false
		}
		c.pressedAt = time.Time{}
	}
	return c.waiters > 0 || !now.Before(c.agentUntil)
}

// pausedForLocked is how long until mayMoveLocked could next change its answer.
func (c *Controller) pausedForLocked(now time.Time) time.Duration {
	if !c.pressedAt.IsZero() {
		return c.pressedAt.Add(c.cfg.HeldButtonTimeout).Sub(now)
	}
	return c.agentUntil.Sub(now)
}

type action int

const (
	actWait action = iota
	actOwed
	actIdle
)

// next decides what the loop does now, or how long it waits before deciding
// again.
func (c *Controller) next(now time.Time, rng *rand.Rand) (action, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	wait := time.Hour
	if !c.enabled.Load() {
		return actWait, wait
	}
	if c.owed == 0 && !c.settleAt.IsZero() {
		if !now.Before(c.settleAt) {
			c.settleAt = time.Time{}
			c.closeSettledLocked()
		} else {
			wait = c.settleAt.Sub(now)
		}
	}
	if c.owed > 0 {
		if start := c.navAt.Add(c.cfg.StartDelay); now.Before(start) {
			return actWait, min(wait, start.Sub(now))
		}
		if c.mayMoveLocked(now) {
			return actOwed, 0
		}
		return actWait, min(wait, c.pausedForLocked(now))
	}
	if now.Before(c.activeUntil) {
		if c.nextIdle.IsZero() {
			c.nextIdle = now.Add(c.cfg.IdleMin + time.Duration(rng.Int63n(int64(c.cfg.IdleMax-c.cfg.IdleMin)+1)))
		}
		if now.Before(c.nextIdle) {
			return actWait, min(wait, c.nextIdle.Sub(now))
		}
		c.nextIdle = time.Time{}
		if c.mayMoveLocked(now) {
			return actIdle, 0
		}
	}
	return actWait, wait
}

func (c *Controller) run() {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		act, wait := c.next(time.Now(), rng)
		switch act {
		case actOwed:
			c.wander(rng, true)
		case actIdle:
			c.wander(rng, false)
		default:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(max(wait, time.Millisecond))
			select {
			case <-c.ctx.Done():
				return
			case <-c.wake:
			case <-timer.C:
			}
		}
		if c.ctx.Err() != nil {
			return
		}
	}
}

// region is the part of the screen ambient moves stay in. It sits below the
// browser's tab strip and toolbar and the band where site navigation menus
// usually open on hover, and away from the edges.
type region struct{ minX, minY, maxX, maxY int }

func contentRegion(width, height int) region {
	return region{
		minX: width * 15 / 100,
		minY: max(height*30/100, 200),
		maxX: width * 85 / 100,
		maxY: height * 85 / 100,
	}
}

func (r region) contains(x, y int) bool {
	return x >= r.minX && x <= r.maxX && y >= r.minY && y <= r.maxY
}

func (r region) clamp(x, y int) (int, int) {
	return min(max(x, r.minX), r.maxX), min(max(y, r.minY), r.maxY)
}

func (r region) random(rng *rand.Rand) (int, int) {
	return r.minX + rng.Intn(r.maxX-r.minX+1), r.minY + rng.Intn(r.maxY-r.minY+1)
}

// wander moves the pointer along short Bezier segments around a home point.
// For a page that is owed moves it runs until the page has them; otherwise it
// makes a short idle wander. Only moves that land inside the region count,
// since the page does not see the rest. It stops early whenever the agent
// takes the pointer, a navigation starts a new page, or the feature is
// disabled.
func (c *Controller) wander(rng *rand.Rand, owed bool) {
	ctx := c.ctx
	c.mu.Lock()
	gen := c.gen
	c.mu.Unlock()

	width, height, err := c.pointer.ScreenSize(ctx)
	if err != nil {
		c.abandon(gen, "screen size", err)
		return
	}
	reg := contentRegion(width, height)
	if reg.maxX <= reg.minX || reg.maxY <= reg.minY {
		c.abandon(gen, "screen too small", nil)
		return
	}
	x, y, err := c.pointer.Location(ctx)
	if err != nil {
		c.abandon(gen, "pointer location", err)
		return
	}
	homeX, homeY := x, y
	// A pointer outside the region first glides to a home point inside it,
	// along a path that is not clamped, since it has to cross the edge.
	gliding := !reg.contains(x, y)
	if gliding {
		homeX, homeY = reg.random(rng)
	}
	idleLeft := 8 + rng.Intn(13)

	for {
		toX, toY := homeX, homeY
		points := 0
		if !gliding {
			angle := rng.Float64() * 2 * math.Pi
			radius := 30 + rng.Float64()*60
			toX, toY = reg.clamp(homeX+int(radius*math.Cos(angle)), homeY+int(radius*math.Sin(angle)))
			points = 8 + rng.Intn(12)
		}
		path := mousetrajectory.NewHumanizeMouseTrajectoryWithOptions(
			float64(x), float64(y), float64(toX), float64(toY), &mousetrajectory.Options{MaxPoints: points},
		).GetPointsInt()
		for i := 1; i < len(path); i++ {
			px, py := path[i][0], path[i][1]
			if !gliding {
				px, py = reg.clamp(px, py)
			}
			if px == x && py == y {
				continue
			}
			counts := reg.contains(px, py)
			more, err := c.step(gen, owed, counts, x, y, px, py)
			if err != nil {
				c.abandon(gen, "pointer", err)
				return
			}
			if !more {
				return
			}
			x, y = px, py
			if !owed && counts {
				if idleLeft--; idleLeft == 0 {
					return
				}
			}
			if !sleep(ctx, gaussian(rng, 12*time.Millisecond, 4*time.Millisecond)) {
				return
			}
		}
		gliding = false
		// Short enough that a page tracking continuous movement does not see
		// the run end between segments.
		if !sleep(ctx, time.Duration(20+rng.Intn(100))*time.Millisecond) {
			return
		}
	}
}

// step moves the pointer from x,y to toX,toY if ambient movement still has
// the pointer, and reports whether the wander should go on.
func (c *Controller) step(gen uint64, owed, counts bool, x, y, toX, toY int) (bool, error) {
	c.input.Lock()
	defer c.input.Unlock()

	c.mu.Lock()
	ok := c.enabled.Load() && c.gen == gen && c.mayMoveLocked(time.Now()) && (!owed || c.owed > 0)
	c.mu.Unlock()
	if !ok {
		return false, nil
	}
	nowX, nowY, err := c.pointer.Location(c.ctx)
	if err != nil {
		return false, err
	}
	// Someone else moved the pointer, such as a person in the live view, so
	// treat it like the agent's own input and get out of the way.
	if nowX != x || nowY != y {
		c.AgentPointer(PointerMove)
		return false, nil
	}
	if err := c.pointer.MoveRelative(c.ctx, toX-x, toY-y); err != nil {
		return false, err
	}
	if !owed || !counts {
		return true, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return false, nil
	}
	c.owed--
	if c.owed > 0 {
		return true, nil
	}
	c.settleAt = time.Now().Add(c.cfg.Settle)
	return false, nil
}

// abandon gives up on the current page's owed moves after a pointer failure,
// so held agent commands are released instead of waiting out MaxHold.
func (c *Controller) abandon(gen uint64, what string, err error) {
	if c.ctx.Err() != nil {
		return
	}
	c.log.Warn("ambientmouse: giving up on page", "step", what, "err", err)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen == gen && c.owed > 0 {
		c.owed = 0
		c.settleAt = time.Time{}
		c.closeSettledLocked()
	}
}

func gaussian(rng *rand.Rand, mean, stddev time.Duration) time.Duration {
	d := mean + time.Duration(rng.NormFloat64()*float64(stddev))
	return min(max(d, mean/3), mean*3)
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
