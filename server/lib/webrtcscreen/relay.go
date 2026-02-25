package webrtcscreen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	cws "github.com/coder/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

// Relay is a WebRTC SFU that connects to a local Neko instance,
// receives its VP8 video stream, and re-serves it to external
// WebRTC clients. The API server mounts HandleWebSocket on a
// single endpoint (e.g., /display/webrtc) — that's all external
// clients need to connect and receive the live screen.
//
// The Neko connection is lazy: it is only established when the
// first client connects via HandleWebSocket.
type Relay struct {
	logger *slog.Logger
	cfg    RelayConfig
	ctx    context.Context

	mu         sync.RWMutex
	localTrack *webrtc.TrackLocalStaticRTP
	nekoPC     *webrtc.PeerConnection
	nekoWS     *cws.Conn
	ready      chan struct{} // closed when localTrack is receiving data

	startOnce sync.Once
}

type RelayConfig struct {
	NekoBaseURL string
	NekoUser    string
	NekoPass    string
	Logger      *slog.Logger
}

func NewRelay(ctx context.Context, cfg RelayConfig) (*Relay, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		"video", "screen",
	)
	if err != nil {
		return nil, fmt.Errorf("creating local track: %w", err)
	}

	return &Relay{
		logger:     cfg.Logger.With("component", "webrtc-relay"),
		cfg:        cfg,
		ctx:        ctx,
		localTrack: localTrack,
		ready:      make(chan struct{}),
	}, nil
}

// ensureRunning starts the Neko connection loop in the background
// on the first call. Subsequent calls are no-ops.
func (r *Relay) ensureRunning() {
	r.startOnce.Do(func() {
		r.logger.Info("first client request, starting neko connection")
		go func() {
			defer r.Close()
			for {
				err := r.Start(r.ctx)
				if r.ctx.Err() != nil {
					return
				}
				r.logger.Warn("webrtc relay disconnected, reconnecting in 3s", "err", err)
				select {
				case <-r.ctx.Done():
					return
				case <-time.After(3 * time.Second):
				}
			}
		}()
	})
}

