package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/onkernel/kernel-images/server/lib/chromiumflags"
	"github.com/onkernel/kernel-images/server/lib/logger"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
)

const (
	chromiumPACFlagPrefix      = "--proxy-pac-url="
	chromiumPACChainName       = "KERNEL_PAC_PROXY"
	chromiumPACUDPChainName    = "KERNEL_PAC_PROXY_UDP"
	chromiumPACTransparentPort = "15080"
	chromiumPACProxyUser       = "pacproxy"
)

var errPACContentRequired = errors.New("content required when enabling PAC for the first time")

type chromiumPACOSApplyState struct {
	Enabled   bool   `json:"enabled"`
	Attempted bool   `json:"attempted"`
	Succeeded bool   `json:"succeeded"`
	Error     string `json:"error,omitempty"`
}

func (s *ApiService) chromiumPACServeURL() string {
	if strings.TrimSpace(s.chromiumPACURL) == "" {
		return "http://127.0.0.1:10001/chromium/proxy/pac/script"
	}
	return s.chromiumPACURL
}

// GetChromiumProxyPac returns the currently configured PAC state.
func (s *ApiService) GetChromiumProxyPac(ctx context.Context, _ oapi.GetChromiumProxyPacRequestObject) (oapi.GetChromiumProxyPacResponseObject, error) {
	cfg, err := func() (oapi.ChromiumProxyPacConfig, error) {
		s.chromiumConfigMu.Lock()
		defer s.chromiumConfigMu.Unlock()
		return s.getChromiumProxyPacConfigLocked()
	}()
	if err != nil {
		return oapi.GetChromiumProxyPac500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()},
		}, nil
	}

	return oapi.GetChromiumProxyPac200JSONResponse(cfg), nil
}

// GetChromiumProxyPacScript serves PAC content as text for API-managed PAC tooling.
func (s *ApiService) GetChromiumProxyPacScript(_ context.Context, _ oapi.GetChromiumProxyPacScriptRequestObject) (oapi.GetChromiumProxyPacScriptResponseObject, error) {
	content, err := func() (*string, error) {
		s.chromiumConfigMu.Lock()
		defer s.chromiumConfigMu.Unlock()
		return s.readPACContentLocked()
	}()
	if err != nil {
		return oapi.GetChromiumProxyPacScript500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()},
		}, nil
	}
	if content == nil || strings.TrimSpace(*content) == "" {
		return oapi.GetChromiumProxyPacScript404JSONResponse{
			NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "pac script not configured"},
		}, nil
	}

	return oapi.GetChromiumProxyPacScript200TextResponse(*content), nil
}

// PutChromiumProxyPac updates PAC script content and applies PAC at OS level.
func (s *ApiService) PutChromiumProxyPac(ctx context.Context, request oapi.PutChromiumProxyPacRequestObject) (oapi.PutChromiumProxyPacResponseObject, error) {
	log := logger.FromContext(ctx)
	if request.Body == nil {
		return oapi.PutChromiumProxyPac400JSONResponse{
			BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"},
		}, nil
	}

	restartRequested := false
	if request.Body.RestartChromium != nil {
		restartRequested = *request.Body.RestartChromium
	}

	if request.Body.Content != nil && strings.TrimSpace(*request.Body.Content) == "" {
		return oapi.PutChromiumProxyPac400JSONResponse{
			BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "content cannot be empty"},
		}, nil
	}

	cfg, err := func() (oapi.ChromiumProxyPacConfig, error) {
		s.chromiumConfigMu.Lock()
		defer s.chromiumConfigMu.Unlock()

		if err := os.MkdirAll(filepath.Dir(s.chromiumPACPath), 0o755); err != nil {
			return oapi.ChromiumProxyPacConfig{}, fmt.Errorf("failed to create PAC directory: %w", err)
		}

		content := request.Body.Content
		if content != nil {
			if err := os.WriteFile(s.chromiumPACPath, []byte(*content), 0o644); err != nil {
				return oapi.ChromiumProxyPacConfig{}, fmt.Errorf("failed to write PAC file: %w", err)
			}
		}

		// Enabling PAC requires either new content or a previously saved PAC file.
		if request.Body.Enabled && content == nil {
			currentContent, err := s.readPACContentLocked()
			if err != nil {
				return oapi.ChromiumProxyPacConfig{}, err
			}
			if currentContent == nil || strings.TrimSpace(*currentContent) == "" {
				return oapi.ChromiumProxyPacConfig{}, errPACContentRequired
			}
		}

		// Remove any legacy Chromium PAC flag; PAC is now managed at OS level.
		tokens, err := chromiumflags.ReadOptionalFlagFile(s.chromiumFlagsPath)
		if err != nil {
			return oapi.ChromiumProxyPacConfig{}, fmt.Errorf("failed to read existing flags: %w", err)
		}
		cleaned := upsertPACFlag(tokens, nil)
		if err := chromiumflags.WriteFlagFile(s.chromiumFlagsPath, cleaned); err != nil {
			return oapi.ChromiumProxyPacConfig{}, fmt.Errorf("failed to write flags: %w", err)
		}

		var pacURL *string
		if request.Body.Enabled {
			pacURLStr := s.chromiumPACServeURL()
			pacURL = &pacURLStr
		}
		osState := applyPACAtOSLevel(ctx, pacURL)
		if err := s.writePACOSApplyStateLocked(osState); err != nil {
			return oapi.ChromiumProxyPacConfig{}, fmt.Errorf("failed to persist PAC OS-level state: %w", err)
		}
		if !osState.Succeeded && osState.Error != "" {
			log.Warn("os-level PAC apply failed", "error", osState.Error)
		}

		return s.getChromiumProxyPacConfigLocked()
	}()
	if err != nil {
		if errors.Is(err, errPACContentRequired) {
			return oapi.PutChromiumProxyPac400JSONResponse{
				BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()},
			}, nil
		}
		return oapi.PutChromiumProxyPac500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()},
		}, nil
	}

	restarted := false
	if restartRequested {
		if err := s.restartChromiumAndWait(ctx, "pac proxy update"); err != nil {
			return oapi.PutChromiumProxyPac500JSONResponse{
				InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()},
			}, nil
		}
		restarted = true
	}

	return oapi.PutChromiumProxyPac200JSONResponse{
		Config:            cfg,
		RestartRequested:  restartRequested,
		ChromiumRestarted: restarted,
	}, nil
}

