package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/kernel/kernel-images/server/lib/policy"
	"github.com/kernel/kernel-images/server/lib/zstdutil"
)

const userDataProfileDir = "/home/kernel/user-data"
const maxStartURLLen = 2048
const startURLDispatchTimeout = 3 * time.Second

type chromiumConfigureState struct {
	displayJSON        *string
	chromiumFlagsJSON  *string
	chromePoliciesJSON *string

	stripComponents int

	profileTemp string // temp archive path
	hasProfile  bool

	startURLRaw *string

	extItems []extensionZipItem // zipTemp paths; merged with chromiumCfgParseExtensions

	allTemps []string
}

func (st *chromiumConfigureState) cleanup() {
	for _, p := range st.allTemps {
		_ = os.Remove(p)
	}
}

// ChromiumConfigure batched Chromium/session configuration plus optional navigation.
func (s *ApiService) ChromiumConfigure(ctx context.Context, request oapi.ChromiumConfigureRequestObject) (resp oapi.ChromiumConfigureResponseObject, err error) {
	start := time.Now()

	if request.Body == nil {
		return cfg400("request body required"), nil
	}

	st := &chromiumConfigureState{}
	if parseErr := chromiumCfgParseMultipart(request.Body, st); parseErr != nil {
		st.cleanup()
		var cfgErr chromiumCfgParseError
		if errors.As(parseErr, &cfgErr) && cfgErr.internal {
			return cfg500Configure(parseErr.Error()), nil
		}
		return cfg400(parseErr.Error()), nil
	}
	defer st.cleanup()

	spec, msgs := chromiumStartURLSpec(st.startURLRaw)
	if msgs != "" {
		return cfg400(msgs), nil
	}

	if cfgActionables(st)+cfgHasStartURLSpec(spec) == 0 {
		return cfg400("no configuration fields provided"), nil
	}

	needsStop := chromiumNeedsStopCycle(st)
	chromiumStopped := false
	restartAfterStop := func() error {
		if !chromiumStopped {
			return nil
		}
		if err := s.startChromiumAndWait(ctx, "batched chromium configure"); err != nil {
			return err
		}
		chromiumStopped = false
		return nil
	}
	defer func() {
		if restartErr := restartAfterStop(); restartErr != nil {
			if resp != nil {
				logger.FromContext(ctx).Error("failed to restart chromium after configure error", "error", restartErr)
				return
			}
			resp = cfg500ConfigureStep(chromiumConfigureStepStart, restartErr.Error())
			err = nil
		}
	}()

	if needsStop {
		logger.FromContext(ctx).Info("chromium configure (stop/start path)")
		if err := s.stopChromium(ctx); err != nil {
			return cfg500ConfigureStep(chromiumConfigureStepStop, err.Error()), nil
		}
		chromiumStopped = true

		policyOverrides, err := chromiumValidatePolicies(st.chromePoliciesJSON)
		if err != nil {
			return cfgResponseFromStepError(chromiumConfigureStepPolicies, err), nil
		}
		if err := chromiumApplyPolicies(ctx, s, policyOverrides); err != nil {
			return cfgResponseFromStepError(chromiumConfigureStepPolicies, err), nil
		}

		if reqMsgs, ierr := chromiumApplyExtensions(ctx, s, st.extItems); reqMsgs != "" {
			return cfg400(fmt.Sprintf("%s: %s", chromiumConfigureStepExtensions, reqMsgs)), nil
		} else if ierr != nil {
			return cfg500ConfigureStep(chromiumConfigureStepExtensions, ierr.Error()), nil
		}

		if st.displayJSON != nil && strings.TrimSpace(*st.displayJSON) != "" {
			displayPlan, displayResp := chromiumPrepareDisplay(ctx, s, st.displayJSON)
			if displayResp != nil {
				return displayResp, nil
			}
			if displayPlan != nil {
				stopped, stopErr := s.stopActiveRecordings(ctx)
				if stopErr != nil {
					return cfg500ConfigureStep(chromiumConfigureStepDisplay, fmt.Sprintf("failed to stop recordings: %v", stopErr)), nil
				}
				if len(stopped) > 0 {
					defer func() {
						go s.startNewRecordingSegments(context.WithoutCancel(ctx), stopped)
					}()
				}
				if rr := chromiumDisplayApplyWhileStopped(ctx, s, displayPlan); rr != nil {
					return rr, nil
				}
			}
		}

		flagsPlan, err := chromiumValidateFlags(st.chromiumFlagsJSON)
		if err != nil {
			return cfgResponseFromStepError(chromiumConfigureStepFlags, err), nil
		}
		if err := chromiumMergeFlags(ctx, s, flagsPlan); err != nil {
			return cfgResponseFromStepError(chromiumConfigureStepFlags, err), nil
		}

		if st.hasProfile {
			preparedProfile, cleanupProfile, err := chromiumPrepareProfileArchive(st.profileTemp, st.stripComponents)
			if cleanupProfile != nil {
				defer cleanupProfile()
			}
			if err != nil {
				return cfg500ConfigureStep(chromiumConfigureStepProfile, err.Error()), nil
			}
			if err := chromiumInstallPreparedProfile(preparedProfile); err != nil {
				return cfg500ConfigureStep(chromiumConfigureStepProfile, err.Error()), nil
			}
		}

		if err := restartAfterStop(); err != nil {
			return cfg500ConfigureStep(chromiumConfigureStepStart, err.Error()), nil
		}
	} else {
		if st.displayJSON != nil && strings.TrimSpace(*st.displayJSON) != "" {
			displayPlan, displayResp := chromiumPrepareDisplay(ctx, s, st.displayJSON)
			if displayResp != nil {
				return displayResp, nil
			}
			if displayPlan != nil {
				if rr := chromiumRunPatchDisplay(ctx, s, displayPlan.body); rr != nil {
					return rr, nil
				}
			}
		}
	}

	if spec.needsNav {
		if err := chromiumDoNavigate(ctx, s, spec); err != nil {
			logger.FromContext(ctx).Warn("start_url dispatch failed", "error", err)
		}
	}

	logger.FromContext(ctx).Info("chromium configure finished", "elapsed", time.Since(start).String())
	return oapi.ChromiumConfigure200JSONResponse{Ok: true}, nil
}

