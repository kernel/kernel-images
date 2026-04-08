package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/onkernel/kernel-images/server/lib/logger"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
)

// BrowserFetch proxies an HTTP request through Chromium's built-in forward proxy,
// inheriting the browser's TLS fingerprint, cookies, proxy config, and default headers.
// (POST /fetch)
func (s *ApiService) BrowserFetch(ctx context.Context, request oapi.BrowserFetchRequestObject) (oapi.BrowserFetchResponseObject, error) {
	log := logger.FromContext(ctx)

	if request.Body == nil {
		return oapi.BrowserFetch400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}

	// Validate URL
	targetURL := request.Body.Url
	if targetURL == "" {
		return oapi.BrowserFetch400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "url is required"}}, nil
	}
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return oapi.BrowserFetch400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid url: " + err.Error()}}, nil
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return oapi.BrowserFetch400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "url must use http or https scheme"}}, nil
	}

	// Determine method
	method := "GET"
	if request.Body.Method != nil {
		method = string(*request.Body.Method)
	}

	// Determine timeout
	timeoutMs := 30000
	if request.Body.TimeoutMs != nil {
		timeoutMs = *request.Body.TimeoutMs
	}

	// Build request to forward proxy
	proxyURL := fmt.Sprintf("http://localhost:%d/proxy?url=%s", s.forwardProxyPort, url.QueryEscape(targetURL))

	var bodyReader io.Reader
	if request.Body.Body != nil {
		bodyReader = strings.NewReader(*request.Body.Body)
	}

	proxyReq, err := http.NewRequestWithContext(ctx, method, proxyURL, bodyReader)
	if err != nil {
		log.Error("failed to create proxy request", "err", err)
		return oapi.BrowserFetch500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to create request"}}, nil
	}

	// Forward custom headers
	if request.Body.Headers != nil {
		for k, v := range *request.Body.Headers {
			proxyReq.Header.Set(k, v)
		}
	}

	// Execute with timeout
	client := &http.Client{
		Timeout: time.Duration(timeoutMs) * time.Millisecond,
		// Don't follow redirects — let the forward proxy handle them
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	start := time.Now()
	resp, err := client.Do(proxyReq)
	durationMs := int(time.Since(start).Milliseconds())

	if err != nil {
		if ctx.Err() != nil {
			return oapi.BrowserFetch408JSONResponse{Message: "request timed out"}, nil
		}
		// Check if the forward proxy is unreachable
		if isConnectionRefused(err) {
			log.Error("forward proxy unreachable", "err", err)
			return oapi.BrowserFetch502JSONResponse{Message: "forward proxy unreachable (Chromium may have crashed)"}, nil
		}
		log.Error("fetch request failed", "err", err)
		return oapi.BrowserFetch502JSONResponse{Message: "request failed: " + err.Error()}, nil
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error("failed to read response body", "err", err)
		return oapi.BrowserFetch502JSONResponse{Message: "failed to read response body"}, nil
	}

	// Collect response headers
	headers := make(map[string]string)
	for k, v := range resp.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	return oapi.BrowserFetch200JSONResponse{
		Status:     ptrOf(resp.StatusCode),
		Headers:    &headers,
		Body:       ptrOf(string(body)),
		DurationMs: ptrOf(durationMs),
	}, nil
}

// isConnectionRefused checks if the error is a connection refused error,
// indicating the forward proxy is not running.
func isConnectionRefused(err error) bool {
	return strings.Contains(err.Error(), "connection refused") ||
		strings.Contains(err.Error(), "dial tcp")
}
