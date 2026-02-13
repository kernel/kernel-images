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

	"github.com/onkernel/kernel-images/server/lib/chromiumflags"
	"github.com/onkernel/kernel-images/server/lib/logger"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/onkernel/kernel-images/server/lib/policy"
	"github.com/onkernel/kernel-images/server/lib/ziputil"
)

var nameRegex = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)

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

	// Materialize uploads
	extBase := "/home/kernel/extensions"

	// Fail early if any destination already exists
	for _, p := range items {
		dest := filepath.Join(extBase, p.name)
		if _, err := os.Stat(dest); err == nil {
			return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: fmt.Sprintf("extension name already exists: %s", p.name)}}, nil
		} else if !os.IsNotExist(err) {
			log.Error("failed to check extension dir", "error", err)
			return oapi.UploadExtensionsAndRestart500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to check extension dir"}}, nil
		}
	}

	for _, p := range items {
		if !p.zipReceived || p.name == "" {
			return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "each item must include zip_file and name"}}, nil
		}
		dest := filepath.Join(extBase, p.name)
		if err := os.MkdirAll(dest, 0o755); err != nil {
			log.Error("failed to create extension dir", "error", err)
			return oapi.UploadExtensionsAndRestart500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to create extension dir"}}, nil
		}
		if err := ziputil.Unzip(p.zipTemp, dest); err != nil {
			log.Error("failed to unzip zip file", "error", err)
			return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid zip file"}}, nil
		}

		// Rewrite update.xml URLs to match the extension name (directory name)
		// This ensures URLs like /extensions/web-bot-auth/ become /extensions/<actual-name>/
		updateXMLPath := filepath.Join(dest, "update.xml")
		if err := policy.RewriteUpdateXMLUrls(updateXMLPath, p.name); err != nil {
			log.Warn("failed to rewrite update.xml URLs", "error", err, "extension", p.name)
			// continue since not all extensions require update.xml
		}

		if err := exec.Command("chown", "-R", "kernel:kernel", dest).Run(); err != nil {
			log.Error("failed to chown extension dir", "error", err)
			return oapi.UploadExtensionsAndRestart500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to chown extension dir"}}, nil
		}

		log.Info("installed extension", "name", p.name)
	}

	// Update enterprise policy for extensions that require it
	// Track which extensions need --load-extension flags (those NOT using policy installation)
	var pathsNeedingFlags []string

	for _, p := range items {
		extensionPath := filepath.Join(extBase, p.name)
		extensionName := p.name
		manifestPath := filepath.Join(extensionPath, "manifest.json")
		updateXMLPath := filepath.Join(extensionPath, "update.xml")

		// Check if this extension requires enterprise policy
		requiresEntPolicy, err := s.policy.RequiresEnterprisePolicy(manifestPath)
		if err != nil {
			log.Warn("failed to read manifest for policy check", "error", err, "extension", extensionName)
			// Continue with requiresEntPolicy = false
		}

		// Try to extract Chrome extension ID from update.xml
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

			// Validate that update.xml and .crx files are present for policy-installed extensions
			// These files are required for ExtensionInstallForcelist to work
			hasUpdateXML := false
			hasCRX := false

			if _, err := os.Stat(updateXMLPath); err == nil {
				// For policy extensions, update.xml must exist AND be parseable
				if extractionErr != nil {
					return oapi.UploadExtensionsAndRestart400JSONResponse{
						BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{
							Message: fmt.Sprintf("extension %s requires enterprise policy but update.xml is invalid: %v", extensionName, extractionErr),
						},
					}, nil
				}
				hasUpdateXML = true
				log.Info("found update.xml in extension zip", "name", extensionName)
			}

			// Look for any .crx file in the directory
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

			// If missing required files for ExtensionInstallForcelist, fall back to --load-extension
			if !hasUpdateXML || !hasCRX {
				log.Info("extension missing policy files, falling back to --load-extension",
					"name", extensionName, "hasUpdateXML", hasUpdateXML, "hasCRX", hasCRX)
				requiresEntPolicy = false
				pathsNeedingFlags = append(pathsNeedingFlags, extensionPath)
			}
		} else {
			// Only add --load-extension flags for non-policy extensions
			pathsNeedingFlags = append(pathsNeedingFlags, extensionPath)
		}

		// Add to enterprise policy
		// Pass both extensionName (for URL paths) and chromeExtensionID (for policy entries)
		if err := s.policy.AddExtension(extensionName, chromeExtensionID, extensionPath, requiresEntPolicy); err != nil {
			log.Error("failed to update enterprise policy", "error", err, "extension", extensionName)
			return oapi.UploadExtensionsAndRestart500JSONResponse{
				InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{
					Message: fmt.Sprintf("failed to update enterprise policy for %s: %v", extensionName, err),
				},
			}, nil
		}

		log.Info("updated enterprise policy", "extension", extensionName, "chromeExtensionID", chromeExtensionID, "requiresEnterprisePolicy", requiresEntPolicy)
	}

	// Build flags overlay file in /chromium/flags, merging with existing flags
	// Only add --load-extension flags for extensions that don't use policy installation
	// NOTE: We intentionally do NOT use --disable-extensions-except here because it causes
	// Chrome to disable external providers (including the policy loader), which prevents
	// enterprise policy extensions (ExtensionInstallForcelist) from being fetched and installed.
	// See Chromium source: extension_service.cc - external providers are only created when
	// extensions_enabled() returns true, which is false when --disable-extensions-except is used.
	var newTokens []string
	if len(pathsNeedingFlags) > 0 {
		newTokens = []string{
			fmt.Sprintf("--load-extension=%s", strings.Join(pathsNeedingFlags, ",")),
		}
	}

	// Merge and write flags
	if _, err := s.mergeAndWriteChromiumFlags(ctx, newTokens); err != nil {
		return oapi.UploadExtensionsAndRestart500JSONResponse{
			InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: err.Error()},
		}, nil
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

