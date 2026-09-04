package api

import (
	"context"
	"errors"
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

const (
	// chromiumFlagsPath is the runtime flags file read by the chromium-launcher at startup.
	chromiumFlagsPath = "/chromium/flags"
	extensionsBaseDir = "/home/kernel/extensions"
)

// UploadExtensionsAndRestart uploads extensions and always restarts Chromium.
func (s *ApiService) UploadExtensionsAndRestart(ctx context.Context, request oapi.UploadExtensionsAndRestartRequestObject) (oapi.UploadExtensionsAndRestartResponseObject, error) {
	if uploadErr := s.uploadExtensions(ctx, request.Body, true); uploadErr != nil {
		if uploadErr.kind == extensionUploadBadRequest {
			return oapi.UploadExtensionsAndRestart400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: uploadErr.message}}, nil
		}
		return oapi.UploadExtensionsAndRestart500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: uploadErr.message}}, nil
	}
	return oapi.UploadExtensionsAndRestart201Response{}, nil
}

// UploadExtensions uploads extensions and activates ordinary unpacked extensions over CDP.
func (s *ApiService) UploadExtensions(ctx context.Context, request oapi.UploadExtensionsRequestObject) (oapi.UploadExtensionsResponseObject, error) {
	if uploadErr := s.uploadExtensions(ctx, request.Body, false); uploadErr != nil {
		if uploadErr.kind == extensionUploadBadRequest {
			return oapi.UploadExtensions400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: uploadErr.message}}, nil
		}
		return oapi.UploadExtensions500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: uploadErr.message}}, nil
	}
	return oapi.UploadExtensions201Response{}, nil
}

type extensionUploadErrorKind uint8

const (
	extensionUploadBadRequest extensionUploadErrorKind = iota
	extensionUploadInternal

	maxExtensionUploadCount    = 20
	maxExtensionZipBytes       = int64(50 << 20)
	extensionActivationTimeout = 30 * time.Second
)

type extensionUploadError struct {
	kind    extensionUploadErrorKind
	message string
}

func badExtensionUpload(message string) *extensionUploadError {
	return &extensionUploadError{kind: extensionUploadBadRequest, message: message}
}

func internalExtensionUpload(message string) *extensionUploadError {
	return &extensionUploadError{kind: extensionUploadInternal, message: message}
}

func (s *ApiService) uploadExtensions(ctx context.Context, mr *multipart.Reader, forceRestart bool) *extensionUploadError {
	log := logger.FromContext(ctx)
	start := time.Now()
	log.Info("upload extensions: begin")

	if mr == nil {
		return badExtensionUpload("request body required")
	}

	extItems, cleanup, uploadErr := readExtensionUpload(mr)
	defer cleanup()
	if uploadErr != nil {
		return uploadErr
	}

	prepared, reqMsg, err := s.prepareExtensionZipItems(ctx, extItems)
	if reqMsg != "" {
		return badExtensionUpload(reqMsg)
	}
	if err != nil {
		return internalExtensionUpload(err.Error())
	}
	defer prepared.cleanup()

	s.chromiumConfigMu.Lock()
	defer s.chromiumConfigMu.Unlock()

	transaction, reqMsg, err := s.commitPreparedExtensions(ctx, prepared)
	if reqMsg != "" {
		return badExtensionUpload(reqMsg)
	}
	if err != nil {
		return internalExtensionUpload(err.Error())
	}

	restarted := forceRestart || prepared.requiresRestart
	var loadErr error
	if restarted {
		if err := s.restartChromiumAndWait(ctx, "extension upload"); err != nil {
			return s.rollbackFailedExtensionActivation(ctx, transaction, err)
		}
	} else if loadErr = s.loadUnpackedExtensions(ctx, prepared.extensions); loadErr != nil {
		log.Warn("CDP extension load failed, restarting Chromium", "error", loadErr)
		if restartErr := s.restartChromiumAndWait(ctx, "extension upload fallback"); restartErr != nil {
			return s.rollbackFailedExtensionActivation(ctx, transaction, errors.Join(loadErr, restartErr))
		}
		restarted = true
	}
	if restarted && !forceRestart {
		if verifyErr := s.verifyUnpackedExtensions(ctx, prepared.extensions); verifyErr != nil {
			return s.rollbackFailedExtensionActivation(ctx, transaction, errors.Join(loadErr, verifyErr))
		}
	}

	log.Info("extensions ready", "restarted", restarted, "elapsed", time.Since(start).String())
	return nil
}

