package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

func tcpOK(host, port string) bool {
	c, err := net.DialTimeout("tcp4", net.JoinHostPort(host, port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

var probeClient = &http.Client{Timeout: time.Second}

func httpProbeOK(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return strings.Contains(string(body), `"Browser"`)
}
