package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/ghodss/yaml"
	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"golang.org/x/sync/errgroup"

	serverpkg "github.com/kernel/kernel-images/server"
	"github.com/kernel/kernel-images/server/cmd/api/api"
	"github.com/kernel/kernel-images/server/cmd/config"
	"github.com/kernel/kernel-images/server/lib/chromedriverproxy"
	"github.com/kernel/kernel-images/server/lib/devtoolsproxy"
	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/forkidentity"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/metrics"
	"github.com/kernel/kernel-images/server/lib/nekoclient"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
	"github.com/kernel/kernel-images/server/lib/sysmon"
	"github.com/kernel/kernel-images/server/lib/telemetry"
	"github.com/kernel/kernel-images/server/lib/wsdrain"
)

func main() {
	slogger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Load configuration from environment variables
	config, err := config.Load()
	if err != nil {
		slogger.Error("failed to load configuration", "err", err)
		os.Exit(1)
	}
	slogger.Info("server configuration", "config", config)

	// context cancellation on SIGINT/SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ensure ffmpeg is available
	mustFFmpeg()

	stz := scaletozero.NewDebouncedControllerWithCooldown(scaletozero.NewUnikraftCloudController(), config.ScaleToZeroCooldown)
	r := chi.NewRouter()
	r.Use(
		chiMiddleware.RequestID,
		chiMiddleware.Logger,
		chiMiddleware.Recoverer,
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxWithLogger := logger.AddToContext(r.Context(), slogger)
				next.ServeHTTP(w, r.WithContext(ctxWithLogger))
			})
		},
		scaletozero.Middleware(stz),
	)

	defaultParams := recorder.FFmpegRecordingParams{
		DisplayNum:  &config.DisplayNum,
		FrameRate:   &config.FrameRate,
		MaxSizeInMB: &config.MaxSizeInMB,
		OutputDir:   &config.OutputDir,
		AudioSource: &config.AudioSource,
		PulseServer: &config.PulseServer,
	}
	if err := defaultParams.Validate(); err != nil {
		slogger.Error("invalid default recording parameters", "err", err)
		os.Exit(1)
	}

	// ws conn tracker
	wsRegistry := wsdrain.New()

	// DevTools WebSocket upstream manager: tail Chromium supervisord log
	const chromiumLogPath = "/var/log/supervisord/chromium"
	upstreamMgr := devtoolsproxy.NewUpstreamManager(chromiumLogPath, slogger)
	upstreamMgr.Start(ctx)

	// Initialize Neko authenticated client
	adminPassword := os.Getenv("NEKO_ADMIN_PASSWORD")
	if adminPassword == "" {
		adminPassword = "admin" // Default from neko.yaml
	}
	nekoAuthClient, err := nekoclient.NewAuthClient("http://127.0.0.1:8080", "admin", adminPassword)
	if err != nil {
		slogger.Error("failed to create neko auth client", "err", err)
		os.Exit(1)
	}

	// Construct events pipeline
	// Sized for the control stream's event rate rather than the operational
	// signals it started with: browser-control CDP commands are one event per
	// keystroke and two per click, so a form-filling session produces thousands
	// where a session used to produce tens.
	eventStream, err := events.NewEventStream(events.EventStreamConfig{
		RingCapacity: 8192,
	})
	if err != nil {
		slogger.Error("failed to create event stream", "err", err)
		os.Exit(1)
	}
	telemetrySession := telemetry.NewTelemetrySession(eventStream)

	// VM-internal failure telemetry. OOM kills come from /dev/kmsg here;
	// service_crashed events arrive via POST /telemetry/events from the
	// supervisord-shim child process. Failure to open /dev/kmsg is not
	// fatal — the rest of the API should stay usable without CAP_SYSLOG.
	if err := sysmon.New(telemetrySession.Publish, slogger).Start(ctx); err != nil {
		slogger.Error("sysmon: kmsg OOM monitor disabled", "err", err)
	}

	// Malformed flag: treat as disabled but surface it, matching how the OTLP
	// identity provider handles the same flag. Exiting would crashloop the VM
	// over a provisioning typo.
	forkIdentityWait, err := forkidentity.WaitEnabled()
	if err != nil {
		slogger.Warn("fork-identity wait flag invalid; treating as disabled", "err", err)
	}
	// An instance that already took a fork identity keeps it across a restart of
	// this process, which the env captured at boot does not reflect.
	s2Stream, s2StreamApplied := appliedS2Stream(config)

	// Optional S2 storage sink. The append session is bound to one stream when
	// it starts, and an instance still holding for a fork identity is carrying
	// the stream name of the instance it was forked from, so defer the writer
	// until the identity that owns the events arrives. Opening that session
	// dials with no deadline while holding the writer's lock, so an in-flight
	// start is tracked for shutdown rather than blocking Stop behind the dial.
	var s2Writer atomic.Pointer[events.S2StorageWriter]
	var s2Starting sync.WaitGroup
	startS2Writer := func(streamName string) error {
		if config.S2Basin == "" || config.S2AccessToken == "" || streamName == "" || s2Writer.Load() != nil {
			return nil
		}
		w := events.NewS2StorageWriter(eventStream, config.S2Basin, config.S2AccessToken, streamName, events.S2Config{}, slogger)
		if !s2Writer.CompareAndSwap(nil, w) {
			return nil
		}
		slogger.Info("S2 storage enabled", "basin", config.S2Basin, "stream", streamName)
		if err := w.Start(ctx); err != nil {
			// Leave the slot empty so a later identity can still open a writer.
			s2Writer.CompareAndSwap(w, nil)
			return err
		}
		return nil
	}
	if !forkIdentityWait || s2StreamApplied {
		// An optional sink that cannot open must not take the browser down: the
		// api runs under supervisord with autorestart, so exiting here would
		// crashloop the VM over a misconfigured basin or token.
		if err := startS2Writer(s2Stream); err != nil {
			slogger.Error("failed to start S2 storage writer, continuing without it", "err", err)
		}
	}

	// Optional OTLP export sink. Independent of S2; both can run together.
	// Constructed when an endpoint is provisioned, but left stopped: export is
	// off until turned on per-session via the telemetry API, so a VM that always
	// has the relay injected does not export (and get dropped) by default.
	var otlpExporter api.OTLPExporter
	var otlpMetrics *events.OTLPMetrics
	if config.OTLPEndpoint != "" {
		// The relay authenticates the VM by its instance JWT, sent as a bearer
		// token. Identity (JWT + resource attrs) is resolved dynamically: on a
		// forked VM the boot env is stale, so it is re-read from the applied
		// fork-identity payload (see otlpIdentityProvider).
		identity := newOTLPIdentityProvider(otlpIdentity{
			jwt:          config.InstanceJWT,
			instanceName: config.InstanceName,
			metro:        config.MetroName,
		}, slogger)
		slogger.Info("OTLP export available", "endpoint", config.OTLPEndpoint, "path", config.OTLPPath)
		// The OTel log SDK reports batch-queue drops through its global logger at
		// logr V(1), which slog renders just below Info, so a default Info handler
		// would swallow it. Wire it to a handler that admits that level and counts
		// drops into otlpMetrics, so backpressure is both logged and scrapable.
		otlpMetrics = &events.OTLPMetrics{}
		otelDiag := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo - 1})
		otel.SetLogger(logr.FromSlogHandler(events.NewDropCountingHandler(otelDiag, otlpMetrics)))
		otlpExporter = events.NewOTLPExportController(eventStream, events.OTLPConfig{
			Endpoint:         config.OTLPEndpoint,
			URLPath:          config.OTLPPath,
			Insecure:         config.OTLPInsecure,
			AuthTokenFunc:    identity.Token,
			ServiceName:      config.OTLPServiceName,
			InstanceNameFunc: identity.InstanceName,
			MetroFunc:        identity.Metro,
			MaxQueueSize:     config.OTLPMaxQueueSize,
			ExportInterval:   config.OTLPExportInterval,
			ExportTimeout:    config.OTLPExportTimeout,
			Metrics:          otlpMetrics,
		}, slogger)
	}

	// A fork boots carrying the stream of the instance it came from, and the S2
	// writer binds a stream when it starts, so it is opened here once the guest
	// has taken an identity of its own. OTLP needs no hook here: its credential
	// resolves per request and its resource attributes at exporter build, and
	// export is turned on per session, which the platform does after the
	// handoff. An export started before then keeps the source's resource
	// attributes until it is restarted.
	onForkIdentityApplied := func(payload forkidentity.Payload) {
		// Opening the S2 append session dials the network with no deadline, so
		// it runs off the handoff's critical path.
		stream := forkidentity.FirstNonEmpty(forkidentity.Env(payload)["S2_STREAM"], config.S2Stream)
		s2Starting.Add(1)
		go func() {
			defer s2Starting.Done()
			if err := startS2Writer(stream); err != nil {
				slogger.Error("failed to start S2 storage writer for fork identity", "err", err)
			}
		}()
	}

	apiService, err := api.New(
		recorder.NewFFmpegManager(),
		recorder.NewFFmpegRecorderFactory(config.PathToFFmpeg, defaultParams, stz),
		upstreamMgr,
		stz,
		nekoAuthClient,
		telemetrySession,
		eventStream,
		config.DisplayNum,
		otlpExporter,
	)
	if err != nil {
		slogger.Error("failed to create api service", "err", err)
		os.Exit(1)
	}

	// api_call event emission. Off until the telemetry handlers flip it on.
	r.Use(api.TelemetryHTTPMiddleware(telemetrySession.Publish))
	r.Use(api.WebMCPRequestSizeMiddleware)
	strictHandler := oapi.NewStrictHandlerWithOptions(apiService, []oapi.StrictMiddlewareFunc{
		api.TelemetryStrictMiddleware(),
	}, oapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  api.StrictRequestErrorHandler,
		ResponseErrorHandlerFunc: api.StrictResponseErrorHandler,
	})
	oapi.HandlerFromMux(strictHandler, r)

	// Fork identity endpoints - not part of OpenAPI spec.
	r.Post("/internal/fork-identity", forkIdentityHandler(slogger, onForkIdentityApplied))
	r.Get("/internal/fork-identity/config", forkIdentityConfigHandler(slogger))

	// endpoints to expose the spec
	r.Get("/spec.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oai.openapi")
		w.Write(serverpkg.OpenAPIYAML)
	})
	r.Get("/spec.json", func(w http.ResponseWriter, r *http.Request) {
		jsonData, err := yaml.YAMLToJSON(serverpkg.OpenAPIYAML)
		if err != nil {
			http.Error(w, "failed to convert YAML to JSON", http.StatusInternalServerError)
			logger.FromContext(r.Context()).Error("failed to convert YAML to JSON", "err", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(jsonData)
	})
	// PTY attach endpoint (WebSocket) - not part of OpenAPI spec
	// Uses WebSocket for bidirectional streaming, which works well through proxies.
	r.Get("/process/{process_id}/attach", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "process_id")
		apiService.HandleProcessAttachWS(w, r, id, wsRegistry)
	})

	// Serve extension files for Chrome policy-installed extensions
	// This allows Chrome to download .crx and update.xml files via HTTP
	extensionsDir := "/home/kernel/extensions"
	r.Get("/extensions/*", func(w http.ResponseWriter, r *http.Request) {
		// Serve files from /home/kernel/extensions/
		fs := http.StripPrefix("/extensions/", http.FileServer(http.Dir(extensionsDir)))
		fs.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", config.Port),
		Handler: r,
	}

	// wait up to 10 seconds for initial upstream; exit nonzero if not found
	if _, err := upstreamMgr.WaitForInitial(10 * time.Second); err != nil {
		slogger.Error("devtools upstream not available", "err", err)
		os.Exit(1)
	}

	rDevtools := chi.NewRouter()
	rDevtools.Use(
		chiMiddleware.Logger,
		chiMiddleware.Recoverer,
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxWithLogger := logger.AddToContext(r.Context(), slogger)
				next.ServeHTTP(w, r.WithContext(ctxWithLogger))
			})
		},
		scaletozero.Middleware(stz),
	)
	// Proxy /json/version and /json/list to upstream Chrome with URL rewriting.
	// Playwright's connectOverCDP requests these with trailing slashes,
	// so we register both variants.
	jsonVersionHandler := chromeJSONProxyHandler(upstreamMgr, slogger, "/json/version")
	rDevtools.Get("/json/version", jsonVersionHandler)
	rDevtools.Get("/json/version/", jsonVersionHandler)

	jsonTargetHandler := chromeJSONProxyHandler(upstreamMgr, slogger, "/json")
	rDevtools.Get("/json", jsonTargetHandler)
	rDevtools.Get("/json/", jsonTargetHandler)
	rDevtools.Get("/json/list", jsonTargetHandler)
	rDevtools.Get("/json/list/", jsonTargetHandler)
	// Checked once per forwarded client frame, so it reads the session's
	// lock-free view rather than taking the telemetry lock.
	controlEnabled := func() bool { return telemetrySession.CategoryEnabled(events.Control) }
	rDevtools.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		devtoolsproxy.WebSocketProxyHandler(upstreamMgr, slogger, config.LogCDPMessages, stz, telemetrySession.Publish, controlEnabled, telemetrySession.ExcludedCdpMethods, wsRegistry).ServeHTTP(w, r)
	})

	srvDevtools := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", config.DevToolsProxyPort),
		Handler: rDevtools,
	}

	// ChromeDriver proxy: intercepts POST /session to inject the DevTools proxy
	// address as goog:chromeOptions.debuggerAddress,
	// proxies WebSocket (BiDi) and all other HTTP to the internal ChromeDriver.
	rChromeDriver := chi.NewRouter()
	rChromeDriver.Use(
		chiMiddleware.Logger,
		chiMiddleware.Recoverer,
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxWithLogger := logger.AddToContext(r.Context(), slogger)
				next.ServeHTTP(w, r.WithContext(ctxWithLogger))
			})
		},
		scaletozero.Middleware(stz),
	)
	rChromeDriver.Handle("/*", chromedriverproxy.Handler(slogger, &chromedriverproxy.Options{
		ChromeDriverUpstream: config.ChromeDriverUpstreamAddr,
		DevToolsProxyAddr:    config.DevToolsProxyAddr,
		Registry:             wsRegistry,
	}))

	srvChromeDriver := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", config.ChromeDriverProxyPort),
		Handler: rChromeDriver,
	}

	// Prometheus metrics for external collection. Served on its own
	// listener, deliberately without the scale-to-zero middleware (periodic
	// scrapes must not count as session activity) and without per-request
	// logging (scrapes would drown the logs).
	rMetrics := chi.NewRouter()
	rMetrics.Use(chiMiddleware.Recoverer)
	metricsCollectors := []metrics.Collector{
		metrics.NewChromeCollector(upstreamMgr),
		metrics.NewGPUCollector(),
		metrics.NewSystemCollector(),
	}
	if otlpMetrics != nil {
		metricsCollectors = append(metricsCollectors, metrics.NewOTLPCollector(otlpMetrics))
	}
	rMetrics.Method(http.MethodGet, "/metrics", metrics.Handler(slogger, metricsCollectors...))
	srvMetrics := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", config.MetricsPort),
		Handler: rMetrics,
	}

	go func() {
		slogger.Info("http server starting", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slogger.Error("http server failed", "err", err)
			stop()
		}
	}()

	go func() {
		slogger.Info("devtools websocket proxy starting", "addr", srvDevtools.Addr)
		if err := srvDevtools.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slogger.Error("devtools websocket proxy failed", "err", err)
			stop()
		}
	}()

	go func() {
		slogger.Info("chromedriver proxy starting", "addr", srvChromeDriver.Addr)
		if err := srvChromeDriver.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slogger.Error("chromedriver proxy failed", "err", err)
			stop()
		}
	}()

	go func() {
		slogger.Info("metrics server starting", "addr", srvMetrics.Addr)
		if err := srvMetrics.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slogger.Error("metrics server failed", "err", err)
			stop()
		}
	}()

	// graceful shutdown
	<-ctx.Done()
	slogger.Info("shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	g, _ := errgroup.WithContext(shutdownCtx)

	g.Go(func() error {
		return srv.Shutdown(shutdownCtx)
	})
	g.Go(func() error {
		return apiService.Shutdown(shutdownCtx)
	})
	g.Go(func() error {
		if n := wsRegistry.CloseAll(websocket.StatusGoingAway, "browser shutting down"); n > 0 {
			slogger.Info("closed active websocket connections for shutdown", "count", n)
		}
		return nil
	})
	g.Go(func() error {
		upstreamMgr.Stop()
		return srvDevtools.Shutdown(shutdownCtx)
	})
	g.Go(func() error {
		return srvChromeDriver.Shutdown(shutdownCtx)
	})
	g.Go(func() error {
		return srvMetrics.Shutdown(shutdownCtx)
	})

	if err := g.Wait(); err != nil {
		slogger.Error("server failed to shutdown", "err", err)
	}

	// s2Writer shuts down after the servers above, since they might produce events we
	// want to capture into the stream; we must let them finish before closing the writer.
	// Stop takes the same lock the unbounded append-session dial holds, so only
	// drain once a start that is still opening has finished. Skipping the drain
	// loses at most the events of a writer that was never serving.
	s2StartSettled := make(chan struct{})
	go func() {
		s2Starting.Wait()
		close(s2StartSettled)
	}()
	select {
	case <-s2StartSettled:
		if w := s2Writer.Load(); w != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer stopCancel()
			if err := w.Stop(stopCtx); err != nil {
				slogger.Error("s2 storage writer stop failed", "err", err)
			}
		}
	case <-time.After(2 * time.Second):
		slogger.Warn("s2 storage writer still opening at shutdown, skipping drain")
	}

	// Likewise stop OTLP export after the servers drain (a no-op if the toggle
	// left it off), so shutdown-window events are exported rather than dropped.
	if otlpExporter != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		if err := otlpExporter.Stop(stopCtx); err != nil {
			slogger.Error("otlp export stop failed", "err", err)
		}
	}

}

