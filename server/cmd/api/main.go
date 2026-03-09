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
	"syscall"
	"time"

	"github.com/ghodss/yaml"
	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"golang.org/x/sync/errgroup"

	serverpkg "github.com/onkernel/kernel-images/server"
	"github.com/onkernel/kernel-images/server/cmd/api/api"
	"github.com/onkernel/kernel-images/server/cmd/config"
	"github.com/onkernel/kernel-images/server/lib/chromedriverproxy"
	"github.com/onkernel/kernel-images/server/lib/devtoolsproxy"
	"github.com/onkernel/kernel-images/server/lib/logger"
	"github.com/onkernel/kernel-images/server/lib/nekoclient"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/onkernel/kernel-images/server/lib/recorder"
	"github.com/onkernel/kernel-images/server/lib/scaletozero"
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

	stz := scaletozero.NewDebouncedController(scaletozero.NewUnikraftCloudController())
	r := chi.NewRouter()
	r.Use(
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
	}
	if err := defaultParams.Validate(); err != nil {
		slogger.Error("invalid default recording parameters", "err", err)
		os.Exit(1)
	}

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

	apiService, err := api.New(
		recorder.NewFFmpegManager(),
		recorder.NewFFmpegRecorderFactory(config.PathToFFmpeg, defaultParams, stz),
		upstreamMgr,
		stz,
		nekoAuthClient,
	)
	if err != nil {
		slogger.Error("failed to create api service", "err", err)
		os.Exit(1)
	}

	strictHandler := oapi.NewStrictHandler(apiService, nil)
	oapi.HandlerFromMux(strictHandler, r)

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
		apiService.HandleProcessAttachWS(w, r, id)
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
	rDevtools.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		devtoolsproxy.WebSocketProxyHandler(upstreamMgr, slogger, config.LogCDPMessages, stz).ServeHTTP(w, r)
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
	}))

	srvChromeDriver := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", config.ChromeDriverProxyPort),
		Handler: rChromeDriver,
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
		upstreamMgr.Stop()
		return srvDevtools.Shutdown(shutdownCtx)
	})
	g.Go(func() error {
		return srvChromeDriver.Shutdown(shutdownCtx)
	})

	if err := g.Wait(); err != nil {
		slogger.Error("server failed to shutdown", "err", err)
	}
}

func mustFFmpeg() {
	cmd := exec.Command("ffmpeg", "-version")
	if err := cmd.Run(); err != nil {
		panic(fmt.Errorf("ffmpeg not found or not executable: %w", err))
	}
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
