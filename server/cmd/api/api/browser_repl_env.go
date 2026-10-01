package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/oapi"
)

const (
	maxBrowserReplEnvBodyBytes  = 1 << 20
	maxBrowserReplEnvVars       = 100
	maxBrowserReplEnvNameBytes  = 256
	maxBrowserReplEnvValueBytes = 32 * 1024

	// browserReplEnvApplyTimeout bounds delivering an environment update to a
	// running REPL. The daemon applies it synchronously, so this only covers
	// transport time.
	browserReplEnvApplyTimeout = 5 * time.Second
)

var browserReplEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Variables the API or the REPL daemon owns, or that change how Node starts.
var reservedBrowserReplEnvNames = []string{"CDP_ENDPOINT", "KERNEL_API_ENDPOINT", "PORT", "NODE_OPTIONS"}

func validateBrowserReplEnv(env map[string]string) error {
	if len(env) > maxBrowserReplEnvVars {
		return fmt.Errorf("env may contain at most %d variables", maxBrowserReplEnvVars)
	}
	for name, value := range env {
		if len(name) > maxBrowserReplEnvNameBytes || !browserReplEnvNamePattern.MatchString(name) {
			return fmt.Errorf("invalid env name %q", name)
		}
		if strings.HasPrefix(name, "BROWSER_REPL_") || slices.Contains(reservedBrowserReplEnvNames, name) {
			return fmt.Errorf("env name %q is reserved", name)
		}
		if len(value) > maxBrowserReplEnvValueBytes {
			return fmt.Errorf("env value for %q exceeds %d bytes", name, maxBrowserReplEnvValueBytes)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("env value for %q contains a NUL byte", name)
		}
	}
	return nil
}

// browserReplEnvChanges lists the updates that turn the previous variables
// into next. A removed variable falls back to the API process's own value, the
// same value a freshly started REPL inherits; nil unsets it.
func browserReplEnvChanges(previous, next map[string]string) map[string]*string {
	changes := make(map[string]*string)
	for name, value := range next {
		if old, ok := previous[name]; !ok || old != value {
			changes[name] = &value
		}
	}
	for name := range previous {
		if _, ok := next[name]; ok {
			continue
		}
		if inherited, ok := os.LookupEnv(name); ok {
			changes[name] = &inherited
		} else {
			changes[name] = nil
		}
	}
	return changes
}

func (m *browserReplManager) envList() []string {
	m.envMu.Lock()
	defer m.envMu.Unlock()
	list := make([]string, 0, len(m.env))
	for name, value := range m.env {
		list = append(list, name+"="+value)
	}
	return list
}

func (m *browserReplManager) envNames() []string {
	m.envMu.Lock()
	defer m.envMu.Unlock()
	names := make([]string, 0, len(m.env))
	return append(names, slices.Sorted(maps.Keys(m.env))...)
}

// SetEnv replaces the variables added to every REPL child and applies the
// change to the running one. It reports whether the running REPL had to be
// terminated because it could not receive the change.
func (m *browserReplManager) SetEnv(ctx context.Context, env map[string]string) (bool, error) {
	if err := m.acquire(ctx); err != nil {
		return false, err
	}
	defer m.release()

	m.envMu.Lock()
	previous := m.env
	m.env = maps.Clone(env)
	m.envMu.Unlock()
	// The stored variables already changed, so finish delivering them even if
	// the caller goes away; the update is bounded by its own timeout.
	return m.applyEnvLocked(context.WithoutCancel(ctx), browserReplEnvChanges(previous, env)), nil
}

// ClearEnv drops the variables, for example when a fork takes its own
// identity. Later REPL children start without them right away; the running
// REPL loses them once no execution holds admission. It does not block.
func (m *browserReplManager) ClearEnv() {
	m.envMu.Lock()
	previous := m.env
	m.env = nil
	m.envMu.Unlock()
	if len(previous) == 0 {
		return
	}
	go func() {
		if err := m.acquire(m.lifecycle); err != nil {
			return
		}
		defer m.release()
		m.envMu.Lock()
		current := maps.Clone(m.env)
		m.envMu.Unlock()
		m.applyEnvLocked(m.lifecycle, browserReplEnvChanges(previous, current))
	}()
}

