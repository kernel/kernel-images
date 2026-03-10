package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

//go:embed viewer.html
var viewerFS embed.FS

type cdpMessage struct {
	ID     int             `json:"id"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string {
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

type cdpResult struct {
	Data json.RawMessage
	Err  error
}

var errCDPDisconnected = fmt.Errorf("CDP connection closed")

type cdpClient struct {
	ws     *websocket.Conn
	nextID atomic.Int64
	mu     sync.Mutex

	pending   map[int]chan cdpResult
	pendingMu sync.Mutex

	handlers   map[string]func(json.RawMessage)
	handlersMu sync.RWMutex

	onDisconnect func()
	log          *slog.Logger
}

func newCDPClient(ctx context.Context, url string, log *slog.Logger) (*cdpClient, error) {
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("dial CDP: %w", err)
	}
	ws.SetReadLimit(64 * 1024 * 1024)

	c := &cdpClient{
		ws:       ws,
		pending:  make(map[int]chan cdpResult),
		handlers: make(map[string]func(json.RawMessage)),
		log:      log,
	}
	c.nextID.Store(1)
	go c.readLoop(ctx)
	return c, nil
}

func (c *cdpClient) readLoop(ctx context.Context) {
	defer func() {
		// Drain all pending calls so blocked goroutines don't leak
		c.pendingMu.Lock()
		for id, ch := range c.pending {
			ch <- cdpResult{Err: errCDPDisconnected}
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()

		if c.onDisconnect != nil {
			c.onDisconnect()
		}
	}()
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			c.log.Error("CDP read error", "error", err)
			return
		}
		var msg cdpMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		if msg.Method != "" {
			c.handlersMu.RLock()
			h, ok := c.handlers[msg.Method]
			c.handlersMu.RUnlock()
			if ok {
				go h(msg.Params)
			}
		}

		if msg.ID > 0 {
			c.pendingMu.Lock()
			ch, ok := c.pending[msg.ID]
			if ok {
				delete(c.pending, msg.ID)
			}
			c.pendingMu.Unlock()
			if ok {
				if msg.Error != nil {
					ch <- cdpResult{Err: msg.Error}
				} else {
					ch <- cdpResult{Data: msg.Result}
				}
			}
		}
	}
}

func (c *cdpClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := int(c.nextID.Add(1))
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		rawParams = b
	}

	msg, _ := json.Marshal(cdpMessage{ID: id, Method: method, Params: rawParams})

	ch := make(chan cdpResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	c.mu.Lock()
	err := c.ws.Write(ctx, websocket.MessageText, msg)
	c.mu.Unlock()
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	select {
	case res := <-ch:
		return res.Data, res.Err
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *cdpClient) callSession(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	id := int(c.nextID.Add(1))
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		rawParams = b
	}

	type sessionMsg struct {
		ID        int             `json:"id"`
		Method    string          `json:"method"`
		Params    json.RawMessage `json:"params,omitempty"`
		SessionID string          `json:"sessionId"`
	}
	msg, _ := json.Marshal(sessionMsg{ID: id, Method: method, Params: rawParams, SessionID: sessionID})

	ch := make(chan cdpResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	c.mu.Lock()
	err := c.ws.Write(ctx, websocket.MessageText, msg)
	c.mu.Unlock()
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	select {
	case res := <-ch:
		return res.Data, res.Err
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *cdpClient) onEvent(method string, handler func(json.RawMessage)) {
	c.handlersMu.Lock()
	c.handlers[method] = handler
	c.handlersMu.Unlock()
}

func (c *cdpClient) close() {
	c.ws.Close(websocket.StatusNormalClosure, "")
}

// viewer tracks a connected browser viewer.
type viewer struct {
	ws  *websocket.Conn
	mu  sync.Mutex
	log *slog.Logger
}

// server orchestrates CDP connection and viewer connections.
type server struct {
	cdpPort    string
	listenAddr string
	quality    int
	width      int
	height     int
	log        *slog.Logger

	ctx       context.Context
	viewers   sync.Map
	cdp       *cdpClient
	sessionID string
	targetID  string
	cdpMu     sync.Mutex

	currentURL string
	pageTitle  string
	stateMu    sync.RWMutex // protects sessionID, targetID, currentURL, pageTitle

	// sessions tracks targetID -> sessionID for attached targets
	sessions   map[string]string
	sessionsMu sync.Mutex
}

func (s *server) getSessionID() string {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.sessionID
}

func (s *server) getTargetID() string {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.targetID
}

func (s *server) setTargetState(targetID, sessionID string) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.sessionID = sessionID
	s.targetID = targetID
}

func (s *server) setPageInfo(url, title string) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if url != "" {
		s.currentURL = url
	}
	if title != "" {
		s.pageTitle = title
	}
}

func (s *server) getPageInfo() (string, string) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.currentURL, s.pageTitle
}

func (s *server) discoverBrowserWSURL(ctx context.Context) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%s/json/version", s.cdpPort)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	var info struct {
		WebSocketDebuggerUrl string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("decode /json/version: %w", err)
	}
	return info.WebSocketDebuggerUrl, nil
}

func (s *server) connectCDP(ctx context.Context) error {
	s.cdpMu.Lock()
	defer s.cdpMu.Unlock()

	if s.cdp != nil {
		return nil
	}

	wsURL, err := s.discoverBrowserWSURL(ctx)
	if err != nil {
		return fmt.Errorf("discover browser WS URL: %w", err)
	}

	s.log.Info("connecting to CDP", "url", wsURL)
	cdp, err := newCDPClient(s.ctx, wsURL, s.log)
	if err != nil {
		return err
	}
	cdp.onDisconnect = func() {
		s.cdpMu.Lock()
		defer s.cdpMu.Unlock()
		s.log.Warn("CDP connection lost, will reconnect on next viewer action")
		s.cdp = nil
		s.setTargetState("", "")
		s.sessionsMu.Lock()
		s.sessions = make(map[string]string)
		s.sessionsMu.Unlock()
	}
	s.cdp = cdp

	// Handle target attachment responses
	cdp.onEvent("Target.attachedToTarget", func(params json.RawMessage) {
		var ev struct {
			SessionID  string `json:"sessionId"`
			TargetInfo struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
			} `json:"targetInfo"`
		}
		json.Unmarshal(params, &ev)
		if ev.TargetInfo.Type == "page" {
			s.sessionsMu.Lock()
			s.sessions[ev.TargetInfo.TargetID] = ev.SessionID
			s.sessionsMu.Unlock()
			s.log.Info("target attached", "targetId", ev.TargetInfo.TargetID, "sessionId", ev.SessionID)
		}
	})

	// Register screencast frame handler
	cdp.onEvent("Page.screencastFrame", func(params json.RawMessage) {
		s.handleScreencastFrame(s.ctx, params)
	})

	// Track URL changes via frame navigation
	cdp.onEvent("Page.frameNavigated", func(params json.RawMessage) {
		var ev struct {
			Frame struct {
				URL            string `json:"url"`
				ParentID       string `json:"parentId"`
				SecurityOrigin string `json:"securityOrigin"`
			} `json:"frame"`
		}
		json.Unmarshal(params, &ev)
		if ev.Frame.ParentID == "" {
			s.setPageInfo(ev.Frame.URL, "")
			s.broadcastURLUpdate()
		}
	})

	// Track page title changes
	cdp.onEvent("Page.domContentEventFired", func(params json.RawMessage) {
		go s.fetchAndBroadcastTitle()
	})

	// When a new page target is created, auto-switch to it
	cdp.onEvent("Target.targetCreated", func(params json.RawMessage) {
		var ev struct {
			TargetInfo struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
			} `json:"targetInfo"`
		}
		json.Unmarshal(params, &ev)
		if ev.TargetInfo.Type == "page" {
			s.log.Info("new page target created", "targetId", ev.TargetInfo.TargetID, "url", ev.TargetInfo.URL)
			go s.switchToTarget(s.ctx, ev.TargetInfo.TargetID)
		}
	})

	// When a page navigates to a real URL, switch to it or update URL
	cdp.onEvent("Target.targetInfoChanged", func(params json.RawMessage) {
		var ev struct {
			TargetInfo struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
				Title    string `json:"title"`
			} `json:"targetInfo"`
		}
		json.Unmarshal(params, &ev)
		if ev.TargetInfo.Type == "page" && ev.TargetInfo.URL != "" &&
			ev.TargetInfo.URL != "about:blank" && ev.TargetInfo.URL != "chrome://newtab/" {
			currentTarget := s.getTargetID()
			if ev.TargetInfo.TargetID != currentTarget {
				s.log.Info("page navigated, switching", "targetId", ev.TargetInfo.TargetID, "url", ev.TargetInfo.URL)
				go s.switchToTarget(s.ctx, ev.TargetInfo.TargetID)
			}
			if ev.TargetInfo.TargetID == currentTarget {
				s.setPageInfo(ev.TargetInfo.URL, ev.TargetInfo.Title)
				s.broadcastURLUpdate()
			}
		}
	})

	// Enable target discovery
	_, err = cdp.call(s.ctx, "Target.setDiscoverTargets", map[string]bool{"discover": true})
	if err != nil {
		cdp.close()
		s.cdp = nil
		return fmt.Errorf("set discover targets: %w", err)
	}

	// Find an initial page to attach to
	result, err := cdp.call(s.ctx, "Target.getTargets", nil)
	if err != nil {
		cdp.close()
		s.cdp = nil
		return fmt.Errorf("get targets: %w", err)
	}

	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"targetInfos"`
	}
	json.Unmarshal(result, &targets)

	for _, t := range targets.TargetInfos {
		if t.Type == "page" {
			s.log.Info("attaching to initial page", "targetId", t.TargetID, "url", t.URL)
			if err := s.switchToTarget(s.ctx, t.TargetID); err != nil {
				s.log.Error("failed to attach to initial page", "error", err)
			}
			break
		}
	}

	return nil
}

