// Command otlpbench is the shared benchmark rig for the OTLP delivery-substrate
// bake-off. It is substrate-independent: every candidate feeds the same
// converter and is measured by the same receiver.
//
//	# terminal 1: measurement receiver (counts records, measures end-to-end lag)
//	go run ./cmd/otlpbench -mode=receiver -addr=:4318 [-delay=50ms]
//
//	# terminal 2: relay-shape load driver (EventStream -> OTLPStorageWriter -> receiver)
//	go run ./cmd/otlpbench -mode=relay -endpoint=127.0.0.1:4318 -path=/v1/logs \
//	    -rate=500 -dur=10s -profile=heavy
//
// The server-side substrates (Temporal activity, claim goroutine) reuse the
// same receiver and workload profiles; only the driver differs.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

func main() {
	mode := flag.String("mode", "receiver", "receiver | relay")
	addr := flag.String("addr", ":4318", "receiver listen address")
	endpoint := flag.String("endpoint", "127.0.0.1:4318", "driver target host[:port]")
	path := flag.String("path", "/v1/logs", "driver target path")
	delay := flag.Duration("delay", 0, "receiver: artificial per-request delay (simulate slow customer)")
	rate := flag.Int("rate", 500, "driver: events per second")
	dur := flag.Duration("dur", 10*time.Second, "driver: run duration")
	profile := flag.String("profile", "light", "driver: light | heavy")
	readLatency := flag.Duration("read-latency", 0, "driver: simulated S2 read-hop latency; models the server-side pusher substrates (shapes 2/3)")
	pauseAt := flag.Duration("pause-at", 0, "driver: when to take the delivery path down (fault injection)")
	pauseFor := flag.Duration("pause-for", 0, "driver: how long the delivery path stays down")
	flag.Parse()

	fault := fault{at: *pauseAt, dur: *pauseFor}
	switch *mode {
	case "receiver":
		runReceiver(*addr, *delay)
	case "relay":
		runRelay(*endpoint, *path, *profile, *rate, *dur, *readLatency, fault)
	case "durable":
		runDurable(*endpoint, *path, *profile, *rate, *dur, fault)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", *mode)
		os.Exit(2)
	}
}

func runReceiver(addr string, delay time.Duration) {
	var (
		mu        sync.Mutex
		total     int
		byteCount int64
		lags      []time.Duration
		seen      = map[int64]struct{}{}
	)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		data := body
		if r.Header.Get("Content-Encoding") == "gzip" {
			if gz, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
				if d, err := io.ReadAll(gz); err == nil {
					data = d
				}
			}
		}
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			http.Error(w, "bad protobuf", http.StatusBadRequest)
			return
		}
		now := time.Now().UnixNano()
		mu.Lock()
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					total++
					if lr.TimeUnixNano > 0 {
						lags = append(lags, time.Duration(now-int64(lr.TimeUnixNano)))
					}
					for _, kv := range lr.Attributes {
						if kv.Key == "kernel.event.seq" {
							seen[kv.Value.GetIntValue()] = struct{}{}
						}
					}
				}
			}
		}
		byteCount += int64(len(data))
		mu.Unlock()

		if delay > 0 {
			time.Sleep(delay)
		}
		resp, _ := proto.Marshal(&collogspb.ExportLogsServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(resp)
	})

	srv := &http.Server{Addr: addr}
	go func() {
		last := 0
		for range time.Tick(time.Second) {
			mu.Lock()
			n := total
			mu.Unlock()
			fmt.Printf("received total=%d (+%d/s)\n", n, n-last)
			last = n
		}
	}()

	go func() {
		fmt.Printf("otlpbench receiver listening on %s (delay=%s)\n", addr, delay)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	mu.Lock()
	defer mu.Unlock()
	fmt.Println("\n=== otlpbench receiver summary ===")
	fmt.Printf("records:     %d\n", total)
	fmt.Printf("unique:      %d\n", len(seen))
	fmt.Printf("duplicates:  %d\n", total-len(seen))
	fmt.Printf("bytes:       %d\n", byteCount)
	fmt.Printf("lag p50:     %s\n", pct(lags, 0.50))
	fmt.Printf("lag p99:     %s\n", pct(lags, 0.99))
}

// fault describes a delivery-path outage injected mid-run.
type fault struct {
	at  time.Duration
	dur time.Duration
}

func (f fault) active(elapsed time.Duration) bool {
	return f.dur > 0 && elapsed >= f.at && elapsed < f.at+f.dur
}

func newWriter(es *events.EventStream, endpoint, path string) *events.OTLPStorageWriter {
	return events.NewOTLPStorageWriter(es, events.OTLPConfig{
		Endpoint: endpoint, URLPath: path, Insecure: true,
		ServiceName: "kernel-browser", InstanceName: "bench", Metro: "dev-local",
	}, slog.Default())
}