func readExtensionUpload(mr *multipart.Reader) ([]extensionZipItem, func(), *extensionUploadError) {
	temps := make([]string, 0)
	cleanup := func() {
		for _, path := range temps {
			_ = os.Remove(path)
		}
	}

	type pending struct {
		zipTemp     string
		name        string
		zipReceived bool
	}
	items := make([]extensionZipItem, 0)
	seenNames := make(map[string]struct{})
	var current *pending

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, cleanup, badExtensionUpload("failed to read form part")
		}
		if current == nil {
			if len(items) >= maxExtensionUploadCount {
				return nil, cleanup, badExtensionUpload(fmt.Sprintf("too many extensions; maximum is %d", maxExtensionUploadCount))
			}
			current = &pending{}
		}

		switch part.FormName() {
		case "extensions.zip_file":
			if current.zipReceived {
				return nil, cleanup, badExtensionUpload("duplicate zip_file in pair")
			}
			tmp, err := os.CreateTemp("", "ext-*.zip")
			if err != nil {
				return nil, cleanup, internalExtensionUpload("internal error")
			}
			temps = append(temps, tmp.Name())
			_, copyErr := copyExtensionZip(tmp, part, maxExtensionZipBytes)
			closeErr := tmp.Close()
			if copyErr != nil {
				return nil, cleanup, copyErr
			}
			if closeErr != nil {
				return nil, cleanup, internalExtensionUpload("internal error")
			}
			current.zipTemp = tmp.Name()
			current.zipReceived = true
		case "extensions.name":
			if current.name != "" {
				return nil, cleanup, badExtensionUpload("duplicate name in pair")
			}
			nameBytes, err := io.ReadAll(io.LimitReader(part, 256))
			if err != nil {
				return nil, cleanup, internalExtensionUpload("failed to read name")
			}
			name := strings.TrimSpace(string(nameBytes))
			if name == "" || !nameRegex.MatchString(name) {
				return nil, cleanup, badExtensionUpload("invalid extension name")
			}
			if _, exists := seenNames[name]; exists {
				return nil, cleanup, badExtensionUpload(fmt.Sprintf("duplicate extension name: %s", name))
			}
			current.name = name
		default:
			return nil, cleanup, badExtensionUpload(fmt.Sprintf("invalid field: %s", part.FormName()))
		}

		if current.zipReceived && current.name != "" {
			items = append(items, extensionZipItem{zipTemp: current.zipTemp, name: current.name})
			seenNames[current.name] = struct{}{}
			current = nil
		}
	}

	if current != nil {
		return nil, cleanup, badExtensionUpload("each extension must include consecutive name and zip_file")
	}
	if len(items) == 0 {
		return nil, cleanup, badExtensionUpload("no extensions provided")
	}
	return items, cleanup, nil
}

func copyExtensionZip(dst io.Writer, src io.Reader, maxBytes int64) (int64, *extensionUploadError) {
	written, err := io.Copy(dst, io.LimitReader(src, maxBytes+1))
	if err != nil {
		return written, internalExtensionUpload("failed to read zip file")
	}
	if written > maxBytes {
		return written, badExtensionUpload("extension zip exceeds maximum allowed size (50 MiB)")
	}
	return written, nil
}

type preparedExtension struct {
	name                     string
	stagingPath              string
	finalPath                string
	chromeExtensionID        string
	requiresEnterprisePolicy bool
}

