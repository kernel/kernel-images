// cdpstandby is a single-client experiment, not a production proxy.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const maxBytes = 4 << 20

// Short-lived HTTP connections deliberately do not depend on TCP surviving standby.
var httpClient = &http.Client{Timeout: 120 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

func request(ctx context.Context, method, url, token string, in, out any) error {
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, b)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, maxBytes*2)).Decode(out)
	}
	return nil
}

func reply(w http.ResponseWriter, value any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Print(err)
	}
}

func main() {
	mode := flag.String("mode", "direct", "direct, reconnect (negative control), relay, or guest")
	listen := flag.String("listen", "127.0.0.1:9226", "HTTP listen address")
	upstream := flag.String("upstream", "", "chrome websocket URL, or guest relay HTTP URL")
	api := flag.String("api", "http://127.0.0.1:4973", "hypeman API URL")
	instance := flag.String("instance", "", "dedicated experiment instance ID")
	tokenFile := flag.String("token-file", "", "hypeman API token file (gateway only)")
	relayToken := flag.String("relay-token-file", "", "shared relay token file")
	idle := flag.Duration("idle", 0, "opt-in command-idle timeout; zero means manual standby only")
	noKeepalive := flag.Bool("no-upstream-keepalive", false, "disable upstream TCP keepalives for direct mode")
	flag.Parse()
	readToken := func(path string) string {
		if path == "" {
			return ""
		}
		b, err := os.ReadFile(path)
		if err != nil {
			log.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	ctx := context.Background()
	var handler http.Handler
	if *mode == "guest" {
		token := readToken(*relayToken)
		if token == "" {
			log.Fatal("guest requires -relay-token-file")
		}
		r, err := newRelay(ctx, *upstream, token)
		if err != nil {
			log.Fatal(err)
		}
		handler = r
	} else {
		if *mode != "direct" && *mode != "reconnect" && *mode != "relay" {
			log.Fatal("unknown mode")
		}
		if *instance == "" {
			log.Fatal("gateway requires -instance")
		}
		g := &gateway{mode: *mode, upstream: *upstream, api: *api, instance: *instance, apiToken: readToken(*tokenFile), relayToken: readToken(*relayToken), idle: *idle, noKeepalive: *noKeepalive, state: "running", pending: make(map[string]bool)}
		handler = g.handler()
	}
	log.Printf("%s listening on %s", *mode, *listen)
	log.Fatal(http.ListenAndServe(*listen, handler))
}
