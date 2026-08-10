package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/recorder"
	"github.com/stretchr/testify/require"
)

// browserReplBundlePath is built once per test run from the runtime sources,
// mirroring how the Docker images bundle the daemon.
var (
	browserReplBundleOnce sync.Once
	browserReplBundlePath string
	browserReplBundleErr  error
)

func ensureBrowserReplBundle(t *testing.T) string {
	t.Helper()
	browserReplBundleOnce.Do(func() {
		if _, err := exec.LookPath("node"); err != nil {
			browserReplBundleErr = fmt.Errorf("node not available: %w", err)
			return
		}
		if _, err := exec.LookPath("esbuild"); err != nil {
			browserReplBundleErr = fmt.Errorf("esbuild not available: %w", err)
			return
		}
		dir, err := os.MkdirTemp("", "browser-repl-bundle")
		if err != nil {
			browserReplBundleErr = err
			return
		}
		browserReplBundlePath = filepath.Join(dir, "browser-repl.js")

		// Stage the runtime package and sources exactly like the Docker builds.
		// Meriyah is installed from the exact package lock, never globally.
		stagingDir, err := os.MkdirTemp("", "browser-repl-runtime")
		if err != nil {
			browserReplBundleErr = err
			return
		}
		defer os.RemoveAll(stagingDir)
		entries, err := os.ReadDir(filepath.Join(serverRootDir(), "runtime"))
		if err != nil {
			browserReplBundleErr = err
			return
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".ts") && name != "package.json" && name != "package-lock.json" {
				continue
			}
			data, readErr := os.ReadFile(filepath.Join(serverRootDir(), "runtime", name))
			if readErr != nil {
				browserReplBundleErr = readErr
				return
			}
			if writeErr := os.WriteFile(filepath.Join(stagingDir, name), data, 0o644); writeErr != nil {
				browserReplBundleErr = writeErr
				return
			}
		}
		npm := exec.Command("npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund", "--omit=dev")
		npm.Dir = stagingDir
		if out, err := npm.CombinedOutput(); err != nil {
			browserReplBundleErr = fmt.Errorf("npm ci failed: %w\n%s", err, out)
			return
		}

		cmd := exec.Command("esbuild",
			"browser-repl.ts",
			"--bundle",
			"--platform=node",
			"--target=node22",
			"--format=cjs",
			"--supported:dynamic-import=true",
			"--outfile="+browserReplBundlePath,
		)
		cmd.Dir = stagingDir
		if out, err := cmd.CombinedOutput(); err != nil {
			browserReplBundleErr = fmt.Errorf("esbuild failed: %w\n%s", err, out)
		}
	})
	if browserReplBundleErr != nil {
		t.Skipf("cannot build browser REPL bundle: %v", browserReplBundleErr)
	}
	return browserReplBundlePath
}

func serverRootDir() string {
	// cmd/api/api -> server root is three directories up.
	wd, _ := os.Getwd()
	return filepath.Join(wd, "..", "..", "..")
}

// newBrowserReplSvc builds an ApiService wired to a freshly built REPL
// bundle on a unique socket, and registers cleanup that kills the child.
func newBrowserReplSvc(t *testing.T) *ApiService {
	t.Helper()
	script := ensureBrowserReplBundle(t)

	t.Setenv("BROWSER_REPL_SCRIPT", script)
	t.Setenv("BROWSER_REPL_SOCKET", filepath.Join(t.TempDir(), "browser-repl.sock"))
	if os.Getenv("NODE_PATH") == "" {
		if out, err := exec.Command("npm", "root", "-g").Output(); err == nil {
			root := string(out)
			// trim trailing newline
			for len(root) > 0 && (root[len(root)-1] == '\n' || root[len(root)-1] == '\r') {
				root = root[:len(root)-1]
			}
			if root != "" {
				t.Setenv("NODE_PATH", root)
			}
		}
	}

	svc, err := newSvc(t, recorder.NewFFmpegManager())
	require.NoError(t, err)
	t.Cleanup(func() {
		svc.browserReplMu.Lock()
		svc.terminateBrowserReplLocked(context.Background(), "test cleanup")
		svc.browserReplMu.Unlock()
	})
	return svc
}

func execBrowserCode(t *testing.T, svc *ApiService, body *oapi.ExecuteBrowserCodeJSONRequestBody) oapi.ExecuteBrowserCode200JSONResponse {
	t.Helper()
	resp, err := svc.ExecuteBrowserCode(context.Background(), oapi.ExecuteBrowserCodeRequestObject{Body: body})
	require.NoError(t, err)
	typed, ok := resp.(oapi.ExecuteBrowserCode200JSONResponse)
	require.True(t, ok, "expected 200 response, got %T", resp)
	return typed
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