// Start connects to Neko and begins relaying video. It blocks until
// the Neko connection drops or ctx is cancelled. Callers should call
// Start in a loop for automatic reconnection.
func (r *Relay) Start(ctx context.Context) error {
	r.mu.Lock()
	r.ready = make(chan struct{})
	r.mu.Unlock()

	token, err := r.nekoLogin(ctx)
	if err != nil {
		return fmt.Errorf("neko login: %w", err)
	}

	wsURL := r.cfg.NekoBaseURL + "/api/ws"
	ws, _, err := cws.Dial(ctx, wsURL, &cws.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + token},
		},
	})
	if err != nil {
		return fmt.Errorf("neko ws dial: %w", err)
	}
	ws.SetReadLimit(1 << 20)

	r.mu.Lock()
	r.nekoWS = ws
	r.mu.Unlock()

	defer func() {
		ws.Close(cws.StatusGoingAway, "done")
		r.mu.Lock()
		r.nekoWS = nil
		r.mu.Unlock()
	}()

	if err := r.waitForEvent(ctx, ws, "system/init"); err != nil {
		return fmt.Errorf("waiting for system/init: %w", err)
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("creating neko peer connection: %w", err)
	}
	r.mu.Lock()
	r.nekoPC = pc
	r.mu.Unlock()
	defer func() {
		pc.Close()
		r.mu.Lock()
		r.nekoPC = nil
		r.mu.Unlock()
	}()

	trackReceived := make(chan struct{}, 1)
	var forwardOnce sync.Once

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeVideo {
			return
		}
		r.logger.Info("neko video track received",
			"codec", track.Codec().MimeType,
			"ssrc", track.SSRC(),
		)

		select {
		case trackReceived <- struct{}{}:
		default:
		}

		forwardOnce.Do(func() {
			r.mu.Lock()
			select {
			case <-r.ready:
			default:
				close(r.ready)
			}
			r.mu.Unlock()

			r.forwardRTP(track)
		})
	})

	disconnected := make(chan struct{})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		r.logger.Info("neko peer connection state", "state", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			select {
			case <-disconnected:
			default:
				close(disconnected)
			}
		}
	})

	// Send signal/request to Neko (audio disabled).
	reqPayload := json.RawMessage(`{"video":{},"audio":{"disabled":true}}`)
	if err := sendWSMsg(ctx, ws, "signal/request", reqPayload); err != nil {
		return fmt.Errorf("sending signal/request: %w", err)
	}

	offerSDP, err := r.waitForOffer(ctx, ws)
	if err != nil {
		return fmt.Errorf("waiting for neko SDP offer: %w", err)
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}); err != nil {
		return fmt.Errorf("setting neko remote desc: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("creating answer: %w", err)
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("setting local desc: %w", err)
	}

	select {
	case <-gatherDone:
	case <-ctx.Done():
		return ctx.Err()
	}

	answerJSON, _ := json.Marshal(struct {
		SDP string `json:"sdp"`
	}{SDP: pc.LocalDescription().SDP})
	if err := sendWSMsg(ctx, ws, "signal/answer", answerJSON); err != nil {
		return fmt.Errorf("sending signal/answer: %w", err)
	}

	r.logger.Info("neko signaling complete, waiting for video track")

	// Background reader for Neko WS (heartbeats, ICE candidates, etc.)
	go r.nekoWSLoop(ctx, ws, pc)

	select {
	case <-trackReceived:
		r.logger.Info("neko video track active, relay ready")
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for neko video track")
	case <-ctx.Done():
		return ctx.Err()
	}

	// Block until disconnection or cancellation.
	select {
	case <-disconnected:
		return fmt.Errorf("neko peer connection lost")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// HandleWebSocket is the HTTP handler for external WebRTC client signaling.
// Mount as: r.Get("/display/webrtc", relay.HandleWebSocket)
//
// Protocol (two messages total, no trickle ICE):
//
//	Client → Server: {"type":"offer","sdp":"v=0\r\n..."}
//	Server → Client: {"type":"answer","sdp":"v=0\r\n..."}
//
// After the exchange, WebRTC media flows directly. The WebSocket
// can be closed.
func (r *Relay) HandleWebSocket(w http.ResponseWriter, req *http.Request) {
	r.ensureRunning()

	// Wait for the relay to be connected to Neko before accepting the client.
	select {
	case <-r.Ready():
	case <-time.After(15 * time.Second):
		http.Error(w, "relay not ready", http.StatusServiceUnavailable)
		return
	case <-req.Context().Done():
		return
	}

	ws, err := cws.Accept(w, req, nil)
	if err != nil {
		r.logger.Error("websocket accept failed", "error", err)
		return
	}
	defer ws.Close(cws.StatusNormalClosure, "")

	ctx := req.Context()

	// Read client's SDP offer.
	_, data, err := ws.Read(ctx)
	if err != nil {
		r.logger.Warn("failed to read client offer", "error", err)
		return
	}

	var offer struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	}
	if err := json.Unmarshal(data, &offer); err != nil {
		r.logger.Warn("invalid client offer", "error", err)
		ws.Close(cws.StatusInvalidFramePayloadData, "expected offer")
		return
	}
	if offer.Type != "offer" {
		r.logger.Warn("unexpected message type from client", "type", offer.Type)
		ws.Close(cws.StatusInvalidFramePayloadData, "expected offer")
		return
	}

	// Create PeerConnection for this client with the relayed track.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		r.logger.Error("failed to create client peer connection", "error", err)
		ws.Close(cws.StatusInternalError, "peer connection failed")
		return
	}

	rtpSender, err := pc.AddTrack(r.localTrack)
	if err != nil {
		pc.Close()
		r.logger.Error("failed to add track to client peer connection", "error", err)
		ws.Close(cws.StatusInternalError, "add track failed")
		return
	}

	// Read and discard RTCP from the client (required by pion).
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(buf); err != nil {
				return
			}
		}
	}()

	// Register state callback before signaling begins so we never miss
	// a terminal transition (e.g. Failed/Closed/Disconnected).
	done := make(chan struct{})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateDisconnected {
			select {
			case <-done:
			default:
				close(done)
			}
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offer.SDP,
	}); err != nil {
		pc.Close()
		r.logger.Error("failed to set client remote description", "error", err)
		ws.Close(cws.StatusInternalError, "sdp failed")
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		r.logger.Error("failed to create answer for client", "error", err)
		ws.Close(cws.StatusInternalError, "answer failed")
		return
	}

	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		r.logger.Error("failed to set local description for client", "error", err)
		ws.Close(cws.StatusInternalError, "local desc failed")
		return
	}

	select {
	case <-gatherDone:
	case <-ctx.Done():
		pc.Close()
		return
	}

	answerMsg, _ := json.Marshal(map[string]string{
		"type": "answer",
		"sdp":  pc.LocalDescription().SDP,
	})
	if err := ws.Write(ctx, cws.MessageText, answerMsg); err != nil {
		pc.Close()
		r.logger.Error("failed to send answer to client", "error", err)
		return
	}

	r.logger.Info("external client connected via WebRTC")

	// Request a keyframe from Neko so the new client gets one immediately.
	r.requestKeyframe()

	// Keep the WebSocket open until the PeerConnection closes, so the
	// client can detect disconnection cleanly.
	select {
	case <-done:
	case <-ctx.Done():
	}
	pc.Close()
}

