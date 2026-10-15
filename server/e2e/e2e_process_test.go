package e2e

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
)

func TestProcessLifecycle(t *testing.T) {
	for _, image := range []string{headlessImage, headfulImage} {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			c := NewTestContainer(t, image)
			require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{
				"PROCESS_OUTPUT_MAX_BYTES": "8388608", "PROCESS_OUTPUT_TOTAL_MAX_BYTES": "16777216",
			}}))
			defer c.Stop(context.Background())
			require.NoError(t, c.WaitReady(ctx))
			client, err := c.APIClient()
			require.NoError(t, err)

			t.Run("fast exits preserve status", func(t *testing.T) {
				for i := 0; i < 20; i++ {
					result, err := client.ProcessExecWithResponse(ctx, oapi.ProcessExecRequest{Command: "sh", Args: lo.ToPtr([]string{"-c", fmt.Sprintf("printf out; printf err >&2; exit %d", i)})})
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, result.StatusCode(), "%s", result.Body)
					require.Equal(t, i, *result.JSON200.ExitCode)
					require.Equal(t, "b3V0", *result.JSON200.StdoutB64)
					require.Equal(t, "ZXJy", *result.JSON200.StderrB64)
				}
			})

			t.Run("unread output and explicit replay", func(t *testing.T) {
				spawn, err := client.ProcessSpawnWithResponse(ctx, oapi.ProcessSpawnRequest{Command: "sh", Args: lo.ToPtr([]string{"-c", "head -c 4194304 /dev/zero; printf tail; printf stderr >&2"})})
				require.NoError(t, err)
				require.NotNil(t, spawn.JSON200, "%s", spawn.Body)
				id := spawn.JSON200.ProcessId
				status := waitImageProcessExit(t, ctx, client, *spawn.JSON200)
				require.Equal(t, 0, *status.ExitCode)
				require.False(t, *status.OutputTruncated)
				response, err := client.ProcessStdoutStream(ctx, *id, &oapi.ProcessStdoutStreamParams{})
				require.NoError(t, err)
				out, errOut, events := readImageProcessStream(t, response)
				require.Equal(t, strings.Repeat("\x00", 4<<20)+"tail", out)
				require.Equal(t, "stderr", errOut)
				requireImageExitEvent(t, events, 0)

				response, err = client.ProcessStdoutStream(ctx, *id, &oapi.ProcessStdoutStreamParams{})
				require.NoError(t, err)
				out, errOut, events = readImageProcessStream(t, response)
				require.Empty(t, out)
				require.Empty(t, errOut)
				require.Empty(t, events)

				response, err = client.ProcessStdoutStream(ctx, *id, &oapi.ProcessStdoutStreamParams{Offset: lo.ToPtr(int64(0))})
				require.NoError(t, err)
				out, errOut, events = readImageProcessStream(t, response)
				require.Equal(t, strings.Repeat("\x00", 4<<20)+"tail", out)
				require.Equal(t, "stderr", errOut)
				requireImageExitEvent(t, events, 0)
				// Resume just after the first event and compare the remaining records.
				response, err = client.ProcessStdoutStream(ctx, *id, &oapi.ProcessStdoutStreamParams{
					Offset: lo.ToPtr(int64(0)), LastEventID: lo.ToPtr(strconv.FormatInt(*events[0].Offset, 10)),
				})
				require.NoError(t, err)
				_, _, resumed := readImageProcessStream(t, response)
				require.Equal(t, events[1:], resumed)
			})

			t.Run("exec deadlines stop children", func(t *testing.T) {
				for _, script := range []string{"sleep 30 & echo $!; wait", "sleep 30 & echo $!"} {
					result, err := client.ProcessExecWithResponse(ctx, oapi.ProcessExecRequest{Command: "sh", Args: lo.ToPtr([]string{"-c", script}), TimeoutSec: lo.ToPtr(1)})
					require.NoError(t, err)
					require.Equal(t, http.StatusUnprocessableEntity, result.StatusCode(), "%s", result.Body)
					require.Equal(t, oapi.ProcessTimeout, result.JSON422.Code)
					data, err := base64.StdEncoding.DecodeString(*result.JSON422.StdoutB64)
					require.NoError(t, err)
					pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
					require.NoError(t, err)
					require.Eventually(t, func() bool {
						code, _, err := c.Exec(ctx, []string{"sh", "-c", fmt.Sprintf("test ! -e /proc/%d/stat || grep -q ') Z ' /proc/%d/stat", pid, pid)})
						return err == nil && code == 0
					}, 2*time.Second, 20*time.Millisecond, "child %d remains alive", pid)
				}
			})

			t.Run("spawn deadlines include tty", func(t *testing.T) {
				for _, tty := range []bool{false, true} {
					spawn, err := client.ProcessSpawnWithResponse(ctx, oapi.ProcessSpawnRequest{Command: "sleep", Args: lo.ToPtr([]string{"30"}), TimeoutSec: lo.ToPtr(1), AllocateTty: &tty})
					require.NoError(t, err)
					require.NotNil(t, spawn.JSON200, "%s", spawn.Body)
					status := waitImageProcessExit(t, ctx, client, *spawn.JSON200)
					require.Equal(t, oapi.ProcessTimeout, *status.TerminationReason)
					response, err := client.ProcessStdoutStream(ctx, *spawn.JSON200.ProcessId, &oapi.ProcessStdoutStreamParams{})
					require.NoError(t, err)
					_, _, events := readImageProcessStream(t, response)
					requireImageExitEvent(t, events, -1)
					require.Equal(t, oapi.ProcessTimeout, *events[len(events)-1].TerminationReason)
				}
			})

			t.Run("quota failure is present at first exited status", func(t *testing.T) {
				spawn, err := client.ProcessSpawnWithResponse(ctx, oapi.ProcessSpawnRequest{Command: "head", Args: lo.ToPtr([]string{"-c", "12582912", "/dev/zero"})})
				require.NoError(t, err)
				require.NotNil(t, spawn.JSON200, "%s", spawn.Body)
				status := waitImageProcessExit(t, ctx, client, *spawn.JSON200)
				require.NotNil(t, status.TerminationReason)
				require.Equal(t, oapi.OutputLimit, *status.TerminationReason)
				require.True(t, *status.OutputTruncated)
				response, err := client.ProcessStdoutStream(ctx, *spawn.JSON200.ProcessId, &oapi.ProcessStdoutStreamParams{})
				require.NoError(t, err)
				out, _, events := readImageProcessStream(t, response)
				require.NotEmpty(t, out)
				require.Less(t, len(out), 8<<20)
				require.Equal(t, oapi.OutputLimit, *events[len(events)-1].TerminationReason)
				require.True(t, *events[len(events)-1].OutputTruncated)
			})
		})
	}
}

