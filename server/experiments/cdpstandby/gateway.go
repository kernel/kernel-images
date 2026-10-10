package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type gateway struct {
	mu                                                  sync.Mutex // Serializes command admission, event delivery, and standby/restore.
	mode, upstream, api, instance, apiToken, relayToken string
	idle                                                time.Duration
	state                                               string
	used                                                bool
	noKeepalive                                         bool
	client, chrome                                      *websocket.Conn
	pending                                             map[string]bool
	lastCommand                                         time.Time
	commandSeq, eventSeq                                uint64
	standbys, wakes                                     int
	wakeMS, standbyMS                                   int64
}

func commandKey(data []byte) string {
	var msg struct {
		ID      *int64 `json:"id"`
		Session string `json:"sessionId"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.ID == nil {
		return ""
	}
	return msg.Session + ":" + strconv.FormatInt(*msg.ID, 10)
}

func (g *gateway) lifecycle(ctx context.Context, action string) error {
	return request(ctx, "POST", g.api+"/instances/"+g.instance+"/"+action, g.apiToken, nil, nil)
}

func (g *gateway) connect(ctx context.Context) error {
	var opts *websocket.DialOptions
	if g.noKeepalive {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: -1}
		opts = &websocket.DialOptions{HTTPClient: &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext}}}
	}
	conn, _, err := websocket.Dial(ctx, g.upstream, opts)
	if err != nil {
		return err
	}
	conn.SetReadLimit(maxBytes)
	g.chrome = conn
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			g.mu.Lock()
			if g.chrome != conn {
				g.mu.Unlock()
				return
			}
			if err == nil {
				err = g.deliver(ctx, data)
			}
			g.mu.Unlock()
			if err != nil {
				g.fail(err)
				return
			}
		}
	}()
	return nil
}

// Called with mu held. An event is acknowledged to the relay only after delivery.
func (g *gateway) deliver(ctx context.Context, data []byte) error {
	if err := g.client.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}
	delete(g.pending, commandKey(data))
	return nil
}

func (g *gateway) fail(err error) {
	log.Print(err)
	g.mu.Lock()
	g.state = "failed"
	client := g.client
	g.mu.Unlock()
	if client != nil {
		client.CloseNow()
	}
}

func (g *gateway) standby(ctx context.Context) error {
	if g.state != "running" || g.client == nil {
		return fmt.Errorf("not running with a client")
	}
	if len(g.pending) != 0 {
		return fmt.Errorf("%d commands pending", len(g.pending))
	}
	start := time.Now()
	if g.mode == "reconnect" {
		conn := g.chrome
		g.chrome = nil
		conn.CloseNow()
	}
	if err := g.lifecycle(ctx, "standby"); err != nil {
		g.state = "failed"
		g.client.CloseNow()
		return err
	}
	g.standbyMS = time.Since(start).Milliseconds()
	g.state = "standby"
	g.standbys++
	log.Printf("standby %d", g.standbys)
	return nil
}

func (g *gateway) wake(ctx context.Context) error {
	if g.state == "running" {
		return nil
	}
	if g.state != "standby" {
		return fmt.Errorf("cannot wake from %s", g.state)
	}
	start := time.Now()
	if err := g.lifecycle(ctx, "restore"); err != nil {
		return err
	}
	if g.mode == "reconnect" {
		if err := g.connect(ctx); err != nil {
			return err
		}
	}
	g.wakeMS = time.Since(start).Milliseconds()
	g.wakes++
	g.state = "running"
	log.Printf("wake %d: %d ms", g.wakes, g.wakeMS)
	return nil
}

func (g *gateway) background(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		g.mu.Lock()
		var err error
		if g.state == "running" {
			if g.mode == "relay" {
				var batch []frame
				err = request(ctx, "GET", g.upstream+"/events?after="+strconv.FormatUint(g.eventSeq, 10), g.relayToken, nil, &batch)
				for _, f := range batch {
					if err != nil {
						break
					}
					if f.Seq != g.eventSeq+1 {
						err = fmt.Errorf("event sequence gap")
						break
					}
					err = g.deliver(ctx, f.Data)
					if err == nil {
						g.eventSeq = f.Seq
					}
				}
			}
			if err == nil && g.idle > 0 && len(g.pending) == 0 && time.Since(g.lastCommand) >= g.idle {
				err = g.standby(ctx)
			}
		}
		g.mu.Unlock()
		if err != nil {
			g.fail(err)
			return
		}
	}
}

func (g *gateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		reply(w, map[string]any{"instance": g.instance, "mode": g.mode, "state": g.state, "pending": len(g.pending), "standbys": g.standbys, "wakes": g.wakes, "wake_ms": g.wakeMS, "standby_ms": g.standbyMS}, nil)
	})
	mux.HandleFunc("POST /standby", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		reply(w, true, g.standby(r.Context()))
	})
	mux.HandleFunc("GET /cdp", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		if g.used {
			g.mu.Unlock()
			http.Error(w, "one client per experiment process", 409)
			return
		}
		g.used = true
		g.mu.Unlock()
		client, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		client.SetReadLimit(maxBytes)
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer client.CloseNow()
		g.mu.Lock()
		g.client, g.lastCommand = client, time.Now()
		if g.mode != "relay" {
			err = g.connect(ctx)
		}
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.chrome != nil {
				g.chrome.CloseNow()
			}
			g.state = "closed"
		}()
		if err != nil {
			g.fail(err)
			return
		}
		go g.background(ctx)
		for {
			_, data, err := client.Read(ctx)
			if err != nil {
				return
			}
			g.mu.Lock()
			err = g.wake(ctx)
			if err == nil {
				g.lastCommand = time.Now()
				if key := commandKey(data); key != "" {
					g.pending[key] = true
				}
				if g.mode == "relay" {
					g.commandSeq++
					err = request(ctx, "POST", g.upstream+"/send", g.relayToken, frame{g.commandSeq, data}, nil)
				} else {
					err = g.chrome.Write(ctx, websocket.MessageText, data)
				}
			}
			g.mu.Unlock()
			if err != nil {
				g.fail(err)
				return
			}
		}
	})
	return mux
}