func (s *server) switchToTarget(ctx context.Context, targetID string) error {
	if s.cdp == nil {
		return fmt.Errorf("CDP not connected")
	}

	s.sessionsMu.Lock()
	existingSession, alreadyAttached := s.sessions[targetID]
	s.sessionsMu.Unlock()

	var sessionID string

	if alreadyAttached {
		sessionID = existingSession
	} else {
		res, err := s.cdp.call(ctx, "Target.attachToTarget", map[string]any{
			"targetId": targetID,
			"flatten":  true,
		})
		if err != nil {
			return fmt.Errorf("attach to target %s: %w", targetID, err)
		}

		var attach struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(res, &attach); err != nil || attach.SessionID == "" {
			return fmt.Errorf("no sessionId in attachToTarget response for %s", targetID)
		}
		sessionID = attach.SessionID

		s.sessionsMu.Lock()
		s.sessions[targetID] = sessionID
		s.sessionsMu.Unlock()
	}

	// Stop old screencast if running
	oldSession := s.getSessionID()
	if oldSession != "" && oldSession != sessionID {
		s.cdp.callSession(ctx, oldSession, "Page.stopScreencast", nil)
	}

	s.setTargetState(targetID, sessionID)

	// Enable Page domain
	s.cdp.callSession(ctx, sessionID, "Page.enable", nil)

	// Set viewport to match screencast dimensions so headless Chrome renders at full resolution
	s.cdp.callSession(ctx, sessionID, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             s.width,
		"height":            s.height,
		"deviceScaleFactor": 1,
		"mobile":            false,
	})

	// Start screencast
	_, err := s.cdp.callSession(ctx, sessionID, "Page.startScreencast", map[string]any{
		"format":        "jpeg",
		"quality":       s.quality,
		"maxWidth":      s.width,
		"maxHeight":     s.height,
		"everyNthFrame": 1,
	})
	if err != nil {
		return fmt.Errorf("start screencast on %s: %w", targetID, err)
	}

	s.log.Info("screencast switched", "targetId", targetID, "sessionId", sessionID)

	// Fetch current URL from the navigation history
	go func() {
		result, err := s.cdp.callSession(ctx, sessionID, "Page.getNavigationHistory", nil)
		if err != nil {
			return
		}
		var nav struct {
			CurrentIndex int `json:"currentIndex"`
			Entries      []struct {
				URL   string `json:"url"`
				Title string `json:"title"`
			} `json:"entries"`
		}
		json.Unmarshal(result, &nav)
		if nav.CurrentIndex >= 0 && nav.CurrentIndex < len(nav.Entries) {
			s.setPageInfo(nav.Entries[nav.CurrentIndex].URL, nav.Entries[nav.CurrentIndex].Title)
			s.broadcastURLUpdate()
		}
	}()

	return nil
}

