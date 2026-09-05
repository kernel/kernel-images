package agentproxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const bridgeTokenEnv = "KERNEL_ACP_BRIDGE_TOKEN"

// Each attachment gets its own expose listener/process group. No connection or
// child process is retained for reattachment after the downstream closes.
type bridge struct {
	url     string
	headers http.Header
	command *exec.Cmd
	done    chan struct{}
	once    sync.Once
}

func startBridge(ctx context.Context, executable string, harness Harness) (*bridge, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(secret)
	env, err := harness.environment(token)
	if err != nil {
		return nil, err
	}
	args := []string{"expose", "--host", "127.0.0.1", "--port", "0", "--cwd", harness.Cwd, "--stderr-mode", "discard", "--token-env", bridgeTokenEnv, "--", harness.Command}
	args = append(args, harness.Args...)
	command := exec.Command(executable, args...)
	command.Env = env
	command.Dir = harness.Cwd
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Keep our reader separate from exec.Wait's pipe cleanup.
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	command.Stderr = writer
	if err := command.Start(); err != nil {
		reader.Close()
		writer.Close()
		return nil, errors.New("could not start ACP bridge")
	}
	writer.Close()
	b := &bridge{command: command, done: make(chan struct{}), headers: http.Header{"Authorization": {"Bearer " + token}}}
	go func() {
		_ = command.Wait()
		close(b.done)
	}()
	ready := make(chan string, 1)
	go func() {
		defer reader.Close()
		defer close(ready)
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			if address := bridgeAddress(scanner.Text()); address != "" {
				ready <- address
				// Never forward agent/bridge stderr into request logs or responses.
				_, _ = io.Copy(io.Discard, reader)
				return
			}
		}
	}()
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case address := <-ready:
		if address != "" {
			b.url = address
			return b, nil
		}
	case <-b.done:
	case <-startup.Done():
	}
	b.close()
	return nil, errors.New("ACP bridge did not become ready")
}

// acpremote 1.7.0 reports its OS-selected port on stderr. Restrict the advertised
// address to the listener we requested; never proxy an arbitrary URL from output.
func bridgeAddress(line string) string {
	address, ok := strings.CutPrefix(line, "Serving ACP WebSocket at ")
	if !ok {
		return ""
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "ws" || u.Hostname() != "127.0.0.1" || u.Path != "/acp/ws" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return ""
	}
	return address
}

func (b *bridge) close() {
	b.once.Do(func() {
		// The CLI handles SIGINT by closing its server and awaiting child cleanup.
		_ = b.command.Process.Signal(os.Interrupt)
		select {
		case <-b.done:
		case <-time.After(6 * time.Second):
		}
		// Terminate remaining descendants if an adapter outlives its bridge.
		_ = syscall.Kill(-b.command.Process.Pid, syscall.SIGKILL)
		<-b.done
	})
}
