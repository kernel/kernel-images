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
	wd, _ := os.Getwd()
	return filepath.Join(wd, "..", "..", "..")
}

func newBrowserReplSvc(t *testing.T) *ApiService {
	t.Helper()
	script := ensureBrowserReplBundle(t)

	t.Setenv("BROWSER_REPL_SCRIPT", script)
	t.Setenv("BROWSER_REPL_SOCKET", filepath.Join(t.TempDir(), "browser-repl.sock"))
	if os.Getenv("NODE_PATH") == "" {
		if out, err := exec.Command("npm", "root", "-g").Output(); err == nil {
			root := string(out)
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

func execCode(t *testing.T, svc *ApiService, code string) oapi.ExecuteBrowserCode200JSONResponse {
	t.Helper()
	return execBrowserCode(t, svc, &oapi.ExecuteBrowserCodeJSONRequestBody{Code: code})
}

func requireExec(t *testing.T, svc *ApiService, code string, want any) oapi.ExecuteBrowserCode200JSONResponse {
	t.Helper()
	resp := execCode(t, svc, code)
	require.True(t, resp.Success, "code %q failed: %v", code, resp.Error)
	require.Equal(t, want, resp.Result, "code: %q", code)
	return resp
}

func requireExecError(t *testing.T, svc *ApiService, code, contains string) oapi.ExecuteBrowserCode200JSONResponse {
	t.Helper()
	resp := execCode(t, svc, code)
	require.False(t, resp.Success, "expected code %q to fail", code)
	require.NotNil(t, resp.Error)
	require.Contains(t, *resp.Error, contains, "code: %q", code)
	return resp
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