func (s *server) broadcastURLUpdate() {
	url, title := s.getPageInfo()
	msg, _ := json.Marshal(map[string]any{
		"type":  "url_update",
		"url":   url,
		"title": title,
	})
	s.viewers.Range(func(key, value any) bool {
		v := value.(*viewer)
		writeCtx, cancel := context.WithTimeout(s.ctx, 500*time.Millisecond)
		defer cancel()
		v.mu.Lock()
		v.ws.Write(writeCtx, websocket.MessageText, msg)
		v.mu.Unlock()
		return true
	})
}

func (s *server) fetchAndBroadcastTitle() {
	sid := s.getSessionID()
	if s.cdp == nil || sid == "" {
		return
	}
	result, err := s.cdp.callSession(s.ctx, sid, "Runtime.evaluate", map[string]any{
		"expression": "document.title",
	})
	if err != nil {
		return
	}
	var evalResult struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	json.Unmarshal(result, &evalResult)
	if evalResult.Result.Value != "" {
		s.setPageInfo("", evalResult.Result.Value)
		s.broadcastURLUpdate()
	}
}

func (s *server) handleNavigation(ctx context.Context, ev inputEvent) {
	sid := s.getSessionID()
	if s.cdp == nil || sid == "" {
		return
	}
	switch ev.Action {
	case "back":
		s.cdp.callSession(ctx, sid, "Runtime.evaluate", map[string]any{
			"expression": "history.back()",
		})
	case "forward":
		s.cdp.callSession(ctx, sid, "Runtime.evaluate", map[string]any{
			"expression": "history.forward()",
		})
	case "reload":
		s.cdp.callSession(ctx, sid, "Page.reload", nil)
	case "navigate":
		url := ev.URL
		if url != "" {
			s.cdp.callSession(ctx, sid, "Page.navigate", map[string]string{
				"url": url,
			})
		}
	}
}

