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
	// Expose /json/version endpoint so clients that attempt to resolve a browser
	// websocket URL via HTTP can succeed. We map the upstream path onto this
	// proxy's host:port so clients connect back to us.
	// Note: Playwright's connectOverCDP requests /json/version/ with trailing slash,
	// so we register both variants to avoid 426 errors from the WebSocket handler.
	jsonVersionHandler := func(w http.ResponseWriter, r *http.Request) {
		current := upstreamMgr.Current()
		if current == "" {
			http.Error(w, "upstream not ready", http.StatusServiceUnavailable)
			return
		}
		proxyWSURL := (&url.URL{Scheme: "ws", Host: r.Host}).String()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"webSocketDebuggerUrl": proxyWSURL,
		})
	}
	rDevtools.Get("/json/version", jsonVersionHandler)
	rDevtools.Get("/json/version/", jsonVersionHandler)

	// Handler for /json and /json/list - proxies to Chrome and rewrites URLs.
	// This is needed for Playwright's connectOverCDP which fetches /json for target discovery.
	jsonTargetHandler := func(w http.ResponseWriter, r *http.Request) {
		current := upstreamMgr.Current()
		if current == "" {
			http.Error(w, "upstream not ready", http.StatusServiceUnavailable)
			return
		}

		// Parse upstream URL to get Chrome's host (e.g., ws://127.0.0.1:9223/...)
		parsed, err := url.Parse(current)
		if err != nil {
			http.Error(w, "invalid upstream URL", http.StatusInternalServerError)
			return
		}

		// Fetch /json from Chrome
		chromeJSONURL := fmt.Sprintf("http://%s/json", parsed.Host)
		resp, err := http.Get(chromeJSONURL)
		if err != nil {
			slogger.Error("failed to fetch /json from Chrome", "err", err, "url", chromeJSONURL)
			http.Error(w, "failed to fetch target list from browser", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Verify Chrome returned a successful response
		if resp.StatusCode != http.StatusOK {
			slogger.Error("Chrome /json returned non-200 status", "status", resp.StatusCode, "url", chromeJSONURL)
			http.Error(w, fmt.Sprintf("browser returned status %d", resp.StatusCode), http.StatusBadGateway)
			return
		}

		// Read and parse the JSON response
		var targets []map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
			slogger.Error("failed to decode /json response", "err", err)
			http.Error(w, "failed to parse target list", http.StatusBadGateway)
			return
		}

		// Rewrite URLs to use this proxy's host instead of Chrome's
		proxyHost := r.Host
		chromeHost := parsed.Host
		for i := range targets {
			// Rewrite webSocketDebuggerUrl
			if wsURL, ok := targets[i]["webSocketDebuggerUrl"].(string); ok {
				targets[i]["webSocketDebuggerUrl"] = rewriteWSURL(wsURL, chromeHost, proxyHost)
			}
			// Rewrite devtoolsFrontendUrl if present
			if frontendURL, ok := targets[i]["devtoolsFrontendUrl"].(string); ok {
				targets[i]["devtoolsFrontendUrl"] = rewriteWSURL(frontendURL, chromeHost, proxyHost)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(targets)
	}
	rDevtools.Get("/json", jsonTargetHandler)
	rDevtools.Get("/json/", jsonTargetHandler)
	rDevtools.Get("/json/list", jsonTargetHandler)
	rDevtools.Get("/json/list/", jsonTargetHandler)

	rDevtools.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		devtoolsproxy.WebSocketProxyHandler(upstreamMgr, slogger, config.LogCDPMessages, stz).ServeHTTP(w, r)
	})

	srvDevtools := &http.Server{
		Addr:    "0.0.0.0:9222",
		Handler: rDevtools,
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

// rewriteWSURL replaces the Chrome host with the proxy host in WebSocket URLs.
// It handles two cases:
// 1. Direct WebSocket URLs: "ws://127.0.0.1:9223/devtools/page/..." -> "ws://127.0.0.1:9222/devtools/page/..."
// 2. DevTools frontend URLs with ws= query param: "https://...?ws=127.0.0.1:9223/..." -> "https://...?ws=127.0.0.1:9222/..."
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