// Close tears down the relay.
func (r *Relay) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.nekoPC != nil {
		r.nekoPC.Close()
		r.nekoPC = nil
	}
	if r.nekoWS != nil {
		r.nekoWS.Close(cws.StatusGoingAway, "shutdown")
		r.nekoWS = nil
	}
}

// Ready returns a channel that is closed once the relay has received
// the video track from Neko and is ready to serve clients.
func (r *Relay) Ready() <-chan struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ready
}

func (r *Relay) forwardRTP(track *webrtc.TrackRemote) {
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			r.logger.Info("neko track read ended", "error", err)
			return
		}
		if err := r.localTrack.WriteRTP(pkt); err != nil {
			r.logger.Debug("local track write failed", "error", err)
		}
	}
}

func (r *Relay) requestKeyframe() {
	r.mu.RLock()
	pc := r.nekoPC
	r.mu.RUnlock()
	if pc == nil {
		return
	}

	for _, receiver := range pc.GetReceivers() {
		t := receiver.Track()
		if t != nil && t.Kind() == webrtc.RTPCodecTypeVideo {
			_ = pc.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(t.SSRC()),
				},
			})
			return
		}
	}
}

// nekoLogin calls Neko's HTTP login API and returns the bearer token.
func (r *Relay) nekoLogin(ctx context.Context) (string, error) {
	payload, _ := json.Marshal(map[string]string{
		"username": r.cfg.NekoUser,
		"password": r.cfg.NekoPass,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", r.cfg.NekoBaseURL+"/api/login", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("login returned %d", resp.StatusCode)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Token == "" {
		return "", fmt.Errorf("empty token")
	}
	return result.Token, nil
}

// Neko WS message envelope.
type nekoMsg struct {
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func sendWSMsg(ctx context.Context, ws *cws.Conn, event string, payload json.RawMessage) error {
	data, _ := json.Marshal(nekoMsg{Event: event, Payload: payload})
	return ws.Write(ctx, cws.MessageText, data)
}

func (r *Relay) waitForEvent(ctx context.Context, ws *cws.Conn, event string) error {
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		var msg nekoMsg
		if json.Unmarshal(data, &msg) == nil && msg.Event == event {
			return nil
		}
	}
}

func (r *Relay) waitForOffer(ctx context.Context, ws *cws.Conn) (string, error) {
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return "", err
		}
		var msg nekoMsg
		if json.Unmarshal(data, &msg) == nil && msg.Event == "signal/provide" {
			var provide struct {
				SDP string `json:"sdp"`
			}
			if err := json.Unmarshal(msg.Payload, &provide); err != nil {
				return "", fmt.Errorf("parsing signal/provide: %w", err)
			}
			return provide.SDP, nil
		}
	}
}

func (r *Relay) nekoWSLoop(ctx context.Context, ws *cws.Conn, pc *webrtc.PeerConnection) {
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var msg nekoMsg
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Event {
		case "system/heartbeat":
			_ = sendWSMsg(ctx, ws, "client/heartbeat", nil)
		case "signal/candidate":
			var candidate webrtc.ICECandidateInit
			if json.Unmarshal(msg.Payload, &candidate) == nil {
				_ = pc.AddICECandidate(candidate)
			}
		}
	}
}
