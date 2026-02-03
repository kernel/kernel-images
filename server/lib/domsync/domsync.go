// Package domsync provides real-time DOM bounding box synchronization from the browser
// to connected clients. It injects a MutationObserver into the browser page and broadcasts
// element positions to Vue clients via WebSocket.
package domsync

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/onkernel/kernel-images/server/lib/devtoolsproxy"
)

// DomElementType represents the category of a DOM element
type DomElementType string

const (
	DomElementTypeInputs   DomElementType = "inputs"
	DomElementTypeButtons  DomElementType = "buttons"
	DomElementTypeLinks    DomElementType = "links"
	DomElementTypeImages   DomElementType = "images"
	DomElementTypeMedia    DomElementType = "media"
)

// DomElement represents an interactive element in the browser
type DomElement struct {
	ID   string         `json:"id"`
	Tag  string         `json:"tag"`
	Type DomElementType `json:"type"`
	Rect DomRect        `json:"rect"`
	Z    json.Number    `json:"z"`
}

// DomRect represents a bounding rectangle
type DomRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// DomViewport represents the browser viewport dimensions
type DomViewport struct {
	W  int `json:"w"`
	H  int `json:"h"`
	SX int `json:"sx"` // scrollX
	SY int `json:"sy"` // scrollY
}

// DomWindowBounds represents the browser window position and chrome offsets
type DomWindowBounds struct {
	X          int  `json:"x"`          // Window X position on screen
	Y          int  `json:"y"`          // Window Y position on screen
	Width      int  `json:"width"`      // Window outer width
	Height     int  `json:"height"`     // Window outer height
	ChromeTop  int  `json:"chromeTop"`  // Offset from window top to viewport top
	ChromeLeft int  `json:"chromeLeft"` // Offset from window left to viewport left
	Fullscreen bool `json:"fullscreen"` // Whether browser is in fullscreen mode
}

// DomSyncPayload is the data sent to clients
type DomSyncPayload struct {
	Seq          int64           `json:"seq"`
	Ts           int64           `json:"ts"`
	Elements     []DomElement    `json:"elements"`
	Viewport     DomViewport     `json:"viewport"`
	WindowBounds DomWindowBounds `json:"windowBounds"`
	URL          string          `json:"url"`
}

// DomMessage is the WebSocket message wrapper
type DomMessage struct {
	Event string          `json:"event"`
	Data  *DomSyncPayload `json:"data,omitempty"`
}

// Manager handles DOM sync operations
type Manager struct {
	upstreamMgr *devtoolsproxy.UpstreamManager
	logger      *slog.Logger

	// Connected client WebSockets
	clientsMu sync.RWMutex
	clients   map[*websocket.Conn]struct{}

	// CDP connection state
	cdpMu      sync.Mutex
	cdpConn    *websocket.Conn
	cdpCancel  context.CancelFunc
	cdpRunning bool

	// Current attached target
	currentSessionID string
	currentTargetID  string

	// Window bounds from CDP (cached, updated periodically)
	windowBoundsMu sync.RWMutex
	windowBounds   DomWindowBounds

	// Last known state for new clients
	lastPayloadMu sync.RWMutex
	lastPayload   *DomSyncPayload

	// Sequence counter
	seq atomic.Int64

	// Throttle state
	throttleMu      sync.Mutex
	lastBroadcastAt time.Time
	pendingPayload  *DomSyncPayload
	throttleTimer   *time.Timer

	// CDP message counter for request IDs
	cdpMsgID atomic.Int64
}

const (
	throttleInterval = 100 * time.Millisecond // Max 10Hz updates - balance between responsiveness and performance
	bindingName      = "__domSyncCallback__"
)

