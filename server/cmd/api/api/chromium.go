package api

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/chromiumflags"
	"github.com/kernel/kernel-images/server/lib/logger"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/policy"
	"github.com/kernel/kernel-images/server/lib/ziputil"
)

var nameRegex = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)

// extensionZipItem is a finalized name + temp zip path (caller removes temps).
type extensionZipItem struct {
	zipTemp string
	name    string
}

// chromiumFlagsPath is the runtime flags file read by the chromium-launcher at startup.
const chromiumFlagsPath = "/chromium/flags"

// UploadExtensionsAndRestart handles multipart upload of one or more extension zips, extracts
// them under /home/kernel/extensions/<name>, writes /chromium/flags to enable them, restarts
// Chromium via supervisord, and waits (via UpstreamManager) until DevTools is ready.
func (s *ApiService) UploadExtensionsAndRestart(ctx context.Context, request oapi.UploadExtensionsAndRestartRequestObject) (oapi.UploadExtensionsAndRestartResponseObject, error) {
	log := logger.FromContext(ctx)
	start := time.Now()
	log.Info("upload extensions: begin")

	if request.Body == nil {
		return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}

	// Strict handler gives us *multipart.Reader; use NextPart() directly
	mr, ok := any(request.Body).(interface {
		NextPart() (*multipart.Part, error)
	})
	if !ok {
		return oapi.UploadExtensionsAndRestart500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "multipart reader not available"}}, nil
	}

	temps := []string{}
	defer func() {
		for _, p := range temps {
			_ = os.Remove(p)
		}
	}()

	type pending struct {
		zipTemp     string
		name        string
		zipReceived bool
	}
	// Process consecutive pairs of fields:
	//   extensions.name (text)
	//   extensions.zip_file (file)
	// Order may be name then zip or zip then name, but they must be consecutive.
	items := []pending{}
	var current *pending

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Error("read form part", "error", err)
			return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "failed to read form part"}}, nil
		}
		if current == nil {
			current = &pending{}
		}
		switch part.FormName() {
		case "extensions.zip_file":
			tmp, err := os.CreateTemp("", "ext-*.zip")
			if err != nil {
				log.Error("failed to create temporary file", "error", err)
				return oapi.UploadExtensionsAndRestart500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "internal error"}}, nil
			}
			temps = append(temps, tmp.Name())
			if _, err := io.Copy(tmp, part); err != nil {
				tmp.Close()
				log.Error("failed to read zip file", "error", err)
				return oapi.UploadExtensionsAndRestart500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to read zip file"}}, nil
			}
			if err := tmp.Close(); err != nil {
				log.Error("failed to finalize temporary file", "error", err)
				return oapi.UploadExtensionsAndRestart500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "internal error"}}, nil
			}
			if current.zipReceived {
				return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "duplicate zip_file in pair"}}, nil
			}
			current.zipTemp = tmp.Name()
			current.zipReceived = true
		case "extensions.name":
			b, err := io.ReadAll(part)
			if err != nil {
				log.Error("failed to read name", "error", err)
				return oapi.UploadExtensionsAndRestart500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to read name"}}, nil
			}
			name := strings.TrimSpace(string(b))
			if name == "" || !nameRegex.MatchString(name) {
				return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid extension name"}}, nil
			}
			if current.name != "" {
				return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "duplicate name in pair"}}, nil
			}
			current.name = name
		default:
			return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: fmt.Sprintf("invalid field: %s", part.FormName())}}, nil
		}
		// If we have both fields, finalize this item
		if current != nil && current.zipReceived && current.name != "" {
			items = append(items, *current)
			current = nil
		}
	}

	// If the last pair is incomplete, reject the request
	if current != nil && (!current.zipReceived || current.name == "") {
		return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "each extension must include consecutive name and zip_file"}}, nil
	}

	if len(items) == 0 {
		return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "no extensions provided"}}, nil
	}

	extItems := make([]extensionZipItem, 0, len(items))
	for _, p := range items {
		if !p.zipReceived || p.name == "" {
			return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "each item must include zip_file and name"}}, nil
		}
		extItems = append(extItems, extensionZipItem{zipTemp: p.zipTemp, name: p.name})
	}

	reqMsg, err := s.applyExtensionZipItems(ctx, extItems)
	if reqMsg != "" {
		return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: reqMsg}}, nil
	}
	if err != nil {
		return oapi.UploadExtensionsAndRestart500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()}}, nil
	}

	// Restart Chromium and wait for DevTools to be ready
	if err := s.restartChromiumAndWait(ctx, "extension upload"); err != nil {
		return oapi.UploadExtensionsAndRestart500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()},
		}, nil
	}

	log.Info("devtools ready", "elapsed", time.Since(start).String())
	return oapi.UploadExtensionsAndRestart201Response{}, nil
}

