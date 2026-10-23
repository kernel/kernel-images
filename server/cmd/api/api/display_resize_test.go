package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/kernel-images/server/lib/nekoclient"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	nekooapi "github.com/m1k1o/neko/server/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCurrentResolutionFromXrandr(t *testing.T) {
	for _, tc := range []struct {
		name                string
		out                 string
		width, height, rate int
		hasRate             bool
	}{
		{"rounded CVT rate", "   376x480_60  59.21*+\n", 376, 480, 59, true},
		{"nearest integer", "   1280x800_60.00 59.81*\n", 1280, 800, 60, true},
		{"non-default rate", "   1920x1080_25.00 24.98*+ 60.00\n", 1920, 1080, 25, true},
		{"Xvfb", "   1280x720 0.00*\n", 1280, 720, 60, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, h, rate, hasRate, err := parseCurrentResolutionFromXrandr(tc.out)
			require.NoError(t, err)
			assert.Equal(t, tc.width, w)
			assert.Equal(t, tc.height, h)
			assert.Equal(t, tc.rate, rate)
			assert.Equal(t, tc.hasRate, hasRate)
		})
	}
	_, _, _, _, err := parseCurrentResolutionFromXrandr("DUMMY0 connected\n")
	require.Error(t, err)
}

func TestRejectedResizeRestoresDisplay(t *testing.T) {
	for _, status := range []int{http.StatusUnprocessableEntity, http.StatusBadRequest, http.StatusInternalServerError} {
		for _, path := range []string{"display", "configure-live", "configure-stopped"} {
			t.Run(fmt.Sprintf("%s/%d", path, status), func(t *testing.T) {
				dir := t.TempDir()
				state := filepath.Join(dir, "screen")
				previous := "   1280x800_25.00 25.00*+\n"
				require.NoError(t, os.WriteFile(state, []byte(previous), 0600))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "xrandr"), []byte("#!/bin/sh\ncat \"$TEST_XRANDR_STATE\"\n"), 0700))
				t.Setenv("TEST_XRANDR_STATE", state)
				t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
				t.Setenv("ENABLE_WEBRTC", "true")

				var requests []nekooapi.ScreenConfiguration
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/api/login" {
						_, _ = w.Write([]byte(`{"token":"test-token"}`))
						return
					}
					var config nekooapi.ScreenConfiguration
					if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					requests = append(requests, config)
					if len(requests) == 1 {
						_ = os.WriteFile(state, []byte("   3840x2160_25.00 25.00*+\n"), 0600)
						w.WriteHeader(status)
						_, _ = w.Write([]byte(`{"message":"cannot set screen size"}`))
						return
					}
					if path == "configure-live" && len(requests) == 2 {
						// Neko can accept a reconfig without applying it.
						w.WriteHeader(http.StatusNoContent)
						return
					}
					_ = os.WriteFile(state, []byte(previous), 0600)
					w.WriteHeader(http.StatusNoContent)
				}))
				defer server.Close()
				client, err := nekoclient.NewAuthClient(server.URL, "admin", "test")
				require.NoError(t, err)
				svc := &ApiService{nekoAuthClient: client, recordManager: recorder.NewFFmpegManager()}
				svc.displayModeOnce.Do(func() { svc.displayModeVal = "xorg" })
				width, height, rate, idle := 379, 480, 60, false
				requestRate := oapi.PatchDisplayRequestRefreshRate(rate)
				body := &oapi.PatchDisplayJSONRequestBody{Width: &width, Height: &height, RefreshRate: &requestRate, RequireIdle: &idle}
				ctx := context.Background()
				if path == "display" {
					resp, err := svc.PatchDisplay(ctx, oapi.PatchDisplayRequestObject{Body: body})
					require.NoError(t, err)
					if status < 500 {
						require.IsType(t, oapi.PatchDisplay400JSONResponse{}, resp)
					} else {
						require.IsType(t, oapi.PatchDisplay500JSONResponse{}, resp)
					}
				} else {
					var resp oapi.ChromiumConfigureResponseObject
					if path == "configure-live" {
						resp = chromiumRunPatchDisplay(ctx, svc, body)
					} else {
						resp = chromiumDisplayApplyWhileStopped(ctx, svc, &chromiumDisplayPlan{width: width, height: height, refreshRate: rate})
					}
					if status < 500 {
						require.IsType(t, oapi.ChromiumConfigure400JSONResponse{}, resp)
					} else {
						require.IsType(t, oapi.ChromiumConfigure500JSONResponse{}, resp)
					}
				}
				if path == "configure-live" {
					require.Len(t, requests, 3)
				} else {
					require.Len(t, requests, 2)
				}
				for _, restore := range requests[1:] {
					assert.Equal(t, 1280, *restore.Width)
					assert.Equal(t, 800, *restore.Height)
					assert.Equal(t, 25, *restore.Rate)
				}
				got, err := os.ReadFile(state)
				require.NoError(t, err)
				assert.Equal(t, previous, string(got))
			})
		}
	}
}
