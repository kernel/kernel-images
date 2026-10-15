// Package livefocus tells the live view which editable element has focus in
// the browser, so a touch client can raise its soft keyboard for real text
// fields only, with the right keyboard, and close it when the field loses
// focus.
//
// It runs only while a live view is subscribed. Each frame gets watcher.js in
// an isolated world created with Page.createIsolatedWorld, and the server
// long-polls it with Runtime.callFunctionOn. No Runtime, Page or DOM domain is
// enabled, nothing is added to the page's main world, and a watcher that stops
// being polled removes its own listeners.
package livefocus

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/kernel/kernel-images/server/lib/browsersurface"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
)

//go:embed watcher.js
var watcherSource string

const (
	worldName = "kernel-live-view"
	// how long the browser connection stays up after the last subscriber
	// leaves, so a live view that reconnects finds it running
	lingerDelay    = 30 * time.Second
	commandTimeout = 5 * time.Second
	// After this many failures in a row, a frame is retried more slowly, and
	// dropped once it is no longer in its session's frame tree.
	injectAttempts = 3
	retryDelay     = 200 * time.Millisecond
	minBackoff     = 250 * time.Millisecond
	maxBackoff     = 5 * time.Second
)

// UpstreamManager reports the browser's current DevTools URL.
type UpstreamManager interface {
	Current() string
}

// Watcher watches the browser while anyone is subscribed.
type Watcher struct {
	upstream UpstreamManager
	log      *slog.Logger
	linger   time.Duration

	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	latest []byte
	// the current run, which only it publishes to
	runCtx      context.Context
	cancel      context.CancelFunc
	lingerTimer *time.Timer
	closed      chan struct{}
	// the current browser connection, nil while there is none
	conn *connection
}

func New(upstream UpstreamManager, log *slog.Logger) *Watcher {
	return &Watcher{
		upstream: upstream,
		log:      log,
		linger:   lingerDelay,
		subs:     make(map[chan []byte]struct{}),
		latest:   mustMarshal(emptyState()),
		closed:   make(chan struct{}),
	}
}

// Subscribe returns the current state as JSON, then each change. A subscriber
// that falls behind gets only the newest state.
func (w *Watcher) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 1)
	w.mu.Lock()
	ch <- w.latest
	w.subs[ch] = struct{}{}
	if w.lingerTimer != nil {
		w.lingerTimer.Stop()
		w.lingerTimer = nil
	}
	if w.cancel == nil && !w.isClosed() {
		w.runCtx, w.cancel = context.WithCancel(context.Background())
		go w.run(w.runCtx)
	}
	w.mu.Unlock()
	var once sync.Once
	return ch, func() { once.Do(func() { w.unsubscribe(ch) }) }
}

func (w *Watcher) unsubscribe(ch chan []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.subs, ch)
	if len(w.subs) > 0 || w.cancel == nil {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(w.linger, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.lingerTimer != timer {
			return
		}
		w.lingerTimer = nil
		w.stopLocked()
	})
	w.lingerTimer = timer
}

// Close stops watching and ends every stream, for server shutdown.
func (w *Watcher) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.isClosed() {
		close(w.closed)
	}
	if w.lingerTimer != nil {
		w.lingerTimer.Stop()
		w.lingerTimer = nil
	}
	w.stopLocked()
}

func (w *Watcher) isClosed() bool {
	select {
	case <-w.closed:
		return true
	default:
		return false
	}
}

func (w *Watcher) stopLocked() {
	if w.cancel == nil {
		return
	}
	w.cancel()
	w.runCtx, w.cancel = nil, nil
	w.latest = mustMarshal(emptyState())
}

// publish is a no-op once ctx, the run that produced state, has been stopped.
func (w *Watcher) publish(ctx context.Context, state State) {
	data := mustMarshal(state)
	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx.Err() != nil || bytes.Equal(data, w.latest) {
		return
	}
	w.latest = data
	for ch := range w.subs {
		select {
		case <-ch:
		default:
		}
		ch <- data
	}
}

