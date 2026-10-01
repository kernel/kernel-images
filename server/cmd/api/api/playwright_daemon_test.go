package api

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pointDaemonSocketAt redirects the package's socket path at a temp file and
// shortens the wait so a test does not sit on the production budget.
func pointDaemonSocketAt(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "pd.sock")
	origSocket, origStartup := playwrightDaemonSocket, playwrightDaemonStartup
	playwrightDaemonSocket, playwrightDaemonStartup = sock, 750*time.Millisecond
	t.Cleanup(func() { playwrightDaemonSocket, playwrightDaemonStartup = origSocket, origStartup })
	return sock
}

func listenAt(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
}

func TestPlaywrightDaemonReachable(t *testing.T) {
	sock := pointDaemonSocketAt(t)
	require.False(t, playwrightDaemonReachable(playwrightDaemonDial), "no socket should not be reachable")
	listenAt(t, sock)
	require.True(t, playwrightDaemonReachable(playwrightDaemonDial))
}

func TestPlaywrightDaemonReachableIgnoresAStaleSocketFile(t *testing.T) {
	sock := pointDaemonSocketAt(t)
	// A daemon killed without cleanup leaves the file behind; it must not read
	// as a running daemon.
	require.NoError(t, os.WriteFile(sock, nil, 0o644))
	require.False(t, playwrightDaemonReachable(playwrightDaemonDial))
}

func TestWaitForPlaywrightDaemonPicksUpALateSocket(t *testing.T) {
	sock := pointDaemonSocketAt(t)
	go func() {
		time.Sleep(150 * time.Millisecond)
		ln, err := net.Listen("unix", sock)
		if err == nil {
			t.Cleanup(func() { ln.Close() })
		}
	}()
	require.True(t, waitForPlaywrightDaemon(time.Now().Add(playwrightDaemonStartup)))
}

func TestSupervisedDaemonIsWaitedForRatherThanSpawned(t *testing.T) {
	pointDaemonSocketAt(t)
	t.Setenv("PLAYWRIGHT_DAEMON_SUPERVISED", "true")

	svc := &ApiService{}
	err := svc.ensurePlaywrightDaemon(context.Background())

	require.ErrorContains(t, err, "supervised playwright daemon did not come back")
	require.Nil(t, svc.playwrightDaemonCmd, "supervised mode must not spawn a competing daemon")
}

func TestSupervisedDaemonSucceedsOnceSupervisordRestartsIt(t *testing.T) {
	sock := pointDaemonSocketAt(t)
	t.Setenv("PLAYWRIGHT_DAEMON_SUPERVISED", "true")
	go func() {
		time.Sleep(150 * time.Millisecond)
		ln, err := net.Listen("unix", sock)
		if err == nil {
			t.Cleanup(func() { ln.Close() })
		}
	}()

	svc := &ApiService{}
	require.NoError(t, svc.ensurePlaywrightDaemon(context.Background()))
	require.Nil(t, svc.playwrightDaemonCmd)
}
