package cdpmonitor

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/oapi"
)

const (
	networkHeaderWait       = time.Second
	maxNetworkExchanges     = 8192
	maxExtraInfoPerExchange = 64
	durableBodyBufferSize   = 10 * 1024 * 1024
)

// Chrome emits base events and ExtraInfo on independent streams. Redirects reuse
// request IDs; only hops whose response advertises ExtraInfo consume the header queue.
// All exchange and hop fields are protected by pendReqMu.
type networkExchange struct {
	addedAt      time.Time
	hops         []*networkHop
	requests     []json.RawMessage
	terminal     bool
	publications int // request/response publishers still using this exchange
}

type networkHop struct {
	exchange         *networkExchange
	request          oapi.BrowserNetworkRequestEventData
	source           oapi.BrowserEventSource
	observedAt       time.Time
	known            bool
	hasExtra         bool
	requestHeaders   json.RawMessage
	requestReady     chan struct{}
	requestMatched   bool
	requestPublished chan struct{}
}

func (m *Monitor) exchangeLocked(key networkRequestKey) *networkExchange {
	e := m.networkExchanges[key]
	if e == nil {
		if m.networkHeadersSaturated {
			return nil
		}
		if len(m.networkExchanges) >= maxNetworkExchanges {
			// ExtraInfo may precede its request. Once an event is dropped, new
			// chains cannot safely resume correlation mid-capture: a later hop's
			// headers could otherwise be mistaken for the missing first hop.
			m.networkHeadersSaturated = true
			return nil
		}
		e = &networkExchange{addedAt: time.Now()}
		m.networkExchanges[key] = e
	}
	return e
}

func (m *Monitor) queueNetworkRequestLocked(key networkRequestKey, request oapi.BrowserNetworkRequestEventData, info targetInfo, observedAt time.Time) *networkHop {
	e := m.exchangeLocked(key)
	if e == nil {
		e = &networkExchange{}
	}
	metadata := targetMetadata(info)
	h := &networkHop{
		exchange:         e,
		request:          request,
		source:           oapi.BrowserEventSource{Kind: oapi.Cdp, Metadata: &metadata},
		observedAt:       observedAt,
		requestReady:     make(chan struct{}),
		requestPublished: make(chan struct{}),
	}
	if m.networkExchanges[key] == e {
		e.hops = append(e.hops, h)
	} else {
		// Keep publication ordering even when header enrichment is unavailable.
		close(h.requestReady)
	}
	e.publications++
	return h
}

func (m *Monitor) handleRequestExtraInfo(p cdpNetworkRequestWillBeSentExtraInfoParams, sessionID string) {
	key := networkRequestKey{sessionID, p.RequestID}
	m.pendReqMu.Lock()
	defer m.pendReqMu.Unlock()
	e := m.exchangeLocked(key)
	if e == nil || len(e.requests) >= maxExtraInfoPerExchange {
		return
	}
	e.requests = append(e.requests, p.Headers)
	e.matchHeaders()
}

func (m *Monitor) resolveNetworkHeadersLocked(h *networkHop, hasExtra bool) {
	if h == nil {
		return
	}
	h.known, h.hasExtra = true, hasExtra
	h.exchange.matchHeaders()
}

func (m *Monitor) resolveFailedHeadersLocked(h *networkHop) {
	if h == nil {
		return
	}
	h.exchange.terminal = true
	// CORS failures can have ExtraInfo without a responseReceived event.
	if !h.known {
		h.known, h.hasExtra = true, true
	}
	h.exchange.matchHeaders()
	m.cleanupNetworkExchangeLocked(networkRequestKey{h.request.SessionId, h.request.RequestId}, h.exchange)
}

func (m *Monitor) cleanupNetworkExchangeLocked(key networkRequestKey, e *networkExchange) {
	if e.terminal && e.publications == 0 && m.networkExchanges[key] == e {
		delete(m.networkExchanges, key)
	}
}

func (m *Monitor) finishNetworkPublication(key networkRequestKey, e *networkExchange) {
	m.pendReqMu.Lock()
	defer m.pendReqMu.Unlock()
	e.publications--
	m.cleanupNetworkExchangeLocked(key, e)
}

func (e *networkExchange) matchHeaders() {
	for _, h := range e.hops {
		if !h.known {
			break
		}
		if h.requestMatched {
			continue
		}
		if h.hasExtra {
			if len(e.requests) == 0 {
				break
			}
			h.requestHeaders, e.requests = e.requests[0], e.requests[1:]
		}
		h.requestMatched = true
		close(h.requestReady)
	}
}

