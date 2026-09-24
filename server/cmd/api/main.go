package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
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
	"github.com/kernel/kernel-images/server/lib/pagerecovery"
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

	// Optional S2 storage sink. Constructed here but opened by the telemetry
	// handler, with the first capture session that has storage on, so an
	// instance whose sessions keep storage off never opens an append session.
	// The session binds one stream when it opens, and an instance still holding
	// for a fork identity is carrying the stream of the instance it was forked
	// from, so the resolver hands out no stream until that identity arrives.
	s2Streams := newS2StreamResolver(config, slogger)
	s2Storage := events.NewS2StorageController(eventStream, config.S2Basin, config.S2AccessToken, s2Streams.Resolve, events.S2Config{}, slogger)

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

	// A fork boots carrying the stream of the instance it came from, so the
	// hook records the stream of the identity the guest has taken for the S2
	// writer to bind when the telemetry handler opens it, which the platform
	// does after the handoff. OTLP needs no hook here: its credential resolves
	// per request and its resource attributes at exporter build, and export is
	// turned on per session, likewise after the handoff. An export started
	// before then keeps the source's resource attributes until it is restarted.
	onForkIdentityApplied := func(payload forkidentity.Payload) {
		s2Streams.RecordAppliedPayload(payload)
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
		s2Storage,
	)
	if err != nil {
		slogger.Error("failed to create api service", "err", err)
		os.Exit(1)
	}

	if err := apiService.StartNetworkMonitor(); err != nil {
		slogger.Error("failed to start network monitor", "err", err)
		os.Exit(1)
	}

	// Navigation retry runs on its own CDP connection so it is unaffected by
	// whether customer telemetry is capturing. It stays nil when off, and the
	// metrics below then report zeros and a down gauge rather than disappearing.
	var recoverer *pagerecovery.Recoverer
	if config.PageRecoveryEnabled {
		recoverer = pagerecovery.New(upstreamMgr, pagerecovery.Config{
			MaxAttempts: config.PageRecoveryMaxAttempts,
			Budget:      config.PageRecoveryBudget,
		}, slogger)
		if err := recoverer.Start(context.Background()); err != nil {
			slogger.Error("failed to start page recovery", "err", err)
			os.Exit(1)
		}
	}

	// api_call event emission. Off until the telemetry handlers flip it on.
	r.Use(api.TelemetryHTTPMiddleware(telemetrySession.Publish))
	r.Use(api.WebMCPRequestSizeMiddleware)
	// Enforce additionalProperties: false on POST /repl.
	r.Use(api.StrictBrowserReplBodyMiddleware)
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
		metrics.NewNetworkCollector(apiService.NetworkMetrics),
		metrics.NewPageRecoveryCollector(func() (retries, recovered, exhausted uint64, up bool) {
			if recoverer == nil {
				return 0, 0, 0, false
			}
			snapshot := recoverer.SnapshotMetrics()
			return snapshot.Retries, snapshot.Recovered, snapshot.Exhausted, snapshot.Up
		}),
		metrics.NewChromeCollector(upstreamMgr),
		metrics.NewGPUCollector(),
		metrics.NewSystemCollector(),
		metrics.NewResponseDrainCollector(
			scaletozero.ResponseDrainOutcomeCounts,
			scaletozero.ActiveResponseHolds,
			scaletozero.FailClosedResponseHolds,
			scaletozero.ActiveResponseCloseMonitors,
			scaletozero.ResponseCloseMonitorRejections,
		),
	}
	if otlpMetrics != nil {
		metricsCollectors = append(metricsCollectors, metrics.NewOTLPCollector(otlpMetrics))
	}
	rMetrics.Method(http.MethodGet, "/metrics", metrics.Handler(slogger, metricsCollectors...))
	srvMetrics := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", config.MetricsPort),
		Handler: rMetrics,
	}

	serveHTTP := func(name string, server *http.Server) {
		go func() {
			slogger.Info(name+" starting", "addr", server.Addr)
			listener, err := net.Listen("tcp", server.Addr)
			if err == nil {
				err = scaletozero.Serve(server, listener)
			}
			if err != nil && err != http.ErrServerClosed {
				slogger.Error(name+" failed", "err", err)
				stop()
			}
		}()
	}
	serveHTTP("http server", srv)
	serveHTTP("devtools websocket proxy", srvDevtools)
	serveHTTP("chromedriver proxy", srvChromeDriver)

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
		if recoverer != nil {
			recoverer.Stop()
		}
		return nil
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

	// S2 storage shuts down after the servers above, since they might produce
	// events we want to capture into the stream; we must let them finish before
	// closing the writer. A no-op when no session ever opened it.
	s2StopCtx, s2StopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer s2StopCancel()
	if err := s2Storage.Stop(s2StopCtx); err != nil {
		slogger.Error("s2 storage writer stop failed", "err", err)
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

// s2StreamResolver uses the hook payload once applied and the persisted payload
// after an API process restart. Resolve is called by the S2 controller when the
// telemetry handler opens the sink, which on a fork happens after the handoff.
type s2StreamResolver struct {
	cfg        *config.Config
	log        *slog.Logger
	hookStream atomic.Pointer[string]
}

func newS2StreamResolver(cfg *config.Config, log *slog.Logger) *s2StreamResolver {
	return &s2StreamResolver{cfg: cfg, log: log}
}

// Resolve returns the stream the S2 writer may bind, or "" while the instance
// is still waiting for a fork identity, which keeps the sink closed rather than
// bound to the parent's stream.
func (r *s2StreamResolver) Resolve() string {
	if stream := r.hookStream.Load(); stream != nil {
		return *stream
	}
	stream, pending := appliedS2Stream(r.cfg)
	if pending {
		r.log.Warn("S2 storage not opened: fork identity pending; it opens with the next telemetry config applied after the handoff")
	}
	return stream
}

// RecordAppliedPayload records the applied identity's stream in-process so a
// resolve that follows the handoff does not depend on re-reading the identity
// files. The hook only runs on a fork, whose boot S2_STREAM is the parent's,
// so a payload without a stream leaves storage closed rather than falling back
// to it. It does not block: the handler contract forbids holding up the handoff.
func (r *s2StreamResolver) RecordAppliedPayload(payload forkidentity.Payload) {
	stream := forkidentity.Env(payload)["S2_STREAM"]
	r.hookStream.Store(&stream)
}

// appliedS2Stream resolves the stream the S2 writer should bind, preferring a
// fork identity the guest has already taken over the env this process started
// with. The env belongs to the instance this one was forked from, and it is what
// a restarted api would otherwise bind for the rest of the instance's life.
// While the wait is armed and no identity has been applied there is no stream
// this instance may bind yet, so the result is empty; pending reports that case.
// Once one is applied only its own stream counts, never the boot env's.
//
// OTLP resolves its identity per use instead (otlpIdentityProvider), so a stale
// read there self-corrects; an S2 append session binds once, so this read has to
// be right the first time.
func appliedS2Stream(cfg *config.Config) (stream string, pending bool) {
	// The wrapper clears stale identity state and writes the ready file before
	// starting this process. Its presence distinguishes a fork-wait boot; once
	// the fork is applied, the marker and payload survive API restarts and must
	// take precedence over the seed identity in the boot environment.
	if _, err := os.Stat(forkidentity.ReadyFile); err != nil {
		return cfg.S2Stream, false
	}
	applied, err := forkidentity.ReadAppliedMarker()
	if err != nil || applied == "" {
		return "", true
	}
	payload, err := forkidentity.ReadPayload()
	if err != nil || payload.InstanceName() != applied {
		return "", true
	}
	return forkidentity.Env(payload)["S2_STREAM"], false
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