func (s *ApiService) getChromiumProxyPacConfigLocked() (oapi.ChromiumProxyPacConfig, error) {
	content, err := s.readPACContentLocked()
	if err != nil {
		return oapi.ChromiumProxyPacConfig{}, err
	}

	osState, err := s.readPACOSApplyStateLocked()
	if err != nil {
		return oapi.ChromiumProxyPacConfig{}, err
	}

	var pacURL *string
	if osState.Enabled {
		pacURL = ptrOf(s.chromiumPACServeURL())
	}

	var pacPath *string
	if content != nil || osState.Enabled {
		pacPath = ptrOf(s.chromiumPACPath)
	}

	cfg := oapi.ChromiumProxyPacConfig{
		Enabled:                              osState.Enabled,
		Content:                              content,
		PacPath:                              pacPath,
		PacUrl:                               pacURL,
		RestartRequiredForImmediateApply:     false,
		DynamicUpdateWithoutRestartSupported: true,
		OsLevelApplyAttempted:                osState.Attempted,
		OsLevelApplySucceeded:                osState.Succeeded,
	}
	if osState.Error != "" {
		cfg.OsLevelApplyError = ptrOf(osState.Error)
	}

	return cfg, nil
}

func (s *ApiService) readPACContentLocked() (*string, error) {
	data, err := os.ReadFile(s.chromiumPACPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read PAC file: %w", err)
	}
	content := string(data)
	return &content, nil
}

