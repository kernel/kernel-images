package agentproxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigurationHTTP(t *testing.T) {
	t.Setenv("PI_HTTP_TEST_KEY", "private-provider-key")
	options := &PiOptions{StateDir: t.TempDir(), RuntimeDir: "/runtime/pi", Node: "/bin/node", Credentials: map[string]string{"provider": "PI_HTTP_TEST_KEY"}}
	h, err := New(context.Background(), Config{ACPRemote: "/bin/acpremote", MaxConnections: 1, Pi: options}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	h.pi.preparer = prepareFunc(func(_ context.Context, dir string, _ json.RawMessage) (Harness, error) {
		return Harness{Command: "/bin/true", Cwd: dir, Env: map[string]string{"HIDDEN": "private-provider-key"}}, nil
	})
	path := "/agent/v1/harnesses/pi/config"
	request := func(method, body, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	initial := request(http.MethodGet, "", "")
	if initial.Code != 200 || initial.Header().Get("ETag") != `"0"` {
		t.Fatal(initial)
	}
	body := `{"launch":{"provider":"openrouter","model":"z-ai/glm-5.3","thinking":"low","credential":"provider"},"shared":{}}`
	if w := request(http.MethodPut, body, ""); w.Code != 428 {
		t.Fatal(w.Code)
	}
	if w := request(http.MethodPut, strings.Replace(body, `"shared":{}`, `"typo":{}`, 1), `"0"`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	ready := request(http.MethodPut, body, `"0"`)
	if ready.Code != 200 || !strings.Contains(ready.Body.String(), `"status":"ready"`) {
		t.Fatal(ready.Body.String())
	}
	if strings.Contains(ready.Body.String(), "private-provider-key") || strings.Contains(ready.Body.String(), "PI_HTTP_TEST_KEY") {
		t.Fatal("configuration response leaked credentials or binding implementation")
	}
	if !strings.Contains(ready.Body.String(), `"extensions":[]`) {
		t.Fatal("extensions serialized as null")
	}
	if w := request(http.MethodPut, body, `"0"`); w.Code != 409 {
		t.Fatal("stale revision accepted")
	}
	if w := request(http.MethodDelete, "", ""); w.Code != 405 {
		t.Fatal(w.Code)
	}
}
