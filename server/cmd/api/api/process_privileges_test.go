package api

import (
	"context"
	"testing"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/stretchr/testify/require"
)

func TestNonRootProcessIdentityRequests(t *testing.T) {
	for _, mode := range []string{"", "false"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("RUN_AS_ROOT", mode)
			for _, body := range []oapi.ProcessExecRequest{
				{Command: "true", AsRoot: ptrOf(true)},
				{Command: "true", AsUser: ptrOf("root")},
				{Command: "true", AsUser: ptrOf("0")},
				{Command: "true", AsUser: ptrOf("1000:0")},
				{Command: "true", AsUser: ptrOf("kernel")},
				{Command: "true", AsRoot: ptrOf(true), Env: &map[string]string{"RUN_AS_ROOT": "true"}},
			} {
				_, err := buildCmd(&body)
				require.ErrorContains(t, err, "RUN_AS_ROOT is false")
				svc := &ApiService{}
				resp, err := svc.ProcessExec(context.Background(), oapi.ProcessExecRequestObject{Body: &body})
				require.NoError(t, err)
				require.IsType(t, oapi.ProcessExec400JSONResponse{}, resp)
				spawn := oapi.ProcessSpawnRequest{Command: body.Command, AsRoot: body.AsRoot, AsUser: body.AsUser, Env: body.Env}
				spawnResp, err := svc.ProcessSpawn(context.Background(), oapi.ProcessSpawnRequestObject{Body: &spawn})
				require.NoError(t, err)
				require.IsType(t, oapi.ProcessSpawn400JSONResponse{}, spawnResp)
			}
			for _, body := range []oapi.ProcessExecRequest{
				{Command: "true"},
				{Command: "true", AsRoot: ptrOf(false)},
				{Command: "true", AsUser: ptrOf("")},
			} {
				cmd, err := buildCmd(&body)
				require.NoError(t, err)
				require.Nil(t, cmd.SysProcAttr)
			}
		})
	}
}
