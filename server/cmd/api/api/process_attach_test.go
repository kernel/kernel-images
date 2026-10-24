package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/scaletozero"
	"github.com/kernel/kernel-images/server/lib/wsdrain"
	"github.com/stretchr/testify/require"
)

func TestProcessAttachReplaysOutputThenExits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := &ApiService{procs: make(map[string]*processHandle), stz: scaletozero.NewNoopController()}

	// The first line is written before any client attaches, so it must come back as replay.
	args := []string{"-c", "echo first; sleep 0.3; echo second"}
	allocateTTY := true
	body := &oapi.ProcessSpawnRequest{Command: "sh", Args: &args, AllocateTty: &allocateTTY}
	spawnResp, err := svc.ProcessSpawn(ctx, oapi.ProcessSpawnRequestObject{Body: body})
	require.NoError(t, err)
	s200, ok := spawnResp.(oapi.ProcessSpawn200JSONResponse)
	require.True(t, ok, "unexpected spawn resp: %T", spawnResp)
	id := s200.ProcessId.String()

	time.Sleep(100 * time.Millisecond)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc.HandleProcessAttachWS(w, r, id, wsdrain.New())
	}))
	defer srv.Close()

	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	var out strings.Builder
	exitCode := -1
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		msgType, data, err := conn.Read(readCtx)
		require.NoError(t, err, "attach ended before exit message; output so far %q", out.String())
		if msgType == websocket.MessageText {
			var ctrl struct {
				Type     string `json:"type"`
				ExitCode *int   `json:"exitCode"`
			}
			require.NoError(t, json.Unmarshal(data, &ctrl))
			if ctrl.Type == "exit" {
				require.NotNil(t, ctrl.ExitCode)
				exitCode = *ctrl.ExitCode
				break
			}
			continue
		}
		out.Write(data)
	}

	require.Equal(t, 0, exitCode)
	got := out.String()
	require.Contains(t, got, "first")
	require.Contains(t, got, "second")
	require.Less(t, strings.Index(got, "first"), strings.Index(got, "second"))
}