func mustFFmpeg() {
	cmd := exec.Command("ffmpeg", "-version")
	if err := cmd.Run(); err != nil {
		panic(fmt.Errorf("ffmpeg not found or not executable: %w", err))
	}
}

// appliedS2Stream resolves the stream the S2 writer should bind, preferring a
// fork identity the guest has already taken over the env this process started
// with. The env belongs to the instance this one was forked from, and it is what
// a restarted api would otherwise bind for the rest of the instance's life.
// Reports whether an applied fork identity supplied it.
//
// OTLP resolves its identity per use instead (otlpIdentityProvider), so a stale
// read there self-corrects; an S2 append session binds once, so this read has to
// be right the first time.
func appliedS2Stream(cfg *config.Config) (string, bool) {
	stream := cfg.S2Stream

	// The wrapper clears stale identity state and writes the ready file before
	// starting this process. Its presence distinguishes a fork-wait boot; once
	// the fork is applied, the marker and payload survive API restarts and must
	// take precedence over the seed identity in the boot environment.
	if _, err := os.Stat(forkidentity.ReadyFile); err != nil {
		return stream, false
	}
	applied, err := forkidentity.ReadAppliedMarker()
	if err != nil || applied == "" {
		return stream, false
	}
	payload, err := forkidentity.ReadPayload()
	if err != nil || payload.InstanceName() != applied {
		return stream, false
	}
	return forkidentity.FirstNonEmpty(forkidentity.Env(payload)["S2_STREAM"], stream), true
}