type preparedExtensionBatch struct {
	stagingRoot     string
	extensions      []preparedExtension
	flagPaths       []string
	requiresRestart bool
}

func (batch *preparedExtensionBatch) cleanup() {
	if batch != nil && batch.stagingRoot != "" {
		_ = os.RemoveAll(batch.stagingRoot)
	}
}

// installExtensionZipItems extracts, validates, and persists extension archives.
// The caller must hold chromiumConfigMu while it commits the prepared batch.
func (s *ApiService) installExtensionZipItems(ctx context.Context, items []extensionZipItem) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	prepared, reqMsg, err := s.prepareExtensionZipItems(ctx, items)
	if prepared != nil {
		defer prepared.cleanup()
	}
	if reqMsg != "" || err != nil {
		return reqMsg, err
	}
	_, reqMsg, err = s.commitPreparedExtensions(ctx, prepared)
	return reqMsg, err
}

// prepareExtensionZipItems extracts and validates archives into staging paths.
// commitPreparedExtensions rechecks destination names while the caller holds the config lock.
func (s *ApiService) prepareExtensionZipItems(ctx context.Context, items []extensionZipItem) (*preparedExtensionBatch, string, error) {
	log := logger.FromContext(ctx)
	if err := os.MkdirAll(extensionsBaseDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("failed to create extension base dir: %w", err)
	}

	stagingRoot, err := os.MkdirTemp(extensionsBaseDir, ".upload-*")
	if err != nil {
		return nil, "", fmt.Errorf("failed to create extension staging dir: %w", err)
	}
	batch := &preparedExtensionBatch{
		stagingRoot: stagingRoot,
		extensions:  make([]preparedExtension, 0, len(items)),
		flagPaths:   make([]string, 0, len(items)),
	}
	failed := true
	defer func() {
		if failed {
			batch.cleanup()
		}
	}()

	seenNames := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, exists := seenNames[item.name]; exists {
			return nil, fmt.Sprintf("duplicate extension name: %s", item.name), nil
		}
		seenNames[item.name] = struct{}{}

		stagingPath := filepath.Join(stagingRoot, item.name)
		finalPath := filepath.Join(extensionsBaseDir, item.name)
		if err := os.Mkdir(stagingPath, 0o755); err != nil {
			return nil, "", fmt.Errorf("failed to create extension staging directory: %w", err)
		}
		if err := ziputil.Unzip(item.zipTemp, stagingPath); err != nil {
			return nil, "invalid zip file", nil
		}

		updateXMLPath := filepath.Join(stagingPath, "update.xml")
		if err := policy.RewriteUpdateXMLUrls(updateXMLPath, item.name); err != nil {
			log.Warn("failed to rewrite update.xml URLs", "error", err, "extension", item.name)
		}
		if err := exec.Command("chown", "-R", "kernel:kernel", stagingPath).Run(); err != nil {
			return nil, "", fmt.Errorf("failed to chown extension dir: %w", err)
		}

		requiresEnterprisePolicy, err := s.policy.RequiresEnterprisePolicy(filepath.Join(stagingPath, "manifest.json"))
		if err != nil {
			return nil, fmt.Sprintf("invalid extension %s: %v", item.name, err), nil
		}

		chromeExtensionID := item.name
		extractedID, extractionErr := policy.ExtractExtensionIDFromUpdateXML(updateXMLPath)
		if extractionErr == nil {
			chromeExtensionID = extractedID
			log.Info("extracted Chrome extension ID from update.xml", "name", item.name, "chromeExtensionID", chromeExtensionID)
		}

		if requiresEnterprisePolicy {
			hasUpdateXML := false
			hasCRX := false
			if _, err := os.Stat(updateXMLPath); err == nil {
				if extractionErr != nil {
					return nil, fmt.Sprintf("extension %s requires enterprise policy but update.xml is invalid: %v", item.name, extractionErr), nil
				}
				hasUpdateXML = true
			} else if !os.IsNotExist(err) {
				return nil, "", fmt.Errorf("failed to inspect update.xml for %s: %w", item.name, err)
			}

			entries, err := os.ReadDir(stagingPath)
			if err != nil {
				return nil, "", fmt.Errorf("failed to inspect extension %s: %w", item.name, err)
			}
			for _, entry := range entries {
				if !entry.IsDir() && filepath.Ext(entry.Name()) == ".crx" {
					hasCRX = true
					break
				}
			}

			if !hasUpdateXML || !hasCRX {
				log.Info("extension missing policy files, falling back to --load-extension",
					"name", item.name, "hasUpdateXML", hasUpdateXML, "hasCRX", hasCRX)
				requiresEnterprisePolicy = false
			} else {
				batch.requiresRestart = true
			}
		}

		if !requiresEnterprisePolicy {
			batch.flagPaths = append(batch.flagPaths, finalPath)
		}
		batch.extensions = append(batch.extensions, preparedExtension{
			name:                     item.name,
			stagingPath:              stagingPath,
			finalPath:                finalPath,
			chromeExtensionID:        chromeExtensionID,
			requiresEnterprisePolicy: requiresEnterprisePolicy,
		})
	}

	failed = false
	return batch, "", nil
}

type optionalFileSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func captureOptionalFileSnapshot(path string) (optionalFileSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return optionalFileSnapshot{path: path}, nil
		}
		return optionalFileSnapshot{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return optionalFileSnapshot{}, err
	}
	return optionalFileSnapshot{path: path, data: data, mode: info.Mode().Perm(), exists: true}, nil
}

func restoreOptionalFileSnapshot(snapshot optionalFileSnapshot) error {
	if !snapshot.exists {
		if err := os.Remove(snapshot.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(snapshot.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(snapshot.path, snapshot.data, snapshot.mode)
}

type committedExtensionBatch struct {
	paths          []string
	flagsSnapshot  optionalFileSnapshot
	policySnapshot optionalFileSnapshot
}

func (batch *committedExtensionBatch) rollback() error {
	var rollbackErr error
	for _, path := range batch.paths {
		if err := os.RemoveAll(path); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove extension directory %s: %w", path, err))
		}
	}
	if err := restoreOptionalFileSnapshot(batch.policySnapshot); err != nil {
		rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore chromium policy: %w", err))
	}
	if err := restoreOptionalFileSnapshot(batch.flagsSnapshot); err != nil {
		rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore chromium flags: %w", err))
	}
	return rollbackErr
}

// commitPreparedExtensions moves a prepared batch into place and updates policy and
// flags, rolling back partial commit failures. Callers may retain the returned
// transaction to roll back a later activation failure.
func (s *ApiService) commitPreparedExtensions(ctx context.Context, batch *preparedExtensionBatch) (transaction *committedExtensionBatch, reqMsg string, err error) {
	for _, extension := range batch.extensions {
		if _, statErr := os.Stat(extension.finalPath); statErr == nil {
			return nil, fmt.Sprintf("extension name already exists: %s", extension.name), nil
		} else if !os.IsNotExist(statErr) {
			return nil, "", fmt.Errorf("failed to check extension dir: %w", statErr)
		}
	}

	flagsSnapshot, err := captureOptionalFileSnapshot(chromiumFlagsPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to snapshot chromium flags: %w", err)
	}
	policySnapshot, err := captureOptionalFileSnapshot(policy.PolicyPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to snapshot chromium policy: %w", err)
	}

	transaction = &committedExtensionBatch{
		paths:          make([]string, 0, len(batch.extensions)),
		flagsSnapshot:  flagsSnapshot,
		policySnapshot: policySnapshot,
	}
	rollbackTransaction := transaction
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := rollbackTransaction.rollback(); rollbackErr != nil {
			reqMsg = ""
			err = errors.Join(err, fmt.Errorf("rollback extension installation: %w", rollbackErr))
		}
		transaction = nil
	}()

	registrations := make([]policy.ExtensionRegistration, 0, len(batch.extensions))
	for _, extension := range batch.extensions {
		if err := os.Rename(extension.stagingPath, extension.finalPath); err != nil {
			return nil, "", fmt.Errorf("commit extension directory %s: %w", extension.name, err)
		}
		transaction.paths = append(transaction.paths, extension.finalPath)
		registrations = append(registrations, policy.ExtensionRegistration{
			Name:                     extension.name,
			ChromeExtensionID:        extension.chromeExtensionID,
			RequiresEnterprisePolicy: extension.requiresEnterprisePolicy,
		})
	}

	if err := s.policy.AddExtensions(registrations); err != nil {
		return nil, "", fmt.Errorf("failed to update enterprise policy: %w", err)
	}

	var newTokens []string
	if len(batch.flagPaths) > 0 {
		newTokens = []string{fmt.Sprintf("--load-extension=%s", strings.Join(batch.flagPaths, ","))}
	}
	if _, err := s.mergeAndWriteChromiumFlags(ctx, newTokens); err != nil {
		return nil, "", err
	}

	committed = true
	for _, extension := range batch.extensions {
		logger.FromContext(ctx).Info("installed extension",
			"name", extension.name,
			"chromeExtensionID", extension.chromeExtensionID,
			"requiresEnterprisePolicy", extension.requiresEnterprisePolicy)
	}
	return transaction, "", nil
}

func (s *ApiService) loadUnpackedExtensions(ctx context.Context, extensions []preparedExtension) error {
	log := logger.FromContext(ctx)
	return s.withCDPClientTimeout(ctx, extensionActivationTimeout, func(cdpCtx context.Context, client *cdpclient.Client) error {
		for _, extension := range extensions {
			if extension.requiresEnterprisePolicy {
				continue
			}
			id, err := client.LoadUnpackedExtension(cdpCtx, extension.finalPath)
			if err != nil {
				return fmt.Errorf("failed to load extension %s: %w", extension.name, err)
			}
			log.Info("loaded unpacked extension over CDP", "name", extension.name, "id", id)
		}
		return nil
	})
}

func (s *ApiService) verifyUnpackedExtensions(ctx context.Context, extensions []preparedExtension) error {
	wanted := make(map[string]struct{}, len(extensions))
	for _, extension := range extensions {
		if !extension.requiresEnterprisePolicy {
			wanted[extension.finalPath] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil
	}

	return s.withCDPClientTimeout(ctx, extensionActivationTimeout, func(cdpCtx context.Context, client *cdpclient.Client) error {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			extensions, err := client.GetExtensions(cdpCtx)
			if err == nil {
				missing := make(map[string]struct{}, len(wanted))
				for path := range wanted {
					missing[path] = struct{}{}
				}
				for _, extension := range extensions {
					if extension.Enabled {
						delete(missing, filepath.Clean(extension.Path))
					}
				}
				if len(missing) == 0 {
					return nil
				}
			}

			select {
			case <-cdpCtx.Done():
				return fmt.Errorf("extensions were not active after restart: %w", cdpCtx.Err())
			case <-ticker.C:
			}
		}
	})
}

func (s *ApiService) rollbackFailedExtensionActivation(ctx context.Context, transaction *committedExtensionBatch, activationErr error) *extensionUploadError {
	rollbackErr := transaction.rollback()
	restartErr := s.restartChromiumAndWait(ctx, "extension upload rollback")
	return internalExtensionUpload(errors.Join(
		fmt.Errorf("extension activation failed: %w", activationErr),
		rollbackErr,
		restartErr,
	).Error())
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
	// Fold kernel-namespaced disable tokens into the plain Chromium switch so
	// /chromium/flags only ever holds switches Chromium understands.
	mergedTokens = chromiumflags.TranslateKernelDisableFeatures(mergedTokens)

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

	s.chromiumConfigMu.Lock()
	defer s.chromiumConfigMu.Unlock()

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

	s.chromiumConfigMu.Lock()
	defer s.chromiumConfigMu.Unlock()

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