type startURLParsed struct {
	needsNav bool
	url      string
}

type chromiumConfigureStep string

const (
	chromiumConfigureStepStop       chromiumConfigureStep = "stop_chromium"
	chromiumConfigureStepStart      chromiumConfigureStep = "start_chromium"
	chromiumConfigureStepPolicies   chromiumConfigureStep = "chrome_policies"
	chromiumConfigureStepExtensions chromiumConfigureStep = "extensions"
	chromiumConfigureStepDisplay    chromiumConfigureStep = "display"
	chromiumConfigureStepFlags      chromiumConfigureStep = "chromium_flags"
	chromiumConfigureStepProfile    chromiumConfigureStep = "profile"
)

func chromiumStartURLSpec(raw *string) (startURLParsed, string) {
	var out startURLParsed
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return out, ""
	}
	if len(*raw) > maxStartURLLen {
		return out, fmt.Sprintf("start_url exceeds max length of %d bytes", maxStartURLLen)
	}
	out.url = normalizeStartURL(*raw)
	out.needsNav = true
	return out, ""
}

func normalizeStartURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" ||
		strings.Contains(parsed.Scheme, ".") ||
		strings.EqualFold(parsed.Scheme, "localhost") {
		return "https://" + rawURL
	}
	return rawURL
}

func chromiumDoNavigate(ctx context.Context, s *ApiService, spec startURLParsed) error {
	upstream := s.upstreamMgr.Current()
	if upstream == "" {
		return fmt.Errorf("devtools upstream not available")
	}
	navCtx, cancel := context.WithTimeout(ctx, startURLDispatchTimeout)
	defer cancel()
	return cdpclient.DispatchStartURL(navCtx, upstream, spec.url)
}

type cfgBadRequestError struct {
	message string
}

func (e cfgBadRequestError) Error() string {
	return e.message
}

func cfgBadRequest(msg string) error {
	return cfgBadRequestError{message: msg}
}

type chromiumCfgParseError struct {
	message  string
	internal bool
}