func waitNetworkHeaders(ctx context.Context, ready <-chan struct{}) {
	timer := time.NewTimer(networkHeaderWait)
	defer timer.Stop()
	select {
	case <-ready:
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (m *Monitor) publishNetworkRequest(ctx context.Context, key networkRequestKey, h *networkHop, redirectPublished <-chan struct{}) {
	m.telemetryWg.Go(func() {
		defer m.finishNetworkPublication(key, h.exchange)
		defer close(h.requestPublished)
		waitNetworkHeaders(ctx, h.requestReady)
		if redirectPublished != nil {
			<-redirectPublished
		}
		m.pendReqMu.Lock()
		payload := h.request
		if h.requestHeaders != nil {
			var headers oapi.BrowserHttpHeaders
			if json.Unmarshal(h.requestHeaders, &headers) == nil && headers != nil {
				payload.Headers = headers
			}
		}
		m.pendReqMu.Unlock()
		data, _ := json.Marshal(payload)
		m.publishEventAt(h.observedAt, EventNetworkRequest, events.Network, h.source, "Network.requestWillBeSent", data, key.sessionID)
	})
}

// The request publisher always drains, including on cancellation. Wait for its
// publication, not just header readiness, to preserve per-request event order.
func (m *Monitor) publishAfterNetworkRequest(h *networkHop, publish func()) {
	if h == nil {
		publish()
		return
	}
	m.telemetryWg.Go(func() {
		<-h.requestPublished
		publish()
	})
}

func (m *Monitor) emitNetworkResponse(ctx context.Context, observedAt time.Time, key networkRequestKey, state networkReqState, info targetInfo, redirect bool) <-chan struct{} {
	published := make(chan struct{})
	h := state.hop
	if h != nil {
		m.pendReqMu.Lock()
		h.exchange.publications++
		if !redirect {
			h.exchange.terminal = true
		}
		m.pendReqMu.Unlock()
	}
	m.telemetryWg.Go(func() {
		defer close(published)
		if h != nil {
			defer m.finishNetworkPublication(key, h.exchange)
		}
		// Fetch before waiting for ExtraInfo so header ordering cannot delay retrieval.
		var body string
		var bodyErr error
		if !redirect {
			body, bodyErr = m.fetchResponseBody(ctx, key.requestID, key.sessionID, state)
		}
		if h != nil {
			waitNetworkHeaders(ctx, h.requestReady)
			<-h.requestPublished
		}
		var headers oapi.BrowserHttpHeaders
		_ = json.Unmarshal(state.resHeaders, &headers)
		// Use Chrome's effective status and headers with its captured body. A wire
		// 304 can omit cached headers; a separate wire response view is intentionally
		// not exposed by browser telemetry.
		payload := oapi.BrowserNetworkResponseEventData{
			SessionId:    key.sessionID,
			TargetId:     info.targetID,
			TargetType:   oapi.BrowserTargetType(info.targetType),
			FrameId:      ptrOf(state.frameID),
			LoaderId:     ptrOf(state.loaderID),
			Url:          ptrOf(state.url),
			NavSeq:       state.navSeq,
			RequestId:    key.requestID,
			Method:       state.method,
			Status:       state.status,
			Headers:      headers,
			StatusText:   optPtr(state.statusText),
			MimeType:     optPtr(state.mimeType),
			ResourceType: optPtr(state.resourceType),
			Body:         optPtr(body),
		}
		if bodyErr != nil {
			payload.BodyError = ptrOf(bodyErr.Error())
		}
		metadata := targetMetadata(info)
		source := oapi.BrowserEventSource{Kind: oapi.Cdp, Metadata: &metadata}
		if h != nil {
			source = h.source
			m.pendReqMu.Lock()
			if h.requestHeaders != nil {
				var requestHeaders oapi.BrowserHttpHeaders
				if json.Unmarshal(h.requestHeaders, &requestHeaders) == nil && requestHeaders != nil {
					payload.RequestHeaders = &requestHeaders
				}
			}
			m.pendReqMu.Unlock()
		}
		data, _ := json.Marshal(payload)
		event := "Network.loadingFinished"
		if redirect {
			event = "Network.requestWillBeSent"
		}
		m.publishEventAt(observedAt, EventNetworkResponse, events.Network, source, event, data, key.sessionID)
	})
	return published
}

func (m *Monitor) finishNetworkRequest(state networkReqState) {
	if cs := state.countedState; cs != nil {
		m.pendReqMu.Lock()
		defer m.pendReqMu.Unlock()
		cs.mu.Lock()
		sameNavigation := state.countedNavSeq == cs.navSeq
		cs.mu.Unlock()
		if sameNavigation {
			cs.onLoadingFinished()
		}
	}
}

func (m *Monitor) configureBodyCapture(ctx context.Context, sessionID string, enabled bool) error {
	size := 0
	if enabled {
		size = durableBodyBufferSize
	}
	_, err := m.send(ctx, "Network.enable", map[string]any{"enableDurableMessages": enabled, "maxTotalBufferSize": size}, sessionID)
	return err
}