func (s *ApiService) readPACOSApplyStateLocked() (chromiumPACOSApplyState, error) {
	state := chromiumPACOSApplyState{}

	data, err := os.ReadFile(s.chromiumPACStatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, fmt.Errorf("failed to read PAC state file: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return state, nil
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("failed to parse PAC state file: %w", err)
	}
	return state, nil
}

func (s *ApiService) writePACOSApplyStateLocked(state chromiumPACOSApplyState) error {
	if err := os.MkdirAll(filepath.Dir(s.chromiumPACStatePath), 0o755); err != nil {
		return err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return os.WriteFile(s.chromiumPACStatePath, encoded, 0o644)
}

func upsertPACFlag(tokens []string, pacURL *string) []string {
	out := make([]string, 0, len(tokens)+1)
	seen := map[string]struct{}{}

	for _, token := range tokens {
		t := strings.TrimSpace(token)
		if t == "" || strings.HasPrefix(t, chromiumPACFlagPrefix) {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}

	if pacURL != nil {
		pacFlag := chromiumPACFlagPrefix + *pacURL
		if _, ok := seen[pacFlag]; !ok {
			out = append(out, pacFlag)
		}
	}
	return out
}

func extractPACFlagValue(tokens []string) *string {
	for _, token := range tokens {
		t := strings.TrimSpace(token)
		if !strings.HasPrefix(t, chromiumPACFlagPrefix) {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(t, chromiumPACFlagPrefix))
		if value == "" {
			continue
		}
		return &value
	}
	return nil
}

func applyPACAtOSLevel(ctx context.Context, pacURL *string) chromiumPACOSApplyState {
	state := chromiumPACOSApplyState{
		Enabled: pacURL != nil,
	}

	if _, err := exec.LookPath("iptables"); err != nil {
		state.Attempted = true
		state.Error = err.Error()
		return state
	}

	state.Attempted = true
	uid, err := lookupUserUID(ctx, chromiumPACProxyUser)
	if err != nil {
		state.Error = err.Error()
		return state
	}

	if pacURL != nil {
		if err := enableTransparentPACRedirect(ctx, uid); err != nil {
			state.Error = err.Error()
			return state
		}
	} else {
		if err := disableTransparentPACRedirect(ctx, uid); err != nil {
			state.Error = err.Error()
			return state
		}
	}

	state.Succeeded = true
	return state
}

func lookupUserUID(ctx context.Context, username string) (string, error) {
	cmd := exec.CommandContext(ctx, "id", "-u", username)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to lookup uid for %s: %v: %s", username, err, strings.TrimSpace(string(out)))
	}
	uid := strings.TrimSpace(string(out))
	if uid == "" {
		return "", fmt.Errorf("empty uid returned for user %s", username)
	}
	return uid, nil
}

func enableTransparentPACRedirect(ctx context.Context, proxyUID string) error {
	if _, err := runIPTables(ctx, "-t", "nat", "-L", chromiumPACChainName); err != nil {
		if _, err := runIPTables(ctx, "-t", "nat", "-N", chromiumPACChainName); err != nil {
			return err
		}
	}

	steps := [][]string{
		{"-t", "nat", "-F", chromiumPACChainName},
		{"-t", "nat", "-A", chromiumPACChainName, "-d", "127.0.0.0/8", "-j", "RETURN"},
		{"-t", "nat", "-A", chromiumPACChainName, "-p", "tcp", "--dport", "80", "-j", "REDIRECT", "--to-ports", chromiumPACTransparentPort},
		{"-t", "nat", "-A", chromiumPACChainName, "-p", "tcp", "--dport", "443", "-j", "REDIRECT", "--to-ports", chromiumPACTransparentPort},
	}
	for _, step := range steps {
		if _, err := runIPTables(ctx, step...); err != nil {
			return err
		}
	}

	checkRule := []string{"-t", "nat", "-C", "OUTPUT", "-p", "tcp", "-m", "owner", "!", "--uid-owner", proxyUID, "-j", chromiumPACChainName}
	if _, err := runIPTables(ctx, checkRule...); err != nil {
		addRule := []string{"-t", "nat", "-A", "OUTPUT", "-p", "tcp", "-m", "owner", "!", "--uid-owner", proxyUID, "-j", chromiumPACChainName}
		if _, err := runIPTables(ctx, addRule...); err != nil {
			return err
		}
	}

	// Block QUIC (UDP/443) at OS level to prevent bypassing the TCP transparent proxy.
	if _, err := runIPTables(ctx, "-t", "filter", "-L", chromiumPACUDPChainName); err != nil {
		if _, err := runIPTables(ctx, "-t", "filter", "-N", chromiumPACUDPChainName); err != nil {
			return err
		}
	}
	udpSteps := [][]string{
		{"-t", "filter", "-F", chromiumPACUDPChainName},
		{"-t", "filter", "-A", chromiumPACUDPChainName, "-p", "udp", "--dport", "443", "-j", "REJECT"},
	}
	for _, step := range udpSteps {
		if _, err := runIPTables(ctx, step...); err != nil {
			return err
		}
	}
	checkUDPOutput := []string{"-t", "filter", "-C", "OUTPUT", "-p", "udp", "-m", "owner", "!", "--uid-owner", proxyUID, "-j", chromiumPACUDPChainName}
	if _, err := runIPTables(ctx, checkUDPOutput...); err != nil {
		addUDPOutput := []string{"-t", "filter", "-A", "OUTPUT", "-p", "udp", "-m", "owner", "!", "--uid-owner", proxyUID, "-j", chromiumPACUDPChainName}
		if _, err := runIPTables(ctx, addUDPOutput...); err != nil {
			return err
		}
	}

	return nil
}

func disableTransparentPACRedirect(ctx context.Context, proxyUID string) error {
	removeRule := []string{"-t", "nat", "-D", "OUTPUT", "-p", "tcp", "-m", "owner", "!", "--uid-owner", proxyUID, "-j", chromiumPACChainName}
	_, _ = runIPTables(ctx, removeRule...)
	_, _ = runIPTables(ctx, "-t", "nat", "-F", chromiumPACChainName)
	_, _ = runIPTables(ctx, "-t", "nat", "-X", chromiumPACChainName)

	removeUDPOutputRule := []string{"-t", "filter", "-D", "OUTPUT", "-p", "udp", "-m", "owner", "!", "--uid-owner", proxyUID, "-j", chromiumPACUDPChainName}
	_, _ = runIPTables(ctx, removeUDPOutputRule...)
	_, _ = runIPTables(ctx, "-t", "filter", "-F", chromiumPACUDPChainName)
	_, _ = runIPTables(ctx, "-t", "filter", "-X", chromiumPACUDPChainName)

	return nil
}

func runIPTables(ctx context.Context, args ...string) ([]byte, error) {
	argv := append([]string{"-w"}, args...)
	cmd := exec.CommandContext(ctx, "iptables", argv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("iptables %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
