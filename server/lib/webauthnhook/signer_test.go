package webauthnhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const assertion = `{"id":"Y3JlZA","rawId":"Y3JlZA","type":"public-key","response":{"clientDataJSON":"e30","authenticatorData":"AA","signature":"AA"}}`

func metroSigner(url string) *MetroSigner {
	return &MetroSigner{
		BaseURL: func() string { return url + "/" },
		Token:   func() string { return "instance-jwt" },
		Client:  &http.Client{},
		Logger:  discard,
	}
}

func TestMetroSignerPostsRequest(t *testing.T) {
	var got struct {
		path, auth, contentType string
		body                    map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth, got.contentType = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &got.body)
		_, _ = io.WriteString(w, `{"result":"credential","credential":`+assertion+`}`)
	}))
	defer server.Close()

	answer := metroSigner(server.URL).Answer(context.Background(), "get", Request{
		Origin: "https://login.example.com", TopOrigin: "https://example.com", PublicKey: json.RawMessage(`{"challenge":"AAAA"}`),
	})
	require.Equal(t, "credential", answer.Result)
	require.JSONEq(t, assertion, string(answer.Credential))
	require.Equal(t, "/webauthn/get", got.path)
	require.Equal(t, "Bearer instance-jwt", got.auth)
	require.Equal(t, "application/json", got.contentType)
	require.Equal(t, map[string]any{
		"origin": "https://login.example.com", "top_origin": "https://example.com", "public_key": map[string]any{"challenge": "AAAA"},
	}, got.body)
}

func TestMetroSignerRetriesServerErrors(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"result":"not_handled"}`)
	}))
	defer server.Close()

	answer := metroSigner(server.URL).Answer(context.Background(), "create", Request{})
	require.Equal(t, notHandled, answer)
	require.EqualValues(t, 3, attempts.Load())
}

func TestMetroSignerGivesUpAtDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	require.Equal(t, notHandled, metroSigner(server.URL).Answer(ctx, "get", Request{}))
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestMetroSignerDoesNotRetryRefusals(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.WriteHeader(status)
		}))
		require.Equal(t, notHandled, metroSigner(server.URL).Answer(context.Background(), "get", Request{}))
		require.EqualValues(t, 1, attempts.Load(), "status %d", status)
		server.Close()
	}
}

func TestMetroSignerWithoutConfigurationLeavesRequestToBrowser(t *testing.T) {
	signer := metroSigner("")
	signer.BaseURL = func() string { return "" }
	require.Equal(t, notHandled, signer.Answer(context.Background(), "get", Request{}))
}

func TestValidAnswer(t *testing.T) {
	registration := `{"id":"Y3JlZA","rawId":"Y3JlZA","type":"public-key","response":{"clientDataJSON":"e30","authenticatorData":"AA","attestationObject":"oA"}}`
	for name, tc := range map[string]struct {
		op     string
		answer Answer
		want   Answer
	}{
		"assertion":             {"get", Answer{Result: "credential", Credential: json.RawMessage(assertion)}, Answer{Result: "credential", Credential: json.RawMessage(assertion)}},
		"registration":          {"create", Answer{Result: "credential", Credential: json.RawMessage(registration)}, Answer{Result: "credential", Credential: json.RawMessage(registration)}},
		"assertion for create":  {"create", Answer{Result: "credential", Credential: json.RawMessage(assertion)}, notHandled},
		"credential without id": {"get", Answer{Result: "credential", Credential: json.RawMessage(`{"response":{}}`)}, notHandled},
		"known error":           {"get", Answer{Result: "error", Name: "SecurityError", Message: "rp mismatch"}, Answer{Result: "error", Name: "SecurityError", Message: "rp mismatch"}},
		"unknown error name":    {"get", Answer{Result: "error", Name: "TypeError", Message: "x"}, Answer{Result: "error", Name: "NotAllowedError", Message: "x"}},
		"unknown result":        {"get", Answer{Result: "maybe"}, notHandled},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, validAnswer(tc.op, tc.answer))
		})
	}
}
