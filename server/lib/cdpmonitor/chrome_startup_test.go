package cdpmonitor

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadDevToolsURLWaitsForCompleteLine(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	defer reader.Close()
	defer writer.Close()
	type result struct {
		url string
		err error
	}
	ready := make(chan result, 1)
	go func() {
		url, err := readDevToolsURL(reader, time.Now().Add(2*time.Second))
		ready <- result{url, err}
	}()
	_, err = io.WriteString(writer, "startup log\nDevTools listening on ws://127.0.0.1:")
	require.NoError(t, err)
	select {
	case got := <-ready:
		t.Fatalf("accepted incomplete URL: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
	_, err = io.WriteString(writer, "1234/devtools/browser/test\n")
	require.NoError(t, err)
	select {
	case got := <-ready:
		require.NoError(t, got.err)
		require.Equal(t, "ws://127.0.0.1:1234/devtools/browser/test", got.url)
	case <-time.After(3 * time.Second):
		t.Fatal("complete URL was not reported")
	}
}

func TestReadDevToolsURLBoundsSilentStartup(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	defer reader.Close()
	defer writer.Close()
	_, err = readDevToolsURL(reader, time.Now().Add(20*time.Millisecond))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
}

func TestReadDevToolsURLIncludesFailedStartupOutput(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	defer reader.Close()
	defer writer.Close()
	_, err = io.WriteString(writer, "startup failed\n")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	_, err = readDevToolsURL(reader, time.Now().Add(time.Second))
	require.ErrorIs(t, err, io.EOF)
	require.ErrorContains(t, err, "startup failed")
}