func runRelay(endpoint, path, profile string, rate int, dur time.Duration, readLatency time.Duration, f fault) {
	// The relay's source is the volatile in-VM ring (1024 slots). While the
	// delivery path is down it keeps filling and overflows; those records are
	// lost with no replay.
	es, err := events.NewEventStream(events.EventStreamConfig{RingCapacity: 1024})
	if err != nil {
		panic(err)
	}
	wctx, wcancel := context.WithCancel(context.Background())
	writer := newWriter(es, endpoint, path)
	if err := writer.Start(wctx); err != nil {
		panic(err)
	}

	const tick = 10 * time.Millisecond
	perTick := rate * int(tick) / int(time.Second)
	if perTick < 1 {
		perTick = 1
	}
	published := 0
	start := time.Now()
	deadline := start.Add(dur)
	paused, resumed := false, false
	t := time.NewTicker(tick)
	defer t.Stop()
	for now := range t.C {
		if !now.Before(deadline) {
			break
		}
		elapsed := now.Sub(start)
		if f.active(elapsed) && !paused {
			wcancel()
			sc, c := context.WithTimeout(context.Background(), 2*time.Second)
			_ = writer.Stop(sc)
			c()
			paused = true
			fmt.Printf("  [fault] relay delivery DOWN at %s\n", elapsed.Round(time.Millisecond))
		}
		if paused && !resumed && elapsed >= f.at+f.dur {
			wctx, wcancel = context.WithCancel(context.Background())
			writer = newWriter(es, endpoint, path)
			_ = writer.Start(wctx)
			resumed = true
			fmt.Printf("  [fault] relay delivery RESTORED at %s\n", elapsed.Round(time.Millisecond))
		}
		for i := 0; i < perTick; i++ {
			env := makeEnvelope(profile, published)
			if readLatency > 0 {
				time.AfterFunc(readLatency, func() { es.Publish(env) })
			} else {
				es.Publish(env)
			}
			published++
		}
	}
	if readLatency > 0 {
		time.Sleep(readLatency + 200*time.Millisecond)
	}

	wcancel()
	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = writer.Stop(stopCtx)

	shape := "relay (in-VM, best-effort)"
	if readLatency > 0 {
		shape = fmt.Sprintf("server-side pusher (modeled, read-latency=%s)", readLatency)
	}
	fmt.Printf("%s: published=%d over %s (target %d/s, profile=%s)\n", shape, published, dur, rate, profile)
}

// runDurable models the server-side durable pusher: a producer appends every
// record to an unbounded persistent log (this models the existing reliable
// VM->S2 write), and a pusher reads from a cursor and delivers. While the
// delivery path is down the log keeps growing, so on resume the pusher catches
// up from the cursor and loses nothing.
func runDurable(endpoint, path, profile string, rate int, dur time.Duration, f fault) {
	es, err := events.NewEventStream(events.EventStreamConfig{RingCapacity: 262144})
	if err != nil {
		panic(err)
	}
	wctx, wcancel := context.WithCancel(context.Background())
	writer := newWriter(es, endpoint, path)
	if err := writer.Start(wctx); err != nil {
		panic(err)
	}

	const tick = 10 * time.Millisecond
	perTick := rate * int(tick) / int(time.Second)
	if perTick < 1 {
		perTick = 1
	}

	var (
		mu       sync.Mutex
		logq     []events.Envelope
		produced int
	)
	start := time.Now()
	prodDone := make(chan struct{})
	go func() {
		defer close(prodDone)
		t := time.NewTicker(tick)
		defer t.Stop()
		deadline := start.Add(dur)
		for now := range t.C {
			if !now.Before(deadline) {
				return
			}
			mu.Lock()
			for i := 0; i < perTick; i++ {
				logq = append(logq, makeEnvelope(profile, produced))
				produced++
			}
			mu.Unlock()
		}
	}()

	cursor := 0
	producing := true
	loggedDown := false
	for producing || func() bool { mu.Lock(); defer mu.Unlock(); return cursor < len(logq) }() {
		elapsed := time.Since(start)
		if f.active(elapsed) {
			if !loggedDown {
				fmt.Printf("  [fault] pusher delivery DOWN at %s (log keeps growing)\n", elapsed.Round(time.Millisecond))
				loggedDown = true
			}
			time.Sleep(tick)
			goto checkProd
		}
		if loggedDown {
			fmt.Printf("  [fault] pusher RESUMED at %s, catching up from cursor\n", elapsed.Round(time.Millisecond))
			loggedDown = false
		}
		for {
			mu.Lock()
			if cursor >= len(logq) {
				mu.Unlock()
				break
			}
			env := logq[cursor]
			mu.Unlock()
			es.Publish(env)
			cursor++
		}
	checkProd:
		select {
		case <-prodDone:
			producing = false
		default:
			time.Sleep(tick)
		}
	}

	wcancel()
	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = writer.Stop(stopCtx)
	fmt.Printf("durable pusher (at-least-once): produced=%d delivered-from-log=%d (profile=%s)\n", produced, cursor, profile)
}

var heavyBody = `{"status":200,"url":"https://example.com/api","mime_type":"application/json","body":"` +
	stringOfLen(8*1024) + `"}`

func makeEnvelope(profile string, i int) events.Envelope {
	meta := map[string]string{"telemetry_session_id": "bench", "cdp_session_id": "s1", "target_id": "t1"}
	ev := events.Event{
		Ts:     time.Now().UnixMicro(),
		Source: oapi.BrowserEventSource{Kind: oapi.BrowserEventSourceKind("cdp"), Metadata: &meta},
	}
	if profile == "heavy" {
		ev.Type = "network_response"
		ev.Category = oapi.TelemetryEventCategory("network")
		ev.Data = []byte(heavyBody)
	} else {
		ev.Type = "page_navigation"
		ev.Category = oapi.TelemetryEventCategory("page")
		ev.Data = []byte(fmt.Sprintf(`{"url":"https://example.com/p/%d","frame_id":"F1"}`, i))
	}
	return events.Envelope{Event: ev}
}

func stringOfLen(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[int(float64(len(d)-1)*p)]
}