// chromeJSONProxyHandler returns a handler that proxies a JSON endpoint from
// Chrome's DevTools API and rewrites WebSocket/DevTools URLs to point to this proxy.
func chromeJSONProxyHandler(upstreamMgr *devtoolsproxy.UpstreamManager, slogger *slog.Logger, chromePath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		current := upstreamMgr.Current()
		if current == "" {
			http.Error(w, "upstream not ready", http.StatusServiceUnavailable)
			return
		}

		parsed, err := url.Parse(current)
		if err != nil {
			http.Error(w, "invalid upstream URL", http.StatusInternalServerError)
			return
		}

		chromeURL := fmt.Sprintf("http://%s%s", parsed.Host, chromePath)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, chromeURL, nil)
		if err != nil {
			slogger.Error("failed to build Chrome request", "err", err, "url", chromeURL)
			http.Error(w, "failed to build browser request", http.StatusInternalServerError)
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			slogger.Error("failed to fetch from Chrome", "err", err, "url", chromeURL)
			http.Error(w, "failed to fetch from browser", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			slogger.Error("Chrome returned non-200 status", "status", resp.StatusCode, "url", chromeURL)
			http.Error(w, fmt.Sprintf("browser returned status %d", resp.StatusCode), http.StatusBadGateway)
			return
		}

		var raw interface{}
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			slogger.Error("failed to decode Chrome JSON response", "err", err, "path", chromePath)
			http.Error(w, "failed to parse browser response", http.StatusBadGateway)
			return
		}

		rewriteChromeURLs(raw, parsed.Host, r.Host)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(raw)
	}
}