// applyExtensionZipItems applies name+zipTemp extension pairs (merge flags for --load-extension).
// On validation errors returns (reqMsg, nil); on internal errors returns ("", err).
func (s *ApiService) applyExtensionZipItems(ctx context.Context, items []extensionZipItem) (reqMsg string, err error) {
	log := logger.FromContext(ctx)
	extBase := "/home/kernel/extensions"
	if err := os.MkdirAll(extBase, 0o755); err != nil {
		return "", fmt.Errorf("failed to create extension base dir: %w", err)
	}

	for _, p := range items {
		dest := filepath.Join(extBase, p.name)
		if _, err := os.Stat(dest); err == nil {
			return fmt.Sprintf("extension name already exists: %s", p.name), nil
		} else if !os.IsNotExist(err) {
			log.Error("failed to check extension dir", "error", err)
			return "", fmt.Errorf("failed to check extension dir: %w", err)
		}
	}

	var createdDests []string
	success := false
	defer func() {
		if success {
			return
		}
		for _, dest := range createdDests {
			if removeErr := os.RemoveAll(dest); removeErr != nil {
				log.Warn("failed to clean up partial extension dir", "error", removeErr, "dest", dest)
			}
		}
	}()

	for _, p := range items {
		dest := filepath.Join(extBase, p.name)
		if err := os.MkdirAll(dest, 0o755); err != nil {
			log.Error("failed to create extension dir", "error", err)
			return "", fmt.Errorf("failed to create extension dir: %w", err)
		}
		createdDests = append(createdDests, dest)
		if err := ziputil.Unzip(p.zipTemp, dest); err != nil {
			log.Error("failed to unzip zip file", "error", err)
			return "invalid zip file", nil
		}

		updateXMLPath := filepath.Join(dest, "update.xml")
		if err := policy.RewriteUpdateXMLUrls(updateXMLPath, p.name); err != nil {
			log.Warn("failed to rewrite update.xml URLs", "error", err, "extension", p.name)
		}

		if err := exec.Command("chown", "-R", "kernel:kernel", dest).Run(); err != nil {
			log.Error("failed to chown extension dir", "error", err)
			return "", fmt.Errorf("failed to chown extension dir: %w", err)
		}

		log.Info("installed extension", "name", p.name)
	}

	var pathsNeedingFlags []string

	for _, p := range items {
		extensionPath := filepath.Join(extBase, p.name)
		extensionName := p.name
		manifestPath := filepath.Join(extensionPath, "manifest.json")
		updateXMLPath := filepath.Join(extensionPath, "update.xml")

		requiresEntPolicy, err := s.policy.RequiresEnterprisePolicy(manifestPath)
		if err != nil {
			log.Warn("failed to read manifest for policy check", "error", err, "extension", extensionName)
		}

		chromeExtensionID := extensionName
		var extractionErr error
		if extractedID, err := policy.ExtractExtensionIDFromUpdateXML(updateXMLPath); err == nil {
			chromeExtensionID = extractedID
			log.Info("extracted Chrome extension ID from update.xml", "name", extensionName, "chromeExtensionID", chromeExtensionID)
		} else {
			extractionErr = err
			log.Info("no Chrome extension ID in update.xml, using name as ID", "name", extensionName, "error", err)
		}

		if requiresEntPolicy {
			log.Info("extension requires enterprise policy", "name", extensionName)

			hasUpdateXML := false
			hasCRX := false

			if _, err := os.Stat(updateXMLPath); err == nil {
				if extractionErr != nil {
					return fmt.Sprintf("extension %s requires enterprise policy but update.xml is invalid: %v", extensionName, extractionErr), nil
				}
				hasUpdateXML = true
				log.Info("found update.xml in extension zip", "name", extensionName)
			}

			entries, err := os.ReadDir(extensionPath)
			if err == nil {
				for _, entry := range entries {
					if !entry.IsDir() && filepath.Ext(entry.Name()) == ".crx" {
						hasCRX = true
						log.Info("found .crx file in extension zip", "name", extensionName, "crx_file", entry.Name())
						break
					}
				}
			}

			if !hasUpdateXML || !hasCRX {
				log.Info("extension missing policy files, falling back to --load-extension",
					"name", extensionName, "hasUpdateXML", hasUpdateXML, "hasCRX", hasCRX)
				requiresEntPolicy = false
				pathsNeedingFlags = append(pathsNeedingFlags, extensionPath)
			}
		} else {
			pathsNeedingFlags = append(pathsNeedingFlags, extensionPath)
		}

		if err := s.policy.AddExtension(extensionName, chromeExtensionID, extensionPath, requiresEntPolicy); err != nil {
			log.Error("failed to update enterprise policy", "error", err, "extension", extensionName)
			return "", fmt.Errorf("failed to update enterprise policy for %s: %w", extensionName, err)
		}

		log.Info("updated enterprise policy", "extension", extensionName, "chromeExtensionID", chromeExtensionID, "requiresEnterprisePolicy", requiresEntPolicy)
	}

	var newTokens []string
	if len(pathsNeedingFlags) > 0 {
		newTokens = []string{
			fmt.Sprintf("--load-extension=%s", strings.Join(pathsNeedingFlags, ",")),
		}
	}

	if _, err := s.mergeAndWriteChromiumFlags(ctx, newTokens); err != nil {
		return "", err
	}

	success = true
	return "", nil
}