func (e chromiumCfgParseError) Error() string {
	return e.message
}

func cfgParseBadRequest(msg string) error {
	return chromiumCfgParseError{message: msg}
}

func cfgParseInternal(msg string) error {
	return chromiumCfgParseError{message: msg, internal: true}
}

func cfgResponseFromStepError(step chromiumConfigureStep, err error) oapi.ChromiumConfigureResponseObject {
	var bad cfgBadRequestError
	if errors.As(err, &bad) {
		return cfg400(fmt.Sprintf("%s: %s", step, bad.message))
	}
	return cfg500ConfigureStep(step, err.Error())
}

type chromiumFlagsPlan struct {
	flags []string
}

type chromiumDisplayPlan struct {
	body        *oapi.PatchDisplayJSONRequestBody
	width       int
	height      int
	refreshRate int
}

func chromiumNeedsStopCycle(st *chromiumConfigureState) bool {
	return st.hasProfile ||
		len(st.extItems) > 0 ||
		policiesContentNonEmpty(st.chromePoliciesJSON) ||
		flagsContentNonEmpty(st.chromiumFlagsJSON)
}

func policiesContentNonEmpty(s *string) bool {
	if !policiesNonEmpty(s) {
		return false
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(*s)), &m); err != nil {
		return true
	}
	return len(m) > 0
}

