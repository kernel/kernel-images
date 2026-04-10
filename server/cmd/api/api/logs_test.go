package api

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

func TestLogsStream_PathFollow(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc := &ApiService{}

	// create temp log file
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "app.log")
	if err := os.WriteFile(logPath, []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// start streaming with follow=true from end of file
	follow := true
	resp, err := svc.LogsStream(ctx, oapi.LogsStreamRequestObject{Params: oapi.LogsStreamParams{Source: "path", Follow: &follow, Path: &logPath}})
	if err != nil {
		t.Fatalf("LogsStream error: %v", err)
	}
	r200, ok := resp.(oapi.LogsStream200TexteventStreamResponse)
	if !ok {
		t.Fatalf("unexpected response type: %T", resp)
	}

	// write another line after starting
	go func() {
		time.Sleep(100 * time.Millisecond)
		f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
		defer f.Close()
		_, _ = f.WriteString("hello world\n")
	}()

	reader := bufio.NewReader(r200.Body)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for SSE line")
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if strings.Contains(payload, "hello world") {
				break
			}
		}
	}
}
