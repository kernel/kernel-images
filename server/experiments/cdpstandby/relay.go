package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type frame struct {
	Seq  uint64          `json:"seq"`
	Data json.RawMessage `json:"data"`
}

type relay struct {
	chrome      *websocket.Conn
	token       string
	sendMu      sync.Mutex
	lastCommand uint64
	mu          sync.Mutex
	sequence    uint64
	queue       []frame
	bytes       int
	err         error
	notify      chan struct{}
}

func newRelay(ctx context.Context, upstream, token string) (*relay, error) {
	conn, _, err := websocket.Dial(ctx, upstream, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(maxBytes)
	r := &relay{chrome: conn, token: token, notify: make(chan struct{}, 1)}
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			r.mu.Lock()
			if err == nil && r.bytes+len(data) > maxBytes {
				err = fmt.Errorf("relay event buffer full")
			}
			if err == nil {
				r.sequence++
				r.queue = append(r.queue, frame{r.sequence, data})
				r.bytes += len(data)
			} else {
				r.err = err
			}
			r.mu.Unlock()
			select {
			case r.notify <- struct{}{}:
			default:
			}
			if err != nil {
				conn.CloseNow()
				return
			}
		}
	}()
	return r, nil
}

func (r *relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Authorization") != "Bearer "+r.token {
		http.Error(w, "unauthorized", 401)
		return
	}
	switch {
	case req.Method == "POST" && req.URL.Path == "/send":
		var f frame
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxBytes)).Decode(&f); err != nil {
			reply(w, nil, err)
			return
		}
		r.sendMu.Lock()
		defer r.sendMu.Unlock()
		// A retry of the last accepted command must not execute it twice.
		if f.Seq > 0 && f.Seq == r.lastCommand {
			reply(w, true, nil)
			return
		}
		if f.Seq != r.lastCommand+1 {
			reply(w, nil, fmt.Errorf("out of order command %d", f.Seq))
			return
		}
		// The chrome connection belongs to the relay, not this HTTP request.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := r.chrome.Write(ctx, websocket.MessageText, f.Data)
		if err == nil {
			r.lastCommand = f.Seq
		}
		reply(w, true, err)
	case req.Method == "GET" && req.URL.Path == "/events":
		after, err := strconv.ParseUint(req.URL.Query().Get("after"), 10, 64)
		if err != nil {
			reply(w, nil, err)
			return
		}
		for attempt := 0; attempt < 2; attempt++ {
			r.mu.Lock()
			if after > r.sequence {
				r.mu.Unlock()
				reply(w, nil, fmt.Errorf("acknowledgment beyond last event"))
				return
			}
			for len(r.queue) > 0 && r.queue[0].Seq <= after {
				r.bytes -= len(r.queue[0].Data)
				r.queue[0] = frame{}
				r.queue = r.queue[1:]
			}
			batch := append([]frame{}, r.queue...)
			err = r.err
			r.mu.Unlock()
			if err != nil || len(batch) > 0 || attempt == 1 {
				reply(w, batch, err)
				return
			}
			select {
			case <-r.notify:
			case <-time.After(100 * time.Millisecond):
			case <-req.Context().Done():
				return
			}
		}
	default:
		http.NotFound(w, req)
	}
}
