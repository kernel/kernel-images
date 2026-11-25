// Spike: WHIP ingest -> v4l2loopback camera.
//
// A WebRTC sender POSTs an SDP offer to /whip. The first VP8 video track is
// written as IVF into ffmpeg, which decodes, scales to a fixed size and writes
// raw frames to the v4l2loopback device that Chromium opens as a camera.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/ivfwriter"
)

//go:embed send.html
var sendPage []byte

var (
	listen = flag.String("listen", ":8090", "http listen address")
	device = flag.String("device", "/dev/video0", "v4l2loopback device")
	width  = flag.Int("width", 1280, "output width")
	height = flag.Int("height", 720, "output height")
	fps    = flag.Int("fps", 0, "force output frame rate (0 = pass through sender timing)")
)

type bridge struct {
	mu sync.Mutex
	pc *webrtc.PeerConnection
}

func main() {
	flag.Parse()
	b := &bridge{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /whip", b.handleWHIP)
	mux.HandleFunc("GET /now", func(w http.ResponseWriter, _ *http.Request) {
		cors(w)
		fmt.Fprint(w, strconv.FormatInt(time.Now().UnixMilli(), 10))
	})
	mux.HandleFunc("POST /log", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		log.Printf("sender: %s", b)
	})
	mux.HandleFunc("GET /send", servePage(sendPage))
	log.Printf("listening on %s, device %s", *listen, *device)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func servePage(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(b)
	}
}

func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

func (b *bridge) handleWHIP(w http.ResponseWriter, r *http.Request) {
	cors(w)
	offer, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	b.mu.Lock()
	if b.pc != nil {
		b.pc.Close()
	}
	b.pc = pc
	b.mu.Unlock()

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) { log.Printf("peer connection: %s", s) })
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeVideo {
			return
		}
		log.Printf("video track %s", track.Codec().MimeType)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
		go requestKeyframes(ctx, pc, uint32(track.SSRC()))
		if err := pump(ctx, track); err != nil {
			log.Printf("pump: %v", err)
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offer)}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	<-gathered
	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprint(w, pc.LocalDescription().SDP)
}

func requestKeyframes(ctx context.Context, pc *webrtc.PeerConnection, ssrc uint32) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
		}
	}
}

// pump depacketizes VP8 into IVF on ffmpeg's stdin; ffmpeg writes to the camera.
func pump(ctx context.Context, track *webrtc.TrackRemote) error {
	vf := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,format=yuv420p",
		*width, *height, *width, *height)
	if *fps > 0 {
		vf += fmt.Sprintf(",fps=%d", *fps)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "warning",
		"-fflags", "nobuffer", "-flags", "low_delay", "-probesize", "32", "-analyzeduration", "0",
		"-f", "ivf", "-i", "pipe:0",
		"-vf", vf, "-f", "v4l2", *device)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Wait()
	defer stdin.Close()

	ivf, err := ivfwriter.NewWith(stdin, ivfwriter.WithCodec(webrtc.MimeTypeVP8))
	if err != nil {
		return err
	}
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return err
		}
		if err := ivf.WriteRTP(pkt); err != nil {
			return err
		}
	}
}