// mergeAndWriteChromiumFlags reads existing flags, merges them with new flags,
// and writes the result back to chromiumFlagsPath. Returns the merged tokens or an error.
func (s *ApiService) mergeAndWriteChromiumFlags(ctx context.Context, newTokens []string) ([]string, error) {
	log := logger.FromContext(ctx)

	// Read existing runtime flags (if any)
	existingTokens, err := chromiumflags.ReadOptionalFlagFile(chromiumFlagsPath)
	if err != nil {
		log.Error("failed to read existing flags", "error", err)
		return nil, fmt.Errorf("failed to read existing flags: %w", err)
	}

	log.Info("merging flags", "existing", existingTokens, "new", newTokens)

	// Merge existing flags with new flags using token-aware API
	mergedTokens := chromiumflags.MergeFlags(existingTokens, newTokens)

	if err := writeChromiumFlags(mergedTokens); err != nil {
		log.Error("failed to write flags", "error", err)
		return nil, err
	}

	log.Info("flags written", "merged", mergedTokens)
	return mergedTokens, nil
}

// writeChromiumFlags ensures the /chromium directory exists and writes tokens
// to chromiumFlagsPath. Shared by mergeAndWriteChromiumFlags and ensureAppMode.
func writeChromiumFlags(tokens []string) error {
	if err := os.MkdirAll("/chromium", 0o755); err != nil {
		return fmt.Errorf("failed to create chromium dir: %w", err)
	}
	if err := chromiumflags.WriteFlagFile(chromiumFlagsPath, tokens); err != nil {
		return fmt.Errorf("failed to write flags file: %w", err)
	}
	return nil
}

// restartChromiumAndWait restarts Chromium via supervisorctl and waits for DevTools to be ready.
// Returns an error if the restart fails or times out.
func (s *ApiService) restartChromiumAndWait(ctx context.Context, operation string) error {
	log := logger.FromContext(ctx)
	start := time.Now()

	log.Info("restarting chromium via supervisorctl", "operation", operation)
	if err := s.stopChromium(ctx); err != nil {
		return err
	}
	if err := s.startChromiumAndWait(ctx, operation); err != nil {
		return err
	}
	log.Info("chromium restart complete", "operation", operation, "elapsed", time.Since(start).String())
	return nil
}

const supervisorCtlConf = "/etc/supervisor/supervisord.conf"
const chromiumDevToolsReadyTimeout = 90 * time.Second

func supervisorctlArgv(verb string, prog string) []string {
	return []string{"-c", supervisorCtlConf, verb, prog}
}

func chromiumSupervisorStatus(ctx context.Context) (string, string, error) {
	cmdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, "supervisorctl", supervisorctlArgv("status", "chromium")...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	fields := strings.Fields(text)
	if len(fields) >= 2 {
		return fields[1], text, nil
	}
	if err != nil {
		return "", text, err
	}
	return "", text, fmt.Errorf("unexpected supervisorctl status output: %q", text)
}