func (s *server) handleScreencastFrame(ctx context.Context, params json.RawMessage) {
	var frame struct {
		Data     string `json:"data"`
		Metadata struct {
			OffsetTop       float64 `json:"offsetTop"`
			PageScaleFactor float64 `json:"pageScaleFactor"`
			DeviceWidth     float64 `json:"deviceWidth"`
			DeviceHeight    float64 `json:"deviceHeight"`
			ScrollOffsetX   float64 `json:"scrollOffsetX"`
			ScrollOffsetY   float64 `json:"scrollOffsetY"`
		} `json:"metadata"`
		SessionID int `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &frame); err != nil {
		return
	}

	// Ack the frame — capture sessionID and cdp ref now to avoid racing with switchToTarget/disconnect
	sid := s.getSessionID()
	cdp := s.cdp
	go func() {
		if cdp == nil || sid == "" {
			return
		}
		cdp.callSession(ctx, sid, "Page.screencastFrameAck", map[string]int{
			"sessionId": frame.SessionID,
		})
	}()

	jpegData, err := base64.StdEncoding.DecodeString(frame.Data)
	if err != nil {
		return
	}

	meta := map[string]any{
		"type":         "frame_meta",
		"deviceWidth":  frame.Metadata.DeviceWidth,
		"deviceHeight": frame.Metadata.DeviceHeight,
		"offsetTop":    frame.Metadata.OffsetTop,
		"scrollX":      frame.Metadata.ScrollOffsetX,
		"scrollY":      frame.Metadata.ScrollOffsetY,
	}
	metaJSON, _ := json.Marshal(meta)

	s.viewers.Range(func(key, value any) bool {
		v := value.(*viewer)
		writeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		v.mu.Lock()
		v.ws.Write(writeCtx, websocket.MessageText, metaJSON)
		v.ws.Write(writeCtx, websocket.MessageBinary, jpegData)
		v.mu.Unlock()
		return true
	})
}

type inputEvent struct {
	Type string `json:"type"`

	MouseType  string  `json:"mouseType,omitempty"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Button     string  `json:"button,omitempty"`
	ClickCount int     `json:"clickCount,omitempty"`
	DeltaX     float64 `json:"deltaX,omitempty"`
	DeltaY     float64 `json:"deltaY,omitempty"`
	Modifiers  int     `json:"modifiers,omitempty"`

	KeyType string `json:"keyType,omitempty"`
	Key     string `json:"key,omitempty"`
	Code    string `json:"code,omitempty"`
	Text    string `json:"text,omitempty"`
	KeyCode int    `json:"keyCode,omitempty"`

	Action string `json:"action,omitempty"`
	URL    string `json:"url,omitempty"`
}

func (s *server) handleInput(ctx context.Context, ev inputEvent) {
	sid := s.getSessionID()
	if s.cdp == nil || sid == "" {
		return
	}

	switch ev.Type {
	case "mouse":
		params := map[string]any{
			"type":      ev.MouseType,
			"x":         ev.X,
			"y":         ev.Y,
			"modifiers": ev.Modifiers,
		}
		if ev.MouseType == "mousePressed" || ev.MouseType == "mouseReleased" {
			params["button"] = ev.Button
			params["clickCount"] = ev.ClickCount
			if ev.ClickCount == 0 {
				params["clickCount"] = 1
			}
		}
		if ev.MouseType == "mouseWheel" {
			params["type"] = "mouseWheel"
			params["deltaX"] = ev.DeltaX
			params["deltaY"] = ev.DeltaY
		}
		s.cdp.callSession(ctx, sid, "Input.dispatchMouseEvent", params)

	case "key":
		params := map[string]any{
			"type":                   ev.KeyType,
			"modifiers":             ev.Modifiers,
			"key":                   ev.Key,
			"code":                  ev.Code,
			"windowsVirtualKeyCode": ev.KeyCode,
			"nativeVirtualKeyCode":  ev.KeyCode,
		}
		if ev.Text != "" {
			params["text"] = ev.Text
		}
		s.cdp.callSession(ctx, sid, "Input.dispatchKeyEvent", params)

	case "navigate":
		s.handleNavigation(ctx, ev)
	}
}

func (s *server) handleViewer(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.log.Error("accept viewer ws", "error", err)
		return
	}
	ws.SetReadLimit(64 * 1024)

	v := &viewer{ws: ws, log: s.log}
	viewerID := fmt.Sprintf("%p", v)
	s.viewers.Store(viewerID, v)
	s.log.Info("viewer connected", "id", viewerID)

	defer func() {
		s.viewers.Delete(viewerID)
		ws.Close(websocket.StatusNormalClosure, "")
		s.log.Info("viewer disconnected", "id", viewerID)
	}()

	if err := s.connectCDP(s.ctx); err != nil {
		s.log.Error("connect CDP for viewer", "error", err)
		return
	}

	// Send current URL to newly connected viewer
	currentURL, pageTitle := s.getPageInfo()
	if currentURL != "" {
		msg, _ := json.Marshal(map[string]any{
			"type":  "url_update",
			"url":   currentURL,
			"title": pageTitle,
		})
		writeCtx, cancel := context.WithTimeout(s.ctx, 500*time.Millisecond)
		ws.Write(writeCtx, websocket.MessageText, msg)
		cancel()
	}

	for {
		_, data, err := ws.Read(s.ctx)
		if err != nil {
			return
		}
		var ev inputEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		s.handleInput(s.ctx, ev)
	}
}