// Observer script to inject into the browser page
// Detects various DOM element types: inputs, buttons, links, images, media
const observerScript = `
(function() {
  // Allow re-initialization if binding was re-added
  if (window.__domSyncInitialized__ && window.__domSyncCallback__) return;
  window.__domSyncInitialized__ = true;

  // Element type definitions with selectors
  const ELEMENT_TYPES = {
    inputs: 'input:not([type="button"]):not([type="submit"]):not([type="reset"]):not([type="image"]):not([type="hidden"]), textarea, select, [contenteditable="true"], [contenteditable=""], [role="textbox"], [role="searchbox"], [role="combobox"], [aria-label*="Search"], .gLFyf, [name="q"]',
    buttons: 'button, input[type="button"], input[type="submit"], input[type="reset"], [role="button"]',
    links: 'a[href]',
    images: 'img, picture, svg, [role="img"], input[type="image"]',
    media: 'video, audio'
  };

  // Combined selector for all types
  const ALL_SELECTORS = Object.values(ELEMENT_TYPES).join(', ');

  let idCounter = 0;
  let lastSentJSON = '';

  // Determine element type based on selectors
  function getElementType(el) {
    for (const [type, selector] of Object.entries(ELEMENT_TYPES)) {
      try {
        if (el.matches(selector)) return type;
      } catch(e) {}
    }
    return 'inputs'; // fallback
  }

  function extractFromShadowRoot(root, elements) {
    if (!root) return;
    try {
      root.querySelectorAll(ALL_SELECTORS).forEach((el) => processElement(el, elements));
      // Recursively check shadow roots
      root.querySelectorAll('*').forEach((el) => {
        if (el.shadowRoot) {
          extractFromShadowRoot(el.shadowRoot, elements);
        }
      });
    } catch(e) {}
  }

  function processElement(el, elements) {
    if (el.type === 'hidden') return;
    const rect = el.getBoundingClientRect();
    if (rect.width < 2 || rect.height < 2) return;
    const style = getComputedStyle(el);
    if (style.display === 'none' || style.visibility === 'hidden' || parseFloat(style.opacity) === 0) return;
    if (!el.dataset.domid) el.dataset.domid = 'd' + (idCounter++);
    elements.push({
      id: el.dataset.domid,
      tag: el.tagName.toLowerCase(),
      type: getElementType(el),
      rect: { x: Math.round(rect.x), y: Math.round(rect.y), w: Math.round(rect.width), h: Math.round(rect.height) }
    });
  }

  function extract() {
    const elements = [];
    try {
      // Main document
      document.querySelectorAll(ALL_SELECTORS).forEach((el) => processElement(el, elements));
      // Check shadow roots
      document.querySelectorAll('*').forEach((el) => {
        if (el.shadowRoot) {
          extractFromShadowRoot(el.shadowRoot, elements);
        }
      });
    } catch(e) {
      console.error('[dom-sync] extract error:', e);
    }

    const fs = !!(document.fullscreenElement || document.webkitFullscreenElement);
    const ct = fs ? 0 : Math.max(0, window.outerHeight - window.innerHeight - 2);
    const cl = fs ? 0 : Math.round((window.outerWidth - window.innerWidth) / 2);

    return {
      e: elements,
      v: { w: window.innerWidth, h: window.innerHeight },
      b: { x: window.screenX, y: window.screenY, w: window.outerWidth, h: window.outerHeight, ct: ct, cl: cl, fs: fs },
      u: location.href
    };
  }

  function send() {
    if (typeof window.__domSyncCallback__ !== 'function') {
      return; // Silently wait - binding may not be ready yet
    }
    try {
      const data = extract();
      const json = JSON.stringify(data);
      // Only send if data changed (reduces noise)
      if (json !== lastSentJSON) {
        lastSentJSON = json;
        window.__domSyncCallback__(json);
      }
    } catch(e) {
      console.error('[dom-sync] send error:', e);
    }
  }

  // Throttle
  let timer = null;
  function throttledSend() {
    if (timer) return;
    send();
    timer = setTimeout(() => { timer = null; }, 150);
  }

  // Observe DOM changes - wait for document.body if needed
  function setupObserver() {
    const target = document.body || document.documentElement;
    if (!target) {
      setTimeout(setupObserver, 50);
      return;
    }
    try {
      new MutationObserver(throttledSend).observe(target, {
        childList: true, subtree: true, attributes: true,
        attributeFilter: ['style', 'class', 'hidden', 'disabled', 'type']
      });
    } catch(e) {
      console.error('[dom-sync] MutationObserver error:', e);
    }
  }
  setupObserver();

  window.addEventListener('scroll', throttledSend, { passive: true });
  window.addEventListener('resize', throttledSend, { passive: true });
  window.addEventListener('focusin', throttledSend, { passive: true });

  // Send periodically - this is the main reliable mechanism
  setInterval(send, 500);

  // Initial attempts with increasing delays
  send();
  setTimeout(send, 50);
  setTimeout(send, 150);
  setTimeout(send, 300);
  setTimeout(send, 600);
  setTimeout(send, 1200);
})();
`