func flagsContentNonEmpty(s *string) bool {
	if !flagsNonEmpty(s) {
		return false
	}
	var raw struct {
		Flags []string `json:"flags"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(*s)), &raw); err != nil {
		return true
	}
	return len(raw.Flags) > 0
}

func policiesNonEmpty(s *string) bool {
	return s != nil && strings.TrimSpace(*s) != ""
}

func flagsNonEmpty(s *string) bool {
	return s != nil && strings.TrimSpace(*s) != ""
}

func cfg400(msg string) oapi.ChromiumConfigure400JSONResponse {
	return oapi.ChromiumConfigure400JSONResponse{
		BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: msg},
	}
}

func cfg500Configure(msg string) oapi.ChromiumConfigure500JSONResponse {
	return oapi.ChromiumConfigure500JSONResponse(oapi.ChromiumConfigureError{
		Phase:   oapi.ConfigurePhase,
		Message: msg,
	})
}

func cfg500ConfigureStep(step chromiumConfigureStep, msg string) oapi.ChromiumConfigure500JSONResponse {
	stepValue := oapi.ChromiumConfigureErrorStep(step)
	return oapi.ChromiumConfigure500JSONResponse(oapi.ChromiumConfigureError{
		Phase:   oapi.ConfigurePhase,
		Step:    &stepValue,
		Message: msg,
	})
}

func cfgActionables(st *chromiumConfigureState) int {
	n := 0
	if policiesContentNonEmpty(st.chromePoliciesJSON) {
		n++
	}
	if flagsContentNonEmpty(st.chromiumFlagsJSON) {
		n++
	}
	if len(st.extItems) > 0 {
		n++
	}
	if st.hasProfile {
		n++
	}
	if st.displayJSON != nil && strings.TrimSpace(*st.displayJSON) != "" {
		n++
	}
	return n
}

func cfgHasStartURLSpec(spec startURLParsed) int {
	if !spec.needsNav {
		return 0
	}
	return 1
}

func chromiumCfgParseMultipart(body interface{}, st *chromiumConfigureState) error {
	mr, ok := any(body).(interface {
		NextPart() (*multipart.Part, error)
	})
	if !ok {
		return cfgParseInternal("multipart reader not available")
	}

	type pend struct {
		zipTmp string
		name   string
		gotZip bool
	}
	var cur *pend
	var gotDisplay, gotChromiumFlags, gotChromePolicies, gotStripComponents, gotProfileArchive, gotStartURL bool

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return cfgParseBadRequest("failed reading multipart")
		}
		switch name := part.FormName(); name {
		case "display":
			if gotDisplay {
				return cfgParseBadRequest("duplicate display field")
			}
			gotDisplay = true
			b, err := io.ReadAll(part)
			if err != nil {
				return cfgParseInternal("read display field")
			}
			v := strings.TrimSpace(string(b))
			st.displayJSON = &v
		case "chromium_flags":
			if gotChromiumFlags {
				return cfgParseBadRequest("duplicate chromium_flags field")
			}
			gotChromiumFlags = true
			b, err := io.ReadAll(part)
			if err != nil {
				return cfgParseInternal("read chromium_flags field")
			}
			v := string(b)
			st.chromiumFlagsJSON = &v
		case "chrome_policies":
			if gotChromePolicies {
				return cfgParseBadRequest("duplicate chrome_policies field")
			}
			gotChromePolicies = true
			b, err := io.ReadAll(part)
			if err != nil {
				return cfgParseInternal("read chrome_policies field")
			}
			v := string(b)
			st.chromePoliciesJSON = &v
		case "strip_components":
			if gotStripComponents {
				return cfgParseBadRequest("duplicate strip_components field")
			}
			gotStripComponents = true
			b, err := io.ReadAll(part)
			if err != nil {
				return cfgParseInternal("read strip_components")
			}
			n, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil || n < 0 {
				return cfgParseBadRequest("strip_components must be a non-negative integer")
			}
			st.stripComponents = n
		case "profile_archive":
			if gotProfileArchive {
				return cfgParseBadRequest("duplicate profile_archive field")
			}
			gotProfileArchive = true
			tmp, err := os.CreateTemp("", "bcc-prof-*.tar.zst")
			if err != nil {
				return cfgParseInternal("temp profile_archive")
			}
			st.allTemps = append(st.allTemps, tmp.Name())
			if _, err := io.Copy(tmp, part); err != nil {
				tmp.Close()
				return cfgParseInternal("read profile_archive")
			}
			if err := tmp.Close(); err != nil {
				return cfgParseInternal("finalize profile_archive")
			}
			st.profileTemp = tmp.Name()
			st.hasProfile = true
		case "start_url":
			if gotStartURL {
				return cfgParseBadRequest("duplicate start_url field")
			}
			gotStartURL = true
			b, err := io.ReadAll(part)
			if err != nil {
				return cfgParseInternal("read start_url")
			}
			v := string(b)
			st.startURLRaw = &v
		case "extensions.zip_file":
			if cur == nil {
				cur = &pend{}
			}
			if cur.gotZip {
				return cfgParseBadRequest("duplicate extensions.zip_file pair")
			}
			tmp, err := os.CreateTemp("", "bcc-ext-*.zip")
			if err != nil {
				return cfgParseInternal("temp extensions.zip_file")
			}
			st.allTemps = append(st.allTemps, tmp.Name())
			if _, err := io.Copy(tmp, part); err != nil {
				tmp.Close()
				return cfgParseInternal("read extensions.zip_file")
			}
			if err := tmp.Close(); err != nil {
				return cfgParseInternal("close extensions.zip_file")
			}
			cur.zipTmp = tmp.Name()
			cur.gotZip = true
		case "extensions.name":
			if cur == nil {
				cur = &pend{}
			}
			b, err := io.ReadAll(part)
			if err != nil {
				return cfgParseInternal("read extensions.name")
			}
			nm := strings.TrimSpace(string(b))
			if nm == "" || !nameRegex.MatchString(nm) {
				return cfgParseBadRequest("invalid extensions.name")
			}
			if cur.name != "" {
				return cfgParseBadRequest("duplicate extensions.name in pair")
			}
			cur.name = nm
		default:
			return cfgParseBadRequest(fmt.Sprintf("unknown form field %q", name))
		}
		if cur != nil && cur.gotZip && cur.name != "" {
			st.extItems = append(st.extItems, extensionZipItem{zipTemp: cur.zipTmp, name: cur.name})
			cur = nil
		}
	}
	if cur != nil && (!cur.gotZip || cur.name == "") {
		return cfgParseBadRequest("each extension pair needs extensions.zip_file plus extensions.name")
	}
	return nil
}

func chromiumPrepareProfileArchive(profilePath string, strip int) (preparedDir string, cleanup func(), err error) {
	parent := filepath.Dir(userDataProfileDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir user-data parent: %w", err)
	}
	preparedDir, err = os.MkdirTemp(parent, ".user-data-new-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp user-data dir: %w", err)
	}
	cleanup = func() {
		_ = os.RemoveAll(preparedDir)
	}

	f, err := os.Open(profilePath)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	defer f.Close()
	if err := zstdutil.UntarZstd(f, preparedDir, strip); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("extract profile archive: %w", err)
	}
	out, err := exec.Command("chown", "-R", "kernel:kernel", preparedDir).CombinedOutput()
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("chown user-data: %w (%s)", err, string(out))
	}
	return preparedDir, cleanup, nil
}

func chromiumInstallPreparedProfile(preparedDir string) error {
	if preparedDir == "" {
		return nil
	}
	parent := filepath.Dir(userDataProfileDir)
	backupDir := filepath.Join(parent, fmt.Sprintf(".user-data-old-%d", time.Now().UnixNano()))
	hadExisting := false

	if _, err := os.Stat(userDataProfileDir); err == nil {
		hadExisting = true
		if err := os.Rename(userDataProfileDir, backupDir); err != nil {
			return fmt.Errorf("backup user-data: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat user-data: %w", err)
	}

	if err := os.Rename(preparedDir, userDataProfileDir); err != nil {
		if hadExisting {
			_ = os.Rename(backupDir, userDataProfileDir)
		}
		return fmt.Errorf("replace user-data: %w", err)
	}
	if hadExisting {
		_ = os.RemoveAll(backupDir)
	}
	return nil
}

func chromiumParseDisplayParts(displayJSON *string) (*oapi.PatchDisplayJSONRequestBody, string) {
	if displayJSON == nil {
		return nil, ""
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(*displayJSON), &raw); err != nil {
		return nil, "invalid display JSON"
	}
	if len(raw) == 0 {
		return nil, "display payload empty"
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return nil, "invalid display marshal"
	}
	var body oapi.PatchDisplayJSONRequestBody
	if err := json.Unmarshal(blob, &body); err != nil {
		return nil, fmt.Sprintf("invalid display payload: %v", err)
	}
	return &body, ""
}

func chromiumPrepareDisplay(ctx context.Context, s *ApiService, displayJSON *string) (*chromiumDisplayPlan, oapi.ChromiumConfigureResponseObject) {
	if displayJSON == nil {
		return nil, nil
	}
	body, msgs := chromiumParseDisplayParts(displayJSON)
	if msgs != "" {
		return nil, cfg400(msgs)
	}
	if body.Width == nil && body.Height == nil {
		return nil, cfg400("no display parameters to update")
	}

	currentWidth, currentHeight, currentRefreshRate, err := s.getCurrentResolution(ctx)
	if err != nil {
		logger.FromContext(ctx).Error("failed to get current resolution", "error", err)
		return nil, cfg500ConfigureStep(chromiumConfigureStepDisplay, "failed to get current display resolution")
	}
	width, height, refreshRate := currentWidth, currentHeight, currentRefreshRate
	if body.Width != nil {
		width = *body.Width
	}
	if body.Height != nil {
		height = *body.Height
	}
	if body.RefreshRate != nil {
		refreshRate = int(*body.RefreshRate)
	}

	if width <= 0 || height <= 0 {
		return nil, cfg400("invalid width/height")
	}

	requireIdle := true
	if body.RequireIdle != nil {
		requireIdle = *body.RequireIdle
	}
	if requireIdle {
		live := s.getActiveNekoSessions(ctx)
		isRecording := s.anyRecordingActive(ctx)
		if live != 0 || isRecording {
			return nil, oapi.ChromiumConfigure409JSONResponse{
				ConflictErrorJSONResponse: oapi.ConflictErrorJSONResponse{
					Message: "resize refused: live view or recording/replay active",
				},
			}
		}
	}

	return &chromiumDisplayPlan{
		body:        body,
		width:       width,
		height:      height,
		refreshRate: refreshRate,
	}, nil
}

func chromiumDisplayApplyWhileStopped(ctx context.Context, s *ApiService, plan *chromiumDisplayPlan) oapi.ChromiumConfigureResponseObject {
	w, h := plan.width, plan.height
	if w <= 0 || h <= 0 {
		return cfg400("display width and height must be positive")
	}
	mode := s.detectDisplayMode(ctx)
	rr := plan.refreshRate
	if mode == "xvfb" {
		s.xvfbResizeMu.Lock()
		err := s.resizeXvfb(ctx, w, h)
		if err == nil {
			s.clearViewportOverride()
		}
		s.xvfbResizeMu.Unlock()
		if err != nil {
			return cfg500ConfigureStep(chromiumConfigureStepDisplay, err.Error())
		}
		return nil
	}
	var err error
	if s.isNekoEnabled() {
		err = s.setResolutionViaNeko(ctx, w, h, rr)
	} else {
		err = s.setResolutionXorgViaXrandr(ctx, w, h, rr, false)
	}
	if err != nil {
		return cfg500ConfigureStep(chromiumConfigureStepDisplay, err.Error())
	}
	return nil
}

func chromiumRunPatchDisplay(ctx context.Context, s *ApiService, body *oapi.PatchDisplayJSONRequestBody) oapi.ChromiumConfigureResponseObject {
	resp, err := s.PatchDisplay(ctx, oapi.PatchDisplayRequestObject{Body: body})
	if err != nil {
		return cfg500ConfigureStep(chromiumConfigureStepDisplay, err.Error())
	}
	switch r := resp.(type) {
	case oapi.PatchDisplay200JSONResponse:
		return nil
	case oapi.PatchDisplay400JSONResponse:
		return cfg400(r.Message)
	case oapi.PatchDisplay409JSONResponse:
		return oapi.ChromiumConfigure409JSONResponse{ConflictErrorJSONResponse: r.ConflictErrorJSONResponse}
	case oapi.PatchDisplay500JSONResponse:
		return cfg500ConfigureStep(chromiumConfigureStepDisplay, r.Message)
	default:
		return cfg500ConfigureStep(chromiumConfigureStepDisplay, "unexpected PatchDisplay response")
	}
}

func chromiumValidatePolicies(raw *string) (policy.ChromiumPolicyOverrides, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(*raw), &m); err != nil {
		return nil, cfgBadRequest("invalid chrome_policies JSON")
	}
	if len(m) == 0 {
		return nil, nil
	}
	overrides, err := policy.NewChromiumPolicyOverrides(m)
	if err != nil {
		return nil, cfgBadRequest(err.Error())
	}
	if err := overrides.Validate(); err != nil {
		return nil, cfgBadRequest(err.Error())
	}
	return overrides, nil
}

func chromiumApplyPolicies(ctx context.Context, s *ApiService, overrides policy.ChromiumPolicyOverrides) error {
	if len(overrides) == 0 {
		return nil
	}
	if err := s.policy.ApplyOverrides(overrides); err != nil {
		if strings.Contains(err.Error(), "cannot be overridden") || strings.Contains(err.Error(), "invalid chromium policy overrides") {
			return cfgBadRequest(err.Error())
		}
		return err
	}
	return nil
}

func chromiumApplyExtensions(ctx context.Context, s *ApiService, items []extensionZipItem) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	return s.applyExtensionZipItems(ctx, items)
}

func chromiumValidateFlags(raw *string) (*chromiumFlagsPlan, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	var body struct {
		Flags []string `json:"flags"`
	}
	if err := json.Unmarshal([]byte(*raw), &body); err != nil {
		return nil, cfgBadRequest("invalid chromium_flags JSON")
	}
	if len(body.Flags) == 0 {
		return nil, nil
	}
	for _, flag := range body.Flags {
		t := strings.TrimSpace(flag)
		if t == "" {
			return nil, cfgBadRequest("empty flag in chromium_flags")
		}
		if !strings.HasPrefix(t, "--") {
			return nil, cfgBadRequest(fmt.Sprintf("invalid flag format: %s (must start with --)", flag))
		}
	}
	return &chromiumFlagsPlan{flags: body.Flags}, nil
}

func chromiumMergeFlags(ctx context.Context, s *ApiService, plan *chromiumFlagsPlan) error {
	if plan == nil {
		return nil
	}
	_, err := s.mergeAndWriteChromiumFlags(ctx, plan.flags)
	return err
}