// applyEnvLocked sends changes to the running REPL. A REPL that cannot apply
// them is terminated so the next request starts one with the stored
// variables. The caller must hold admission.
func (m *browserReplManager) applyEnvLocked(ctx context.Context, changes map[string]*string) bool {
	child := m.child
	if child == nil || len(changes) == 0 {
		return false
	}
	select {
	case err := <-child.done:
		// Already gone; ensureLocked replaces it on the next execution.
		child.done = closedWaitChannel(err)
		return false
	default:
	}

	if err := m.sendEnvLocked(ctx, changes); err != nil {
		logger.FromContext(ctx).Error("browser REPL env update failed; terminating child", "repl_id", child.id, "error", err)
		m.terminateLocked(ctx, "env update failure")
		return true
	}
	return false
}

func (m *browserReplManager) sendEnvLocked(ctx context.Context, changes map[string]*string) error {
	request, err := prepareBrowserReplEnvRequest(changes)
	if err != nil {
		return err
	}
	resp, err := m.executeLocked(ctx, request, browserReplEnvApplyTimeout)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("browser REPL rejected env update: %s", resp.Error)
	}
	return nil
}

// browserReplDaemonEnvRequest is the wire format of an environment update. A
// nil value unsets the variable.
type browserReplDaemonEnvRequest struct {
	ID  string             `json:"id"`
	Env map[string]*string `json:"env"`
}

func prepareBrowserReplEnvRequest(changes map[string]*string) (*browserReplRequest, error) {
	id := uuid.New().String()
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(browserReplDaemonEnvRequest{ID: id, Env: changes}); err != nil {
		return nil, fmt.Errorf("failed to marshal env request: %w", err)
	}
	return &browserReplRequest{id: id, bytes: buf.Bytes()}, nil
}

func (s *ApiService) GetBrowserReplEnv(ctx context.Context, request oapi.GetBrowserReplEnvRequestObject) (oapi.GetBrowserReplEnvResponseObject, error) {
	return oapi.GetBrowserReplEnv200JSONResponse{Names: s.browserRepl.envNames()}, nil
}

func (s *ApiService) SetBrowserReplEnv(ctx context.Context, request oapi.SetBrowserReplEnvRequestObject) (oapi.SetBrowserReplEnvResponseObject, error) {
	if request.Body == nil || request.Body.Env == nil {
		return oapi.SetBrowserReplEnv400JSONResponse{
			BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "env is required"},
		}, nil
	}
	if err := validateBrowserReplEnv(request.Body.Env); err != nil {
		return oapi.SetBrowserReplEnv400JSONResponse{
			BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()},
		}, nil
	}
	terminated, err := s.browserRepl.SetEnv(ctx, request.Body.Env)
	if err != nil {
		return nil, err
	}
	return oapi.SetBrowserReplEnv200JSONResponse(browserReplEnvResponse(s.browserRepl.envNames(), terminated)), nil
}

func (s *ApiService) DeleteBrowserReplEnv(ctx context.Context, request oapi.DeleteBrowserReplEnvRequestObject) (oapi.DeleteBrowserReplEnvResponseObject, error) {
	terminated, err := s.browserRepl.SetEnv(ctx, map[string]string{})
	if err != nil {
		return nil, err
	}
	return oapi.DeleteBrowserReplEnv200JSONResponse(browserReplEnvResponse(s.browserRepl.envNames(), terminated)), nil
}

// ClearBrowserReplEnv drops the Browser REPL variables without blocking.
func (s *ApiService) ClearBrowserReplEnv() {
	s.browserRepl.ClearEnv()
}

func browserReplEnvResponse(names []string, terminated bool) oapi.BrowserReplEnv {
	resp := oapi.BrowserReplEnv{Names: names}
	if terminated {
		resp.ReplTerminated = &terminated
	}
	return resp
}