func waitChromiumSupervisorStatus(ctx context.Context, want string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		status, out, err := chromiumSupervisorStatus(ctx)
		if err == nil && status == want {
			return out, nil
		}
		if out != "" {
			last = out
		}
		if time.Now().After(deadline) {
			if err != nil {
				return last, err
			}
			return last, fmt.Errorf("chromium did not reach %s within %s (last status: %s)", want, timeout, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// stopChromium runs supervisorctl stop chromium and waits for the command to complete.
//
// On success it emits a "chromium stopped" info log with both string (`elapsed`)
// and numeric (`elapsed_ms`) duration attributes, plus an `outcome` field that
// distinguishes the success-after-error recovery paths from the canonical
// success path. The numeric attribute is what aggregations like
// p99(elapsed_ms) in SigNoz key off; the string is kept for parity with the
// existing `startChromiumAndWait` "devtools ready" log shape.
func (s *ApiService) stopChromium(ctx context.Context) error {
	log := logger.FromContext(ctx)
	start := time.Now()
	cmdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	log.Info("stopping chromium via supervisorctl")
	out, err := exec.CommandContext(cmdCtx, "supervisorctl", supervisorctlArgv("stop", "chromium")...).CombinedOutput()
	if err != nil {
		log.Error("failed to stop chromium", "error", err, "out", string(out))
		status, statusOut, statusErr := chromiumSupervisorStatus(ctx)
		if statusErr == nil {
			switch status {
			case "STOPPED":
				elapsed := time.Since(start)
				log.Info("chromium stopped",
					"outcome", "already_stopped",
					"elapsed", elapsed.String(),
					"elapsed_ms", elapsed.Milliseconds(),
					"status", statusOut,
				)
				return nil
			case "STOPPING":
				if stoppedOut, waitErr := waitChromiumSupervisorStatus(ctx, "STOPPED", 30*time.Second); waitErr == nil {
					elapsed := time.Since(start)
					log.Info("chromium stopped",
						"outcome", "stopping_then_stopped",
						"elapsed", elapsed.String(),
						"elapsed_ms", elapsed.Milliseconds(),
						"status", stoppedOut,
					)
					return nil
				}
			}
		}
		return fmt.Errorf("supervisorctl stop chromium failed: %w", err)
	}
	confirmed := true
	if stoppedOut, waitErr := waitChromiumSupervisorStatus(ctx, "STOPPED", 30*time.Second); waitErr != nil {
		confirmed = false
		log.Warn("chromium stop command completed but stopped status was not confirmed", "error", waitErr, "status", stoppedOut)
	}
	elapsed := time.Since(start)
	outcome := "success"
	if !confirmed {
		outcome = "success_unconfirmed"
	}
	log.Info("chromium stopped",
		"outcome", outcome,
		"elapsed", elapsed.String(),
		"elapsed_ms", elapsed.Milliseconds(),
	)
	return nil
}

// startChromiumAndWait launches chromium via supervisorctl start and waits
// for DevTools to actually serve the new Chromium browser.
//
// Readiness is gated on two independent signals being satisfied together:
//
//  1. The UpstreamManager has observed a "DevTools listening on ws://..." log
//     line whose URL differs from the URL we saw on entry (prevUpstream).
//     UpstreamManager only updates its current URL after the new Chromium
//     prints that line, so a new URL is the earliest reliable evidence that
//     the new Chromium has bound its DevTools listener.
//  2. After dialing that new URL, a Browser.getVersion CDP round-trip
//     succeeds. This rules out two failure modes that a bare websocket.Dial
//     does not: a dial completing against a half-closed socket from the
//     just-killed previous Chromium, or against a freshly bound TCP
//     listener that has not yet wired up CDP routes. Either case can
//     otherwise produce a false "ready" return, after which the new
//     Chromium can take several more seconds to actually serve requests
//     (and live view will appear blank during that window).
//
// We intentionally do NOT short-circuit on the supervisorctl start command
// returning ("doneCh") -- that command returns as soon as supervisord ack's
// the fork, long before the new Chromium has bound any ports.
func (s *ApiService) startChromiumAndWait(ctx context.Context, operation string) error {
	log := logger.FromContext(ctx)
	start := time.Now()

	prevUpstream := s.upstreamMgr.Current()
	updates, cancelSub := s.upstreamMgr.Subscribe()
	defer cancelSub()

	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	log.Info("starting chromium via supervisorctl", "operation", operation)
	go func() {
		defer close(doneCh)
		cmdCtx, cancelCmd := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancelCmd()
		out, err := exec.CommandContext(cmdCtx, "supervisorctl", supervisorctlArgv("start", "chromium")...).CombinedOutput()
		if err != nil {
			log.Error("failed to start chromium", "error", err, "out", string(out))
			errCh <- fmt.Errorf("supervisorctl start chromium failed: %w", err)
		}
	}()

	timeout := time.NewTimer(chromiumDevToolsReadyTimeout)
	defer timeout.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	tryReady := func(upstream string) bool {
		if upstream == "" || upstream == prevUpstream {
			return false
		}
		dialCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		c, err := cdpclient.Dial(dialCtx, upstream)
		if err != nil {
			return false
		}
		defer c.Close()
		if _, err := c.GetBrowserVersion(dialCtx); err != nil {
			log.Debug("dial succeeded but Browser.getVersion failed; ignoring", "operation", operation, "url", upstream, "err", err)
			return false
		}
		return true
	}

	for {
		select {
		case upstream, ok := <-updates:
			if ok && tryReady(upstream) {
				log.Info("devtools ready", "operation", operation, "elapsed", time.Since(start).String())
				return nil
			}
		case err := <-errCh:
			return err
		case <-doneCh:
			// supervisorctl start returned; the new chromium has been forked
			// but its DevTools listener may not be bound yet. Do not try
			// ready against the current upstream here -- it may still be the
			// previous chromium's URL. Continue waiting for either the
			// updates channel or the ticker to pick up the new URL.
			doneCh = nil
		case <-ticker.C:
			if tryReady(s.upstreamMgr.Current()) {
				log.Info("devtools ready", "operation", operation, "elapsed", time.Since(start).String())
				return nil
			}
		case <-timeout.C:
			status, statusOut, _ := chromiumSupervisorStatus(ctx)
			log.Info("devtools not ready in time", "operation", operation, "elapsed", time.Since(start).String(), "supervisor_status", statusOut)
			return fmt.Errorf("devtools not ready in time (chromium status: %s)", status)
		}
	}
}

// PatchChromiumPolicies applies user-provided Chromium enterprise policy overrides
// to policy.json, restarts Chromium, and waits for DevTools to be ready.
func (s *ApiService) PatchChromiumPolicies(ctx context.Context, request oapi.PatchChromiumPoliciesRequestObject) (oapi.PatchChromiumPoliciesResponseObject, error) {
	log := logger.FromContext(ctx)
	start := time.Now()
	log.Info("patch chromium policies: begin")

	if request.Body == nil || len(*request.Body) == 0 {
		return oapi.PatchChromiumPolicies400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required with at least one policy"}}, nil
	}

	overrides, err := policy.NewChromiumPolicyOverrides(map[string]interface{}(*request.Body))
	if err != nil {
		return oapi.PatchChromiumPolicies400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
	}

	if err := s.policy.ApplyOverrides(overrides); err != nil {
		if strings.Contains(err.Error(), "invalid chromium policy overrides") || strings.Contains(err.Error(), "cannot be overridden") {
			return oapi.PatchChromiumPolicies400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
		}
		log.Error("failed to apply policy overrides", "error", err)
		return oapi.PatchChromiumPolicies500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()}}, nil
	}

	log.Info("policy overrides applied, restarting chromium")

	if err := s.restartChromiumAndWait(ctx, "policy update"); err != nil {
		return oapi.PatchChromiumPolicies500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()}}, nil
	}

	log.Info("devtools ready after policy update", "elapsed", time.Since(start).String())
	return oapi.PatchChromiumPolicies200Response{}, nil
}

