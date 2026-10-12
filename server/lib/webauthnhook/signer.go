package webauthnhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// signerWindow bounds one request's exchange with metro-api, retries
	// included, so the page is answered well before it would give up.
	signerWindow     = 10 * time.Second
	signerRetryDelay = 250 * time.Millisecond
	signerRetryMax   = 2 * time.Second
	maxAnswerBytes   = 64 << 10
)

// Request is what metro-api receives for a page's get or create call.
type Request struct {
	Origin    string          `json:"origin"`
	TopOrigin string          `json:"top_origin"`
	Mediation string          `json:"mediation,omitempty"`
	PublicKey json.RawMessage `json:"public_key"`
}

// Answer is metro-api's response, relayed to the page.
type Answer struct {
	Result     string          `json:"result"`
	Credential json.RawMessage `json:"credential,omitempty"`
	Name       string          `json:"name,omitempty"`
	Message    string          `json:"message,omitempty"`
}

var notHandled = Answer{Result: "not_handled"}

// Signer answers page requests. op is "get" or "create".
type Signer interface {
	Answer(ctx context.Context, op string, req Request) Answer
}

// MetroSigner forwards requests to metro-api, authenticated as the instance.
// Both values are read per request: a forked VM learns them after boot.
type MetroSigner struct {
	BaseURL func() string
	Token   func() string
	Client  *http.Client
	Logger  *slog.Logger
}

// Answer posts to {BaseURL}/webauthn/{op}. Server errors and transport
// failures are retried within signerWindow; anything else that is not a valid
// answer leaves the request to the browser.
func (m *MetroSigner) Answer(ctx context.Context, op string, req Request) Answer {
	base, token := strings.TrimRight(m.BaseURL(), "/"), m.Token()
	if base == "" || token == "" {
		m.Logger.Warn("webauthn: metro-api is not configured; leaving the request to the browser")
		return notHandled
	}
	body, err := json.Marshal(req)
	if err != nil {
		return notHandled
	}
	ctx, cancel := context.WithTimeout(ctx, signerWindow)
	defer cancel()
	url := base + "/webauthn/" + op
	delay := signerRetryDelay
	for {
		answer, retry := m.post(ctx, url, token, body)
		if !retry {
			return validAnswer(op, answer)
		}
		select {
		case <-ctx.Done():
			m.Logger.Warn("webauthn: metro-api did not answer in time", "op", op)
			return notHandled
		case <-time.After(delay):
		}
		delay = min(2*delay, signerRetryMax)
	}
}

func (m *MetroSigner) post(ctx context.Context, url, token string, body []byte) (Answer, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return notHandled, false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.Client.Do(req)
	if err != nil {
		m.Logger.Warn("webauthn: metro-api request failed", "url", url, "err", err)
		return notHandled, ctx.Err() == nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		m.Logger.Warn("webauthn: metro-api refused the request", "url", url, "status", resp.StatusCode)
		return notHandled, resp.StatusCode >= 500
	}
	var answer Answer
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswerBytes)).Decode(&answer); err != nil {
		m.Logger.Warn("webauthn: metro-api answer is malformed", "url", url, "err", err)
		return notHandled, false
	}
	return answer, false
}

var errorNames = map[string]bool{
	"SecurityError":     true,
	"NotAllowedError":   true,
	"InvalidStateError": true,
	"NotSupportedError": true,
}

// validAnswer keeps the page from receiving a credential it cannot build or
// an exception type the WebAuthn API does not throw.
func validAnswer(op string, answer Answer) Answer {
	switch answer.Result {
	case "credential":
		if !validCredential(op, answer.Credential) {
			return notHandled
		}
		return Answer{Result: answer.Result, Credential: answer.Credential}
	case "error":
		if !errorNames[answer.Name] {
			answer.Name = "NotAllowedError"
		}
		return Answer{Result: answer.Result, Name: answer.Name, Message: answer.Message}
	default:
		return notHandled
	}
}

func validCredential(op string, raw json.RawMessage) bool {
	var credential struct {
		ID       string         `json:"id"`
		RawID    string         `json:"rawId"`
		Response map[string]any `json:"response"`
	}
	if json.Unmarshal(raw, &credential) != nil || credential.ID == "" || credential.RawID == "" {
		return false
	}
	required := []string{"clientDataJSON", "authenticatorData", "signature"}
	if op == "create" {
		required = []string{"clientDataJSON", "authenticatorData", "attestationObject"}
	}
	for _, field := range required {
		if value, ok := credential.Response[field].(string); !ok || value == "" {
			return false
		}
	}
	return true
}