func (s *server) serveViewer(w http.ResponseWriter, r *http.Request) {
	data, err := viewerFS.ReadFile("viewer.html")
	if err != nil {
		http.Error(w, "viewer not found", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *server) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "ok")
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n := def
	fmt.Sscanf(v, "%d", &n)
	return n
}

func main() {
	cdpPort := flag.String("cdp-port", "", "CDP port (default: INTERNAL_PORT env or 9223)")
	listen := flag.String("listen", ":8080", "HTTP listen address")
	quality := flag.Int("quality", 80, "JPEG quality (1-100)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	port := *cdpPort
	if port == "" {
		port = os.Getenv("INTERNAL_PORT")
	}
	if port == "" {
		port = "9223"
	}

	s := &server{
		cdpPort:    port,
		listenAddr: *listen,
		quality:    *quality,
		width:      envInt("WIDTH", 1920),
		height:     envInt("HEIGHT", 1080),
		ctx:        context.Background(),
		sessions:   make(map[string]string),
		log:        log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.serveViewer)
	mux.HandleFunc("/ws", s.handleViewer)
	mux.HandleFunc("/health", s.healthHandler)

	log.Info("starting cdp-live-view", "listen", s.listenAddr, "cdpPort", s.cdpPort)
	if err := http.ListenAndServe(s.listenAddr, mux); err != nil {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
}
