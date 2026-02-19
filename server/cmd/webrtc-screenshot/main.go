package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image/jpeg"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	cws "github.com/coder/websocket"
	"github.com/onkernel/kernel-images/server/lib/vpxdecoder"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
)

func main() {
	serverURL := flag.String("server", "ws://127.0.0.1:10001/display/webrtc", "WebRTC signaling WebSocket URL")
	outputPath := flag.String("output", "/tmp/screen.jpg", "Path to write JPEG screenshots")
	quality := flag.Int("quality", 85, "JPEG quality (1-100)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fb := &frameBuffer{
		path:    *outputPath,
		quality: *quality,
		logger:  logger,
	}

	for {
		err := run(ctx, logger, *serverURL, fb)
		if ctx.Err() != nil {
			logger.Info("shutting down")
			return
		}
		logger.Warn("connection lost, reconnecting in 2s", "error", err)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

func run(ctx context.Context, logger *slog.Logger, serverURL string, fb *frameBuffer) error {
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Connect to signaling WebSocket.
	ws, _, err := cws.Dial(connectCtx, serverURL, nil)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer ws.Close(cws.StatusGoingAway, "done")

	// Create PeerConnection.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return fmt.Errorf("new peer connection: %w", err)
	}
	defer pc.Close()

	// We want to receive video only.
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		return fmt.Errorf("add transceiver: %w", err)
	}

	trackCh := make(chan *webrtc.TrackRemote, 1)
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() == webrtc.RTPCodecTypeVideo {
			select {
			case trackCh <- track:
			default:
			}
		}
	})

	disconnected := make(chan struct{})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Info("peer connection state", "state", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			select {
			case <-disconnected:
			default:
				close(disconnected)
			}
		}
	})

	// Create offer (with all ICE candidates gathered).
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	gatherDone := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local desc: %w", err)
	}
	select {
	case <-gatherDone:
	case <-connectCtx.Done():
		return connectCtx.Err()
	}

	// Send offer to server.
	offerMsg, _ := json.Marshal(map[string]string{
		"type": "offer",
		"sdp":  pc.LocalDescription().SDP,
	})
	if err := ws.Write(connectCtx, cws.MessageText, offerMsg); err != nil {
		return fmt.Errorf("send offer: %w", err)
	}

	// Receive answer.
	_, answerData, err := ws.Read(connectCtx)
	if err != nil {
		return fmt.Errorf("read answer: %w", err)
	}
	var answer struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	}
	if err := json.Unmarshal(answerData, &answer); err != nil {
		return fmt.Errorf("invalid answer: %w", err)
	}
	if answer.Type != "answer" {
		return fmt.Errorf("invalid answer: unexpected type %q", answer.Type)
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer.SDP,
	}); err != nil {
		return fmt.Errorf("set remote desc: %w", err)
	}

	logger.Info("WebRTC connected, waiting for video track")

	// Wait for video track.
	var track *webrtc.TrackRemote
	select {
	case track = <-trackCh:
		logger.Info("video track received",
			"codec", track.Codec().MimeType,
			"ssrc", track.SSRC(),
		)
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for video track")
	case <-ctx.Done():
		return ctx.Err()
	}

	// Decode loop: depacketize VP8, decode every frame, write JPEG.
	return fb.decodeLoop(ctx, track, disconnected)
}

// frameBuffer holds the VP8 decoder state and handles writing JPEGs.
type frameBuffer struct {
	path    string
	quality int
	logger  *slog.Logger

	mu        sync.RWMutex
	lastWrite time.Time
	frames    int64
}

func (fb *frameBuffer) decodeLoop(ctx context.Context, track *webrtc.TrackRemote, disconnected <-chan struct{}) error {
	dec, err := vpxdecoder.New()
	if err != nil {
		return fmt.Errorf("vpx decoder init: %w", err)
	}
	defer dec.Close()

	var (
		frameBuf     bytes.Buffer
		frameStarted bool
	)

	statsStart := time.Now()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-disconnected:
			return fmt.Errorf("peer connection lost")
		default:
		}

		pkt, _, err := track.ReadRTP()
		if err != nil {
			return fmt.Errorf("read rtp: %w", err)
		}

		// Depacketize VP8 from RTP.
		vp8Pkt := &codecs.VP8Packet{}
		payload, err := vp8Pkt.Unmarshal(pkt.Payload)
		if err != nil {
			continue
		}

		// S=1 + PID=0 → start of new frame.
		if vp8Pkt.S == 1 && vp8Pkt.PID == 0 {
			frameBuf.Reset()
			frameStarted = true
		}

		if !frameStarted {
			continue
		}

		frameBuf.Write(payload)

		// Marker bit → last packet of frame.
		if !pkt.Marker {
			continue
		}
		frameStarted = false

		if frameBuf.Len() == 0 {
			continue
		}

		img, err := dec.Decode(frameBuf.Bytes())
		if err != nil {
			fb.logger.Debug("decode failed", "error", err, "size", frameBuf.Len())
			continue
		}

		var jpegBuf bytes.Buffer
		if err := jpeg.Encode(&jpegBuf, img, &jpeg.Options{Quality: fb.quality}); err != nil {
			fb.logger.Warn("jpeg encode failed", "error", err)
			continue
		}

		fb.writeToFile(jpegBuf.Bytes())

		fb.mu.Lock()
		fb.frames++
		count := fb.frames
		fb.mu.Unlock()

		if count%100 == 0 {
			elapsed := time.Since(statsStart)
			fb.logger.Info("frame stats",
				"frames", count,
				"fps", fmt.Sprintf("%.1f", float64(count)/elapsed.Seconds()),
				"size_kb", jpegBuf.Len()/1024,
				"resolution", fmt.Sprintf("%dx%d", img.Rect.Dx(), img.Rect.Dy()),
			)
		}
	}
}

func (fb *frameBuffer) writeToFile(data []byte) {
	dir := filepath.Dir(fb.path)
	tmp, err := os.CreateTemp(dir, ".screenshot-*.tmp")
	if err != nil {
		fb.logger.Warn("failed to create temp file", "error", err)
		return
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, fb.path); err != nil {
		fb.logger.Warn("rename failed", "error", err)
		os.Remove(tmpName)
		return
	}

	fb.mu.Lock()
	fb.lastWrite = time.Now()
	fb.mu.Unlock()
}

// http handler to serve the latest screenshot (optional, for debugging)
func (fb *frameBuffer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(fb.path)
	if err != nil {
		http.Error(w, "no screenshot yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}