func (w *Watcher) run(ctx context.Context) {
	backoff := minBackoff
	for ctx.Err() == nil {
		if url := w.upstream.Current(); url != "" {
			started := time.Now()
			err := w.watch(ctx, url)
			w.publish(ctx, emptyState())
			if ctx.Err() != nil {
				return
			}
			w.log.Info("livefocus: browser connection ended", "err", err)
			if time.Since(started) > maxBackoff {
				backoff = minBackoff
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (w *Watcher) watch(ctx context.Context, url string) error {
	dialCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	protocol, err := cdpclient.DialWithEvents(dialCtx, url)
	cancel()
	if err != nil {
		return err
	}
	defer protocol.Close()
	c := newConnection(ctx, protocol, func(state State) { w.publish(ctx, state) })
	w.mu.Lock()
	w.conn = c
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.conn == c {
			w.conn = nil
		}
		w.mu.Unlock()
	}()
	return c.run()
}

// Blur blurs the browser's focused text field, the way Safari's Done button
// does when the live view's keyboard is dismissed. seq is the focused field's
// Seq the live view last saw; the blur does nothing once the page was tapped or
// focus moved since, or when no field has focus.
func (w *Watcher) Blur(ctx context.Context, seq int64) error {
	w.mu.Lock()
	c := w.conn
	w.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.blur(ctx, seq)
}

type frameKey struct {
	session string
	frame   string
}

type watchedSession struct {
	targetID string
	// a page target: its root frame is a tab's top frame, and same-process
	// child frames report rects relative to it
	page     bool
	ctx      context.Context
	cancel   context.CancelFunc
	scanning bool
	rescan   bool
}

type watchedFrame struct {
	state frameState
	top   bool
	// the watcher in the frame's current document, empty between documents
	objectID string
}

// connection watches every page and out-of-process iframe on one DevTools
// connection.
type connection struct {
	ctx      context.Context
	protocol *cdpclient.Client
	surface  *browsersurface.Tracker
	publish  func(State)

	// keeps states published in the order they were taken
	emitMu   sync.Mutex
	mu       sync.Mutex
	sessions map[string]*watchedSession
	frames   map[frameKey]*watchedFrame
	bounds   map[string]windowBounds
	seq      int64
}

func newConnection(ctx context.Context, protocol *cdpclient.Client, publish func(State)) *connection {
	return &connection{
		ctx:      ctx,
		protocol: protocol,
		surface:  browsersurface.New(protocol, browsersurface.WithoutLocations()),
		publish:  publish,
		sessions: make(map[string]*watchedSession),
		frames:   make(map[frameKey]*watchedFrame),
		bounds:   make(map[string]windowBounds),
	}
}

func (c *connection) run() error {
	events, unsubscribe := c.surface.Subscribe()
	defer unsubscribe()
	defer c.removeAll()
	if err := c.surface.Start(c.ctx); err != nil {
		return err
	}
	for {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case event, ok := <-events:
			if !ok {
				return errors.New("devtools connection closed")
			}
			switch event.Kind {
			case browsersurface.EventSessionAttached:
				if event.Target.Type == "page" || event.Target.Type == "iframe" {
					c.addSession(event.SessionID, event.Target)
				}
			case browsersurface.EventSessionRemoved:
				c.removeSession(event.SessionID)
			}
		}
	}
}

func (c *connection) addSession(sessionID string, target browsersurface.SessionTarget) {
	ctx, cancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	if _, ok := c.sessions[sessionID]; ok {
		c.mu.Unlock()
		cancel()
		return
	}
	c.sessions[sessionID] = &watchedSession{targetID: target.ID, page: target.Type == "page", ctx: ctx, cancel: cancel}
	c.mu.Unlock()
	c.scan(sessionID)
}

func (c *connection) removeSession(sessionID string) {
	c.mu.Lock()
	session, ok := c.sessions[sessionID]
	if ok {
		session.cancel()
		delete(c.sessions, sessionID)
		for key := range c.frames {
			if key.session == sessionID {
				delete(c.frames, key)
			}
		}
		if session.page {
			delete(c.bounds, session.targetID)
		}
	}
	c.mu.Unlock()
	if ok {
		c.emit()
	}
}

func (c *connection) removeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, session := range c.sessions {
		session.cancel()
	}
}

// scan watches every frame in the session's frame tree that is not watched
// yet. Scans of one session run one at a time, and a scan asked for while one
// runs is done once it finishes.
func (c *connection) scan(sessionID string) {
	c.mu.Lock()
	session, ok := c.sessions[sessionID]
	if !ok {
		c.mu.Unlock()
		return
	}
	if session.scanning {
		session.rescan = true
		c.mu.Unlock()
		return
	}
	session.scanning = true
	c.mu.Unlock()

	go func() {
		for {
			c.scanOnce(sessionID, session)
			c.mu.Lock()
			if !session.rescan || session.ctx.Err() != nil {
				session.scanning = false
				c.mu.Unlock()
				return
			}
			session.rescan = false
			c.mu.Unlock()
		}
	}()
}

func (c *connection) scanOnce(sessionID string, session *watchedSession) {
	ctx, cancel := context.WithTimeout(session.ctx, commandTimeout)
	raw, err := c.protocol.Send(ctx, "Page.getFrameTree", nil, sessionID)
	cancel()
	if err != nil {
		return
	}
	var result struct {
		FrameTree frameTree `json:"frameTree"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return
	}
	var walk func(tree frameTree, root bool)
	walk = func(tree frameTree, root bool) {
		c.watchFrame(sessionID, session, tree.Frame.ID, root && session.page)
		for _, child := range tree.ChildFrames {
			walk(child, false)
		}
	}
	walk(result.FrameTree, true)
}

// frameExists reports whether the frame is still in its session's frame tree
// and has no session of its own, and true when that cannot be told.
func (c *connection) frameExists(ctx context.Context, key frameKey) bool {
	if c.ownSession(key) {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	raw, err := c.protocol.Send(ctx, "Page.getFrameTree", nil, key.session)
	var result struct {
		FrameTree frameTree `json:"frameTree"`
	}
	if err != nil || json.Unmarshal(raw, &result) != nil {
		return true
	}
	return result.FrameTree.contains(key.frame)
}

type frameTree struct {
	Frame struct {
		ID string `json:"id"`
	} `json:"frame"`
	ChildFrames []frameTree `json:"childFrames"`
}

func (t frameTree) contains(frameID string) bool {
	if t.Frame.ID == frameID {
		return true
	}
	for _, child := range t.ChildFrames {
		if child.contains(frameID) {
			return true
		}
	}
	return false
}

// ownSession reports whether the frame is an out-of-process iframe with its
// own session, through which it is watched instead. An iframe target's ID is
// its frame's ID.
func (c *connection) ownSession(key frameKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, session := range c.sessions {
		if id != key.session && !session.page && session.targetID == key.frame {
			return true
		}
	}
	return false
}

func (c *connection) watchFrame(sessionID string, session *watchedSession, frameID string, top bool) {
	key := frameKey{session: sessionID, frame: frameID}
	if c.ownSession(key) {
		return
	}
	tab := ""
	if session.page {
		tab = session.targetID
	}
	c.mu.Lock()
	if _, ok := c.frames[key]; ok || session.ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	frame := &watchedFrame{state: frameState{tab: tab}, top: top}
	c.frames[key] = frame
	c.mu.Unlock()
	go c.pollFrame(session.ctx, key, frame)
}

// pollFrame keeps a watcher in the frame's current document and reports
// what it sees. A poll fails when the document goes away, and the next
// document gets a new watcher. A frame that keeps failing, as one between
// redirects or showing an error page can, is retried for as long as it exists.
func (c *connection) pollFrame(ctx context.Context, key frameKey, frame *watchedFrame) {
	defer func() {
		c.mu.Lock()
		if c.frames[key] == frame {
			delete(c.frames, key)
		}
		c.mu.Unlock()
		c.emit()
	}()
	failures := 0
	backoff := retryDelay
	for ctx.Err() == nil {
		if failures >= injectAttempts {
			if !c.frameExists(ctx, key) {
				return
			}
			sleep(ctx, backoff)
			backoff = min(backoff*2, maxBackoff)
			failures = 0
		}
		objectID, err := c.inject(ctx, key)
		if err != nil {
			failures++
			sleep(ctx, retryDelay)
			continue
		}
		c.setWatcher(key, frame, objectID)
		// counts as a failure until the watcher reports
		failures++
		frames := -1
		var seen int64
		for {
			report, err := c.poll(ctx, key.session, objectID, seen)
			if err != nil || report.Stopped {
				break
			}
			failures = 0
			backoff = retryDelay
			seen = report.V
			c.update(key, frame, report)
			// a new document, or frames added or removed in this one
			if report.Frames != frames {
				frames = report.Frames
				c.scan(key.session)
			}
		}
		c.setWatcher(key, frame, "")
		c.update(key, frame, nil)
	}
}

func (c *connection) setWatcher(key frameKey, frame *watchedFrame, objectID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frames[key] == frame {
		frame.objectID = objectID
	}
}

// blur asks the watcher of the frame holding the focused field to blur it.
func (c *connection) blur(ctx context.Context, seq int64) error {
	var target *watchedFrame
	var key frameKey
	c.mu.Lock()
	for k, frame := range c.frames {
		r := frame.state.report
		if r == nil || r.Focus == nil || !r.Focused || frame.objectID == "" {
			continue
		}
		if target == nil || frame.state.seq > target.state.seq {
			target, key = frame, k
		}
	}
	var objectID string
	if target != nil {
		objectID = target.objectID
	}
	c.mu.Unlock()
	if target == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	_, err := c.protocol.Send(ctx, "Runtime.callFunctionOn", map[string]any{
		"objectId":            objectID,
		"functionDeclaration": "function(seq) { return this.blur(seq) }",
		"arguments":           []map[string]any{{"value": seq}},
		"returnByValue":       true,
	}, key.session)
	return err
}

func (c *connection) inject(ctx context.Context, key frameKey) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	raw, err := c.protocol.Send(cmdCtx, "Page.createIsolatedWorld", map[string]any{
		"frameId":   key.frame,
		"worldName": worldName,
	}, key.session)
	if err != nil {
		return "", err
	}
	var world struct {
		ExecutionContextID int `json:"executionContextId"`
	}
	if err := json.Unmarshal(raw, &world); err != nil || world.ExecutionContextID == 0 {
		return "", fmt.Errorf("livefocus: no isolated world for frame %s", key.frame)
	}
	raw, err = c.protocol.Send(cmdCtx, "Runtime.evaluate", map[string]any{
		"expression": watcherSource,
		"contextId":  world.ExecutionContextID,
	}, key.session)
	if err != nil {
		return "", err
	}
	var result struct {
		Result struct {
			ObjectID string `json:"objectId"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.ExceptionDetails != nil || result.Result.ObjectID == "" {
		return "", fmt.Errorf("livefocus: watcher did not start in frame %s", key.frame)
	}
	return result.Result.ObjectID, nil
}

// poll waits for the watcher's next report. The watcher answers within its
// heartbeat even when nothing changes, so a poll ends on its own.
func (c *connection) poll(ctx context.Context, sessionID, objectID string, seen int64) (*frameReport, error) {
	raw, err := c.protocol.Send(ctx, "Runtime.callFunctionOn", map[string]any{
		"objectId":            objectID,
		"functionDeclaration": "function(seen) { return this.next(seen) }",
		"arguments":           []map[string]any{{"value": seen}},
		"awaitPromise":        true,
		"returnByValue":       true,
	}, sessionID)
	if err != nil {
		return nil, err
	}
	var result struct {
		Result struct {
			Value *frameReport `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.ExceptionDetails != nil || result.Result.Value == nil {
		return nil, errors.New("livefocus: watcher poll failed")
	}
	return result.Result.Value, nil
}

// update records a frame's report, nil once its document is gone. A top
// frame keeps its place on screen until its next document reports, so the
// live view keeps knowing where the page is during a navigation.
func (c *connection) update(key frameKey, frame *watchedFrame, report *frameReport) {
	if report != nil && frame.top && report.Viewport != nil {
		c.refreshBounds(key, frame, report.Viewport)
	}
	c.mu.Lock()
	if c.frames[key] != frame {
		c.mu.Unlock()
		return
	}
	if previous := frame.state.report; report == nil && frame.top && previous != nil {
		report = &frameReport{Top: true, Visible: previous.Visible, Viewport: previous.Viewport}
	}
	c.seq++
	frame.state.report = report
	frame.state.seq = c.seq
	c.mu.Unlock()
	c.emit()
}

// refreshBounds reads the tab's window position when the page's size, zoom or
// position on screen changed since its last report.
func (c *connection) refreshBounds(key frameKey, frame *watchedFrame, vp *viewport) {
	c.mu.Lock()
	previous := frame.state.report
	_, known := c.bounds[frame.state.tab]
	c.mu.Unlock()
	if known && previous != nil && previous.Viewport != nil && *previous.Viewport == *vp {
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, commandTimeout)
	defer cancel()
	raw, err := c.protocol.Send(ctx, "Browser.getWindowForTarget", map[string]any{"targetId": frame.state.tab}, "")
	if err != nil {
		return
	}
	var result struct {
		Bounds windowBounds `json:"bounds"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return
	}
	c.mu.Lock()
	if c.sessions[key.session] != nil {
		c.bounds[frame.state.tab] = result.Bounds
	}
	c.mu.Unlock()
}

func (c *connection) emit() {
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	c.mu.Lock()
	frames := make([]frameState, 0, len(c.frames))
	for _, frame := range c.frames {
		frames = append(frames, frame.state)
	}
	bounds := make(map[string]windowBounds, len(c.bounds))
	for tab, b := range c.bounds {
		bounds[tab] = b
	}
	c.mu.Unlock()
	c.publish(merge(frames, bounds))
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func mustMarshal(state State) []byte {
	data, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	return data
}