// mergeAndWriteChromiumFlags reads existing flags, merges them with new flags,
// and writes the result back to /chromium/flags. Returns the merged tokens or an error.
func (s *ApiService) mergeAndWriteChromiumFlags(ctx context.Context, newTokens []string) ([]string, error) {
	log := logger.FromContext(ctx)
	s.chromiumConfigMu.Lock()
	defer s.chromiumConfigMu.Unlock()

	// Read existing runtime flags from /chromium/flags (if any)
	existingTokens, err := chromiumflags.ReadOptionalFlagFile(s.chromiumFlagsPath)
	if err != nil {
		log.Error("failed to read existing flags", "error", err)
		return nil, fmt.Errorf("failed to read existing flags: %w", err)
	}

	log.Info("merging flags", "existing", existingTokens, "new", newTokens)

	// Merge existing flags with new flags using token-aware API
	mergedTokens := chromiumflags.MergeFlags(existingTokens, newTokens)

	// Ensure the chromium directory exists
	if err := os.MkdirAll(filepath.Dir(s.chromiumFlagsPath), 0o755); err != nil {
		log.Error("failed to create chromium dir", "error", err)
		return nil, fmt.Errorf("failed to create chromium dir: %w", err)
	}

	// Write flags file with merged flags
	if err := chromiumflags.WriteFlagFile(s.chromiumFlagsPath, mergedTokens); err != nil {
		log.Error("failed to write flags", "error", err)
		return nil, fmt.Errorf("failed to write flags: %w", err)
	}

	log.Info("flags written", "merged", mergedTokens)
	return mergedTokens, nil
}

// restartChromiumAndWait restarts Chromium via supervisorctl and waits for DevTools to be ready.
// Returns an error if the restart fails or times out.
func (s *ApiService) restartChromiumAndWait(ctx context.Context, operation string) error {
	log := logger.FromContext(ctx)
	start := time.Now()

	// Begin listening for devtools URL updates, since we are about to restart Chromium
	updates, cancelSub := s.upstreamMgr.Subscribe()
	defer cancelSub()

	// Run supervisorctl restart with a new context to let it run beyond the lifetime of the http request.
	// This lets us return as soon as the DevTools URL is updated.
	errCh := make(chan error, 1)
	log.Info("restarting chromium via supervisorctl", "operation", operation)
	go func() {
		cmdCtx, cancelCmd := context.WithTimeout(context.WithoutCancel(ctx), 1*time.Minute)
		defer cancelCmd()
		out, err := exec.CommandContext(cmdCtx, "supervisorctl", "-c", "/etc/supervisor/supervisord.conf", "restart", "chromium").CombinedOutput()
		if err != nil {
			log.Error("failed to restart chromium", "error", err, "out", string(out))
			errCh <- fmt.Errorf("supervisorctl restart failed: %w", err)
		}
	}()

	// Wait for either a new upstream, a restart error, or timeout
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()
	select {
	case <-updates:
		log.Info("devtools ready", "operation", operation, "elapsed", time.Since(start).String())
		return nil
	case err := <-errCh:
		return err
	case <-timeout.C:
		log.Info("devtools not ready in time", "operation", operation, "elapsed", time.Since(start).String())
		return fmt.Errorf("devtools not ready in time")
	}
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