var chromeURLFields = []string{"webSocketDebuggerUrl", "devtoolsFrontendUrl"}

func rewriteChromeURLs(v interface{}, chromeHost, proxyHost string) {
	switch val := v.(type) {
	case map[string]interface{}:
		for _, field := range chromeURLFields {
			if s, ok := val[field].(string); ok {
				val[field] = rewriteWSURL(s, chromeHost, proxyHost)
			}
		}
		for _, nested := range val {
			rewriteChromeURLs(nested, chromeHost, proxyHost)
		}
	case []interface{}:
		for _, item := range val {
			rewriteChromeURLs(item, chromeHost, proxyHost)
		}
	}
}

// rewriteWSURL replaces the Chrome host with the proxy host in WebSocket URLs.
// It handles two cases:
// 1. Direct WebSocket URLs: ws://chrome-host/devtools/... -> ws://proxy-host/devtools/...
// 2. DevTools frontend URLs with ws= query param: ...?ws=chrome-host/devtools/... -> ...?ws=proxy-host/devtools/...
func rewriteWSURL(urlStr, chromeHost, proxyHost string) string {
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return urlStr
	}

	// Case 1: Direct replacement if the URL's host matches Chrome's host
	if parsed.Host == chromeHost {
		parsed.Host = proxyHost
	}

	// Case 2: Check for ws= query parameter (used in devtoolsFrontendUrl)
	// e.g., https://chrome-devtools-frontend.appspot.com/.../inspector.html?ws=127.0.0.1:9223/devtools/page/...
	if wsParam := parsed.Query().Get("ws"); wsParam != "" {
		// The ws param value is like "127.0.0.1:9223/devtools/page/..."
		// We need to replace the host portion
		if strings.HasPrefix(wsParam, chromeHost) {
			newWsParam := strings.Replace(wsParam, chromeHost, proxyHost, 1)
			q := parsed.Query()
			q.Set("ws", newWsParam)
			parsed.RawQuery = q.Encode()
		}
	}

	return parsed.String()
}