// PatchChromiumFlags handles updating Chromium launch flags at runtime.
// It merges the provided flags with existing flags in /chromium/flags, writes the updated
// flags file, restarts Chromium via supervisord, and waits until DevTools is ready.
func (s *ApiService) PatchChromiumFlags(ctx context.Context, request oapi.PatchChromiumFlagsRequestObject) (oapi.PatchChromiumFlagsResponseObject, error) {
	log := logger.FromContext(ctx)
	start := time.Now()
	log.Info("patch chromium flags: begin")

	if request.Body == nil {
		return oapi.PatchChromiumFlags400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body required"}}, nil
	}

	if len(request.Body.Flags) == 0 {
		return oapi.PatchChromiumFlags400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "at least one flag required"}}, nil
	}

	// Validate flags - they should start with "--"
	for _, flag := range request.Body.Flags {
		trimmed := strings.TrimSpace(flag)
		if trimmed == "" {
			return oapi.PatchChromiumFlags400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "empty flag provided"}}, nil
		}
		if !strings.HasPrefix(trimmed, "--") {
			return oapi.PatchChromiumFlags400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: fmt.Sprintf("invalid flag format: %s (must start with --)", flag)}}, nil
		}
	}

	// Merge and write flags
	if _, err := s.mergeAndWriteChromiumFlags(ctx, request.Body.Flags); err != nil {
		return oapi.PatchChromiumFlags500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()},
		}, nil
	}

	// Restart Chromium and wait for DevTools to be ready
	if err := s.restartChromiumAndWait(ctx, "flags update"); err != nil {
		return oapi.PatchChromiumFlags500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()},
		}, nil
	}

	log.Info("devtools ready after flags update", "elapsed", time.Since(start).String())
	return oapi.PatchChromiumFlags200Response{}, nil
}
