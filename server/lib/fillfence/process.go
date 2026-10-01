package fillfence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kernel/kernel-images/server/lib/forkidentity"
)

// Browser contains the process configuration supplied by chromium-launcher.
// Address is loopback-only in the image; tests use their own isolated listener.
type Browser struct {
	Command      *exec.Cmd
	Executable   string
	ProfileDir   string
	DevToolsPort string
	Address      string
	Identity     func() string
}

func (b Browser) Run(ctx context.Context) (result error) {
	// The listener is also the kernel-enforced single-owner exclusion. A second
	// launcher cannot start a browser while this owner still holds the port.
	listener, err := net.Listen("tcp4", b.Address)
	if err != nil {
		return err
	}
	defer listener.Close()
	stop := func() error {
		deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return StopPrevious(deadline, b.Executable)
	}
	if err := stop(); err != nil {
		return err
	}
	port, err := net.Listen("tcp4", "127.0.0.1:"+b.DevToolsPort)
	if err != nil {
		return fmt.Errorf("previous DevTools listener still present: %w", err)
	}
	port.Close()
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		if err := os.Remove(filepath.Join(b.ProfileDir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := b.Command.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- b.Command.Wait() }()
	// Keep the listener reserved until teardown has attempted to fence Chrome.
	// If this fails, the next launcher must still pass its own strict exit check.
	var server *http.Server
	defer func() {
		result = errors.Join(result, stop())
		if server != nil {
			server.Close()
		}
	}()
	upstream, err := b.waitUpstream(ctx)
	if err != nil {
		return err
	}
	server = &http.Server{Handler: New(upstream, b.Identity), ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-exited:
		return err
	case err := <-served:
		return err
	}
}

func (b Browser) waitUpstream(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := &http.Client{Timeout: time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+b.DevToolsPort+"/json/version", nil)
		if err != nil {
			return "", err
		}
		response, err := client.Do(req)
		if err == nil {
			var result struct {
				URL string `json:"webSocketDebuggerUrl"`
			}
			err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result)
			response.Body.Close()
			if err == nil && result.URL != "" {
				return result.URL, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("new browser not ready: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Identity never adopts a fork in place. New captures its birth identity; a
// template waiting for handoff is disabled for its entire owner lifetime. After
// handoff it needs a launcher restart (and confirmed old-Chrome exit) to fill.
func Identity() string {
	wait, err := forkidentity.WaitEnabled()
	if err != nil {
		return ""
	}
	if _, err := os.Stat(forkidentity.ReadyFile); err == nil {
		applied, err := forkidentity.ReadAppliedMarker()
		if err != nil || applied == "" {
			return ""
		}
		payload, err := forkidentity.ReadPayload()
		if err != nil || payload.InstanceName() != applied {
			return ""
		}
		return applied
	}
	if wait {
		return ""
	}
	return forkidentity.FirstNonEmpty(os.Getenv("INSTANCE_NAME"), os.Getenv("INST_NAME"))
}