func TestProcessTTYWithoutOutputStorage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := NewTestContainer(t, headlessImage)
	require.NoError(t, c.Start(ctx, ContainerConfig{Env: map[string]string{"PROCESS_OUTPUT_DIR": "/proc/unavailable"}}))
	defer c.Stop(context.Background())
	require.NoError(t, c.WaitReady(ctx))
	client, err := c.APIClient()
	require.NoError(t, err)
	spawn, err := client.ProcessSpawnWithResponse(ctx, oapi.ProcessSpawnRequest{
		Command: "sh", Args: lo.ToPtr([]string{"-c", "read line; printf 'reply:%s\\n' \"$line\"; read line"}),
		AllocateTty: lo.ToPtr(true), Rows: lo.ToPtr(24), Cols: lo.ToPtr(80),
	})
	require.NoError(t, err)
	require.NotNil(t, spawn.JSON200, "%s", spawn.Body)
	defer client.ProcessKillWithResponse(context.Background(), *spawn.JSON200.ProcessId, oapi.ProcessKillRequest{Signal: "KILL"})
	url := strings.Replace(c.APIBaseURL(), "http", "ws", 1) + "/process/" + spawn.JSON200.ProcessId.String() + "/attach"
	conn, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	resized, err := client.ProcessResizeWithResponse(ctx, *spawn.JSON200.ProcessId, oapi.ProcessResizeRequest{Rows: 30, Cols: 100})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resized.StatusCode())
	require.NoError(t, conn.Write(ctx, websocket.MessageBinary, []byte("hello\n")))
	var output strings.Builder
	for !strings.Contains(output.String(), "reply:hello") {
		kind, data, err := conn.Read(ctx)
		require.NoError(t, err)
		if kind == websocket.MessageBinary {
			output.Write(data)
		}
	}
	// Non-TTY capture still fails before running commands when storage is unavailable.
	result, err := client.ProcessExecWithResponse(ctx, oapi.ProcessExecRequest{Command: "true"})
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, result.StatusCode())
}

func waitImageProcessExit(t *testing.T, ctx context.Context, client *oapi.ClientWithResponses, spawn oapi.ProcessSpawnResult) *oapi.ProcessStatus {
	t.Helper()
	var status *oapi.ProcessStatus
	require.Eventually(t, func() bool {
		response, err := client.ProcessStatusWithResponse(ctx, *spawn.ProcessId)
		require.NoError(t, err)
		require.NotNil(t, response.JSON200, "%s", response.Body)
		status = response.JSON200
		return *status.State == "exited"
	}, 5*time.Second, 10*time.Millisecond)
	return status
}

func readImageProcessStream(t *testing.T, response *http.Response) (string, string, []oapi.ProcessStreamEvent) {
	t.Helper()
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var out, errOut strings.Builder
	events := make([]oapi.ProcessStreamEvent, 0)
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		if !strings.HasPrefix(scanner.Text(), "data: ") {
			continue
		}
		var event oapi.ProcessStreamEvent
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &event))
		events = append(events, event)
		if event.DataB64 != nil {
			data, err := base64.StdEncoding.DecodeString(*event.DataB64)
			require.NoError(t, err)
			if *event.Stream == "stdout" {
				out.Write(data)
			} else {
				errOut.Write(data)
			}
		}
	}
	require.NoError(t, scanner.Err())
	return out.String(), errOut.String(), events
}

func requireImageExitEvent(t *testing.T, events []oapi.ProcessStreamEvent, code int) {
	t.Helper()
	require.NotEmpty(t, events)
	last := events[len(events)-1]
	require.NotNil(t, last.Event)
	require.Equal(t, oapi.Exit, *last.Event)
	require.Equal(t, code, *last.ExitCode)
	for _, event := range events[:len(events)-1] {
		require.Nil(t, event.Event)
	}
}