// NewManager creates a new DOM sync manager
func NewManager(upstreamMgr *devtoolsproxy.UpstreamManager, logger *slog.Logger) *Manager {
	return &Manager{
		upstreamMgr: upstreamMgr,
		logger:      logger,
		clients:     make(map[*websocket.Conn]struct{}),
	}
}

// Start begins the DOM sync manager, connecting to the browser via CDP
func (m *Manager) Start(ctx context.Context) {
	go m.cdpLoop(ctx)
}

// Stop shuts down the manager
func (m *Manager) Stop() {
	m.cdpMu.Lock()
	if m.cdpCancel != nil {
		m.cdpCancel()
	}
	m.cdpMu.Unlock()

	m.clientsMu.Lock()
	for conn := range m.clients {
		conn.Close(websocket.StatusGoingAway, "server shutting down")
		delete(m.clients, conn)
	}
	m.clientsMu.Unlock()
}

// HandleWebSocket handles a client WebSocket connection
func (m *Manager) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		m.logger.Error("[dom-sync] websocket accept failed", "err", err)
		return
	}

	m.logger.Info("[dom-sync] client connected")

	// Register client
	m.clientsMu.Lock()
	m.clients[conn] = struct{}{}
	m.clientsMu.Unlock()

	// Send last known state immediately
	m.lastPayloadMu.RLock()
	lastPayload := m.lastPayload
	m.lastPayloadMu.RUnlock()

	if lastPayload != nil {
		msg := DomMessage{Event: "dom/sync", Data: lastPayload}
		if data, err := json.Marshal(msg); err == nil {
			conn.Write(context.Background(), websocket.MessageText, data)
		}
	}

	// Keep the handler running - read messages until connection closes
	// Using background context since r.Context() is cancelled when handler returns
	defer func() {
		m.clientsMu.Lock()
		delete(m.clients, conn)
		m.clientsMu.Unlock()
		conn.Close(websocket.StatusNormalClosure, "")
		m.logger.Info("[dom-sync] client disconnected")
	}()

	for {
		_, _, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		// We don't need to handle client messages for now
	}
}

// cdpLoop maintains the CDP connection and reinjects the observer on page loads
func (m *Manager) cdpLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Try with current URL if we have it
		if url := m.upstreamMgr.Current(); url != "" {
			m.connectAndObserve(ctx, url)
		}
		time.Sleep(2 * time.Second)
	}
}

func (m *Manager) connectAndObserve(ctx context.Context, upstreamURL string) {
	m.cdpMu.Lock()
	if m.cdpRunning {
		m.cdpMu.Unlock()
		return
	}
	m.cdpRunning = true
	cdpCtx, cancel := context.WithCancel(ctx)
	m.cdpCancel = cancel
	m.cdpMu.Unlock()

	defer func() {
		m.cdpMu.Lock()
		m.cdpRunning = false
		m.cdpCancel = nil
		m.currentSessionID = ""
		m.currentTargetID = ""
		m.cdpMu.Unlock()
	}()

	m.logger.Info("[dom-sync] connecting to CDP", "url", upstreamURL)

	conn, _, err := websocket.Dial(cdpCtx, upstreamURL, nil)
	if err != nil {
		m.logger.Error("[dom-sync] CDP dial failed", "err", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	m.cdpMu.Lock()
	m.cdpConn = conn
	m.cdpMu.Unlock()

	// Enable Target domain to discover and attach to pages
	if err := m.cdpSend(cdpCtx, conn, "", "Target.setDiscoverTargets", map[string]interface{}{
		"discover": true,
	}); err != nil {
		m.logger.Error("[dom-sync] failed to enable target discovery", "err", err)
		return
	}

	m.logger.Info("[dom-sync] CDP connected, discovering targets")

	// Find and attach to the first page target
	if err := m.findAndAttachToPage(cdpCtx, conn); err != nil {
		m.logger.Error("[dom-sync] failed to attach to page", "err", err)
		return
	}

	// Listen for CDP events
	for {
		select {
		case <-cdpCtx.Done():
			return
		default:
		}

		_, msg, err := conn.Read(cdpCtx)
		if err != nil {
			m.logger.Error("[dom-sync] CDP read error", "err", err)
			return
		}

		var event map[string]interface{}
		if err := json.Unmarshal(msg, &event); err != nil {
			continue
		}

		method, _ := event["method"].(string)
		params, _ := event["params"].(map[string]interface{})
		sessionId, _ := event["sessionId"].(string)

		switch method {
		case "Runtime.bindingCalled":
			// Handle DOM sync callback - only from our session
			if sessionId == m.currentSessionID {
				name, _ := params["name"].(string)
				payload, _ := params["payload"].(string)
				if name == bindingName && payload != "" {
					m.handleDomCallback(payload)
				}
			}

		case "Page.loadEventFired", "Page.domContentEventFired":
			// Re-inject observer on page load - minimal delay
			if sessionId == m.currentSessionID {
				go func() {
					time.Sleep(10 * time.Millisecond)
					m.injectObserverScript(cdpCtx, conn, m.currentSessionID)
				}()
			}

		case "Page.frameNavigated":
			// Re-inject on navigation - slightly longer for frame setup
			if sessionId == m.currentSessionID {
				go func() {
					time.Sleep(25 * time.Millisecond)
					m.injectObserverScript(cdpCtx, conn, m.currentSessionID)
				}()
			}

		case "Target.targetInfoChanged":
			// Check if a different tab became active
			targetInfo, _ := params["targetInfo"].(map[string]interface{})
			if targetInfo != nil {
				targetType, _ := targetInfo["type"].(string)
				targetID, _ := targetInfo["targetId"].(string)
				attached, _ := targetInfo["attached"].(bool)

				// If this is a page and it's not our current target, consider switching
				if targetType == "page" && targetID != m.currentTargetID && !attached {
					m.logger.Debug("[dom-sync] detected new page target", "targetId", targetID)
				}
			}

		case "Target.targetCreated":
			// A new target was created
			targetInfo, _ := params["targetInfo"].(map[string]interface{})
			if targetInfo != nil {
				targetType, _ := targetInfo["type"].(string)
				if targetType == "page" {
					m.logger.Debug("[dom-sync] new page target created")
				}
			}

		case "Target.targetDestroyed":
			// Our target was destroyed, need to find a new one
			targetID, _ := params["targetId"].(string)
			if targetID == m.currentTargetID {
				m.logger.Info("[dom-sync] current target destroyed, finding new target")
				go func() {
					time.Sleep(500 * time.Millisecond)
					m.findAndAttachToPage(cdpCtx, conn)
				}()
			}

		case "Target.detachedFromTarget":
			// We were detached from the target
			detachedSessionId, _ := params["sessionId"].(string)
			if detachedSessionId == m.currentSessionID {
				m.logger.Info("[dom-sync] detached from target, reattaching")
				m.currentSessionID = ""
				m.currentTargetID = ""
				go func() {
					time.Sleep(500 * time.Millisecond)
					m.findAndAttachToPage(cdpCtx, conn)
				}()
			}
		}
	}
}

func (m *Manager) findAndAttachToPage(ctx context.Context, conn *websocket.Conn) error {
	// Get list of targets
	id := m.cdpMsgID.Add(1)
	msg := map[string]interface{}{
		"id":     id,
		"method": "Target.getTargets",
	}
	data, _ := json.Marshal(msg)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}

	// Read response - we need to find the result
	for i := 0; i < 10; i++ {
		_, respData, err := conn.Read(ctx)
		if err != nil {
			return err
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(respData, &resp); err != nil {
			continue
		}

		// Check if this is our response
		respID, _ := resp["id"].(float64)
		if int64(respID) == id {
			result, _ := resp["result"].(map[string]interface{})
			targetInfos, _ := result["targetInfos"].([]interface{})

			// Find the first page target
			for _, ti := range targetInfos {
				targetInfo, _ := ti.(map[string]interface{})
				targetType, _ := targetInfo["type"].(string)
				targetID, _ := targetInfo["targetId"].(string)

				if targetType == "page" && targetID != "" {
					m.logger.Info("[dom-sync] found page target", "targetId", targetID)
					return m.attachToTarget(ctx, conn, targetID)
				}
			}
			m.logger.Warn("[dom-sync] no page targets found")
			return nil
		}

		// If it's an event, we might need to handle it
		if method, ok := resp["method"].(string); ok {
			m.logger.Debug("[dom-sync] received event while waiting for targets", "method", method)
		}
	}

	return nil
}

func (m *Manager) attachToTarget(ctx context.Context, conn *websocket.Conn, targetID string) error {
	// Attach to the target with flatten=true to get a session
	id := m.cdpMsgID.Add(1)
	msg := map[string]interface{}{
		"id":     id,
		"method": "Target.attachToTarget",
		"params": map[string]interface{}{
			"targetId": targetID,
			"flatten":  true,
		},
	}
	data, _ := json.Marshal(msg)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}

	// Read response to get session ID
	for i := 0; i < 10; i++ {
		_, respData, err := conn.Read(ctx)
		if err != nil {
			return err
		}

		var resp map[string]interface{}
		if err := json.Unmarshal(respData, &resp); err != nil {
			continue
		}

		respID, _ := resp["id"].(float64)
		if int64(respID) == id {
			result, _ := resp["result"].(map[string]interface{})
			sessionId, _ := result["sessionId"].(string)

			if sessionId == "" {
				m.logger.Error("[dom-sync] no session ID in attach response")
				return nil
			}

			m.cdpMu.Lock()
			m.currentSessionID = sessionId
			m.currentTargetID = targetID
			m.cdpMu.Unlock()

			m.logger.Info("[dom-sync] attached to target", "sessionId", sessionId, "targetId", targetID)

			// Now set up the session
			return m.setupSession(ctx, conn, sessionId)
		}
	}

	return nil
}

func (m *Manager) setupSession(ctx context.Context, conn *websocket.Conn, sessionId string) error {
	// Add the binding for the callback
	if err := m.cdpSend(ctx, conn, sessionId, "Runtime.addBinding", map[string]interface{}{
		"name": bindingName,
	}); err != nil {
		m.logger.Error("[dom-sync] failed to add binding", "err", err)
		return err
	}

	// Enable Runtime domain to receive binding calls
	if err := m.cdpSend(ctx, conn, sessionId, "Runtime.enable", nil); err != nil {
		m.logger.Error("[dom-sync] failed to enable Runtime", "err", err)
		return err
	}

	// Enable Page domain for load events
	if err := m.cdpSend(ctx, conn, sessionId, "Page.enable", nil); err != nil {
		m.logger.Error("[dom-sync] failed to enable Page", "err", err)
		return err
	}

	// Add script to evaluate on every new document (persists across navigations)
	if err := m.cdpSend(ctx, conn, sessionId, "Page.addScriptToEvaluateOnNewDocument", map[string]interface{}{
		"source": observerScript,
	}); err != nil {
		m.logger.Warn("[dom-sync] failed to add script to new documents", "err", err)
	}

	// Inject the observer script now
	if err := m.injectObserverScript(ctx, conn, sessionId); err != nil {
		m.logger.Error("[dom-sync] failed to inject observer", "err", err)
		return err
	}

	m.logger.Info("[dom-sync] session setup complete")
	return nil
}

func (m *Manager) cdpSend(ctx context.Context, conn *websocket.Conn, sessionId string, method string, params interface{}) error {
	id := m.cdpMsgID.Add(1)
	msg := map[string]interface{}{
		"id":     id,
		"method": method,
	}
	if sessionId != "" {
		msg["sessionId"] = sessionId
	}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func (m *Manager) injectObserverScript(ctx context.Context, conn *websocket.Conn, sessionId string) error {
	m.logger.Debug("[dom-sync] injecting observer script", "sessionId", sessionId)
	return m.cdpSend(ctx, conn, sessionId, "Runtime.evaluate", map[string]interface{}{
		"expression":            observerScript,
		"includeCommandLineAPI": true,
	})
}

func (m *Manager) handleDomCallback(payload string) {
	// Compact payload format: e=elements, v=viewport, b=bounds, u=url
	var data struct {
		E []struct {
			ID   string `json:"id"`
			Tag  string `json:"tag"`
			Type string `json:"type"`
			Rect struct {
				X int `json:"x"`
				Y int `json:"y"`
				W int `json:"w"`
				H int `json:"h"`
			} `json:"rect"`
		} `json:"e"`
		V struct {
			W int `json:"w"`
			H int `json:"h"`
		} `json:"v"`
		B struct {
			X  int  `json:"x"`
			Y  int  `json:"y"`
			W  int  `json:"w"`
			H  int  `json:"h"`
			CT int  `json:"ct"` // chromeTop
			CL int  `json:"cl"` // chromeLeft
			FS bool `json:"fs"` // fullscreen
		} `json:"b"`
		U string `json:"u"` // URL
	}
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		m.logger.Error("[dom-sync] failed to parse callback payload", "err", err, "payload", payload[:min(len(payload), 200)])
		return
	}

	m.logger.Info("[dom-sync] received callback", "domElements", len(data.E), "chromeTop", data.B.CT, "url", data.U)

	// Convert to full format for client
	windowBounds := DomWindowBounds{
		X:          data.B.X,
		Y:          data.B.Y,
		Width:      data.B.W,
		Height:     data.B.H,
		ChromeTop:  data.B.CT,
		ChromeLeft: data.B.CL,
		Fullscreen: data.B.FS,
	}

	// Convert elements
	elements := make([]DomElement, 0, len(data.E))
	for _, e := range data.E {
		elementType := DomElementType(e.Type)
		if elementType == "" {
			elementType = DomElementTypeInputs // default fallback
		}
		elements = append(elements, DomElement{
			ID:   e.ID,
			Tag:  e.Tag,
			Type: elementType,
			Rect: DomRect{
				X: e.Rect.X,
				Y: e.Rect.Y,
				W: e.Rect.W,
				H: e.Rect.H,
			},
			Z: json.Number("0"),
		})
	}

	// Add synthetic addressbar element if not fullscreen and chrome is visible
	if !windowBounds.Fullscreen && windowBounds.ChromeTop > 50 {
		addressBarY := -windowBounds.ChromeTop + 40
		addressBarX := 140 - windowBounds.ChromeLeft  // Start further right to avoid refresh button
		addressBarWidth := windowBounds.Width - 350   // Narrower to avoid right-side buttons

		if addressBarWidth > 100 {
			elements = append(elements, DomElement{
				ID:   "addressbar",
				Tag:  "input",
				Type: DomElementTypeInputs,
				Rect: DomRect{X: addressBarX, Y: addressBarY, W: addressBarWidth, H: 35},
				Z:    json.Number("0"),
			})
		}
	}

	syncPayload := &DomSyncPayload{
		Seq:          m.seq.Add(1),
		Ts:           time.Now().UnixMilli(),
		Elements:     elements,
		Viewport:     DomViewport{W: data.V.W, H: data.V.H},
		WindowBounds: windowBounds,
		URL:          data.U,
	}

	// Throttle broadcasts
	m.throttleMu.Lock()
	now := time.Now()
	timeSinceLast := now.Sub(m.lastBroadcastAt)

	if timeSinceLast >= throttleInterval {
		m.lastBroadcastAt = now
		m.throttleMu.Unlock()
		m.broadcast(syncPayload)
	} else {
		// Schedule deferred broadcast
		m.pendingPayload = syncPayload
		if m.throttleTimer == nil {
			m.throttleTimer = time.AfterFunc(throttleInterval-timeSinceLast, func() {
				m.throttleMu.Lock()
				pending := m.pendingPayload
				m.pendingPayload = nil
				m.throttleTimer = nil
				m.lastBroadcastAt = time.Now()
				m.throttleMu.Unlock()
				if pending != nil {
					m.broadcast(pending)
				}
			})
		}
		m.throttleMu.Unlock()
	}
}

func (m *Manager) broadcast(payload *DomSyncPayload) {
	// Store last payload for new clients
	m.lastPayloadMu.Lock()
	m.lastPayload = payload
	m.lastPayloadMu.Unlock()

	msg := DomMessage{Event: "dom/sync", Data: payload}
	data, err := json.Marshal(msg)
	if err != nil {
		m.logger.Error("[dom-sync] failed to marshal payload", "err", err)
		return
	}

	m.clientsMu.RLock()
	clients := make([]*websocket.Conn, 0, len(m.clients))
	for conn := range m.clients {
		clients = append(clients, conn)
	}
	m.clientsMu.RUnlock()

	for _, conn := range clients {
		if err := conn.Write(context.Background(), websocket.MessageText, data); err != nil {
			m.logger.Debug("[dom-sync] failed to send to client", "err", err)
		}
	}

	m.logger.Info("[dom-sync] broadcast",
		"clients", len(clients),
		"elements", len(payload.Elements),
		"seq", payload.Seq,
		"url", payload.URL)
}

// ClientCount returns the number of connected clients
func (m *Manager) ClientCount() int {
	m.clientsMu.RLock()
	defer m.clientsMu.RUnlock()
	return len(m.clients)
}

// IsConnected returns whether the manager is connected to CDP
func (m *Manager) IsConnected() bool {
	m.cdpMu.Lock()
	defer m.cdpMu.Unlock()
	return m.cdpRunning
}

// Handler returns an http.Handler for the DOM sync WebSocket endpoint
func (m *Manager) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.HandleWebSocket(w, r)
	})
}

// GetLastPayload returns the last known DOM sync payload
func (m *Manager) GetLastPayload() *DomSyncPayload {
	m.lastPayloadMu.RLock()
	defer m.lastPayloadMu.RUnlock()
	return m.lastPayload
}

// Status returns the current status of the DOM sync manager
type Status struct {
	Connected   bool   `json:"connected"`
	ClientCount int    `json:"clientCount"`
	LastSeq     int64  `json:"lastSeq"`
	SessionID   string `json:"sessionId"`
	TargetID    string `json:"targetId"`
}

func (m *Manager) Status() Status {
	m.lastPayloadMu.RLock()
	lastSeq := int64(0)
	if m.lastPayload != nil {
		lastSeq = m.lastPayload.Seq
	}
	m.lastPayloadMu.RUnlock()

	m.cdpMu.Lock()
	sessionID := m.currentSessionID
	targetID := m.currentTargetID
	m.cdpMu.Unlock()

	return Status{
		Connected:   m.IsConnected(),
		ClientCount: m.ClientCount(),
		LastSeq:     lastSeq,
		SessionID:   sessionID,
		TargetID:    targetID,
	}
}
