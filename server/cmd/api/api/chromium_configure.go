package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"os"
	"os/exec"
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

type chromiumConfigureState struct {
	displayJSON       *string
	chromiumFlagsJSON *string
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
func (s *ApiService) ChromiumConfigure(ctx context.Context, request oapi.ChromiumConfigureRequestObject) (oapi.ChromiumConfigureResponseObject, error) {
	start := time.Now()

	if request.Body == nil {
		return cfg400("request body required"), nil
	}

	st := &chromiumConfigureState{}
	if msg := chromiumCfgParseMultipart(request.Body, st); msg != "" {
		st.cleanup()
		return cfg400(msg), nil
	}
	defer st.cleanup()

	if cfgActionables(st)+cfgHasStartURL(st.startURLRaw) == 0 {
		return cfg400("no configuration fields provided"), nil
	}

	needsStop := chromiumNeedsStopCycle(st)

	if needsStop {
		if chromiumDisplayHasSizedRequest(st.displayJSON) {
			stopped, stopErr := s.stopActiveRecordings(ctx)
			if stopErr != nil {
				return cfg500Configure(fmt.Sprintf("failed to stop recordings: %v", stopErr)), nil
			}
			if len(stopped) > 0 {
				defer func() {
					go s.startNewRecordingSegments(context.WithoutCancel(ctx), stopped)
				}()
			}
		}

		logger.FromContext(ctx).Info("chromium configure (stop/start path)")
		if err := s.stopChromium(ctx); err != nil {
			return cfg500Configure(err.Error()), nil
		}

		if st.hasProfile {
			if err := chromiumApplyProfileArchive(st.profileTemp, st.stripComponents); err != nil {
				return cfg500Configure(err.Error()), nil
			}
		}

		if chromiumDisplayHasSizedRequest(st.displayJSON) {
			b, msgs := chromiumParseDisplayParts(st.displayJSON)
			if msgs != "" {
				return cfg400(msgs), nil
			}
			if b != nil {
				if rr := chromiumDisplayApplyWhileStopped(ctx, s, b); rr != nil {
					return rr, nil
				}
			}
		}

		if msgs := chromiumApplyPolicies(ctx, s, st.chromePoliciesJSON); msgs != "" {
			return policyDisposition(msgs), nil
		}

		if reqMsgs, ierr := chromiumApplyExtensions(ctx, s, st.extItems); reqMsgs != "" {
			return cfg400(reqMsgs), nil
		} else if ierr != nil {
			return cfg500Configure(ierr.Error()), nil
		}

		if msgs := chromiumMergeFlagsRaw(ctx, s, st.chromiumFlagsJSON); msgs != "" {
			if strings.HasPrefix(msgs, "bad:") {
				return cfg400(strings.TrimPrefix(msgs, "bad:")), nil
			}
			return cfg500Configure(strings.TrimPrefix(msgs, "int:")), nil
		}

		if err := s.startChromiumAndWait(ctx, "batched chromium configure"); err != nil {
			return cfg500Configure(err.Error()), nil
		}
	} else {
		if st.displayJSON != nil && strings.TrimSpace(*st.displayJSON) != "" {
			body, msgs := chromiumParseDisplayParts(st.displayJSON)
			if msgs != "" {
				return cfg400(msgs), nil
			}
			if rr := chromiumRunPatchDisplay(ctx, s, body); rr != nil {
				return rr, nil
			}
		}
	}

	spec, msgs := chromiumStartURLSpec(st.startURLRaw)
	if msgs != "" {
		return cfg400(msgs), nil
	}
	if spec.needsNav {
		if err := chromiumDoNavigate(ctx, s, spec); err != nil {
			return cfg500Navigate(err.Error()), nil
		}
	}

	logger.FromContext(ctx).Info("chromium configure finished", "elapsed", time.Since(start).String())
	return oapi.ChromiumConfigure200JSONResponse{Ok: true}, nil
}

type startURLParsed struct {
	needsNav bool
	url      string
	wait     cdpclient.NavigateWaitUntil
	timeout  time.Duration
}

func chromiumStartURLSpec(raw *string) (startURLParsed, string) {
	var out startURLParsed
	out.timeout = 45 * time.Second
	out.wait = cdpclient.NavigateWaitLoad
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return out, ""
	}
	s := strings.TrimSpace(*raw)
	if strings.HasPrefix(s, "{") {
		var v struct {
			URL       string `json:"url"`
			WaitUntil string `json:"wait_until"`
			Timeout   *int   `json:"timeout_sec,omitempty"`
		}
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return out, "invalid start_url JSON"
		}
		if strings.TrimSpace(v.URL) == "" {
			return out, "start_url JSON requires url"
		}
		switch strings.TrimSpace(strings.ToLower(v.WaitUntil)) {
		case "", "load":
			out.wait = cdpclient.NavigateWaitLoad
		case "domcontentloaded":
			out.wait = cdpclient.NavigateWaitDOMContentLoaded
		default:
			return out, "wait_until must be load or domcontentloaded"
		}
		out.url = strings.TrimSpace(v.URL)
		if v.Timeout != nil && *v.Timeout > 0 {
			out.timeout = time.Duration(*v.Timeout) * time.Second
		}
	} else {
		out.url = s
	}
	if errMsgs := chromiumValidateNavigateURL(out.url); errMsgs != "" {
		return out, errMsgs
	}
	out.needsNav = true
	return out, ""
}

func chromiumValidateNavigateURL(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return "invalid start URL"
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "http", "about", "data", "chrome", "devtools":
	default:
		return fmt.Sprintf("unsupported URL scheme %q", parsed.Scheme)
	}
	return ""
}

func chromiumDoNavigate(ctx context.Context, s *ApiService, spec startURLParsed) error {
	upstream := s.upstreamMgr.Current()
	if upstream == "" {
		return fmt.Errorf("devtools upstream not available")
	}
	navCtx, cancel := context.WithTimeout(ctx, spec.timeout)
	defer cancel()
	return cdpclient.NavigateFirstPage(navCtx, upstream, spec.url, spec.wait)
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

func chromiumDisplayHasSizedRequest(displayJSON *string) bool {
	if displayJSON == nil {
		return false
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(*displayJSON), &raw); err != nil {
		return false
	}
	w, ow := raw["width"]
	h, oh := raw["height"]
	if !ow || !oh {
		return false
	}
	fw, wok := w.(float64)
	fh, hok := h.(float64)
	if wok && hok && fw > 0 && fh > 0 {
		return true
	}
	iw, wok := w.(int)
	ih, hok := h.(int)
	return wok && hok && iw > 0 && ih > 0
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

func cfg500Navigate(msg string) oapi.ChromiumConfigure500JSONResponse {
	return oapi.ChromiumConfigure500JSONResponse(oapi.ChromiumConfigureError{
		Phase:   oapi.NavigatePhase,
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
	if chromiumDisplayHasSizedRequest(st.displayJSON) {
		n++
	}
	return n
}

func cfgHasStartURL(s *string) int {
	if s == nil || strings.TrimSpace(*s) == "" {
		return 0
	}
	return 1
}

func chromiumCfgParseMultipart(body interface{}, st *chromiumConfigureState) string {
	mr, ok := any(body).(interface {
		NextPart() (*multipart.Part, error)
	})
	if !ok {
		return "multipart reader not available"
	}

	type pend struct {
		zipTmp string
		name   string
		gotZip bool
	}
	var cur *pend

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "failed reading multipart"
		}
		switch name := part.FormName(); name {
		case "display":
			b, err := io.ReadAll(part)
			if err != nil {
				return "read display field"
			}
			v := strings.TrimSpace(string(b))
			st.displayJSON = &v
		case "chromium_flags":
			b, err := io.ReadAll(part)
			if err != nil {
				return "read chromium_flags field"
			}
			v := string(b)
			st.chromiumFlagsJSON = &v
		case "chrome_policies":
			b, err := io.ReadAll(part)
			if err != nil {
				return "read chrome_policies field"
			}
			v := string(b)
			st.chromePoliciesJSON = &v
		case "strip_components":
			b, err := io.ReadAll(part)
			if err != nil {
				return "read strip_components"
			}
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n >= 0 {
				st.stripComponents = n
			}
		case "profile_archive":
			tmp, err := os.CreateTemp("", "bcc-prof-*.tar.zst")
			if err != nil {
				return "temp profile_archive"
			}
			st.allTemps = append(st.allTemps, tmp.Name())
			if _, err := io.Copy(tmp, part); err != nil {
				tmp.Close()
				return "read profile_archive"
			}
			if err := tmp.Close(); err != nil {
				return "finalize profile_archive"
			}
			st.profileTemp = tmp.Name()
			st.hasProfile = true
		case "start_url":
			b, err := io.ReadAll(part)
			if err != nil {
				return "read start_url"
			}
			v := string(b)
			st.startURLRaw = &v
		case "extensions.zip_file":
			if cur == nil {
				cur = &pend{}
			}
			tmp, err := os.CreateTemp("", "bcc-ext-*.zip")
			if err != nil {
				return "temp extensions.zip_file"
			}
			st.allTemps = append(st.allTemps, tmp.Name())
			if _, err := io.Copy(tmp, part); err != nil {
				tmp.Close()
				return "read extensions.zip_file"
			}
			if err := tmp.Close(); err != nil {
				return "close extensions.zip_file"
			}
			if cur.gotZip {
				return "duplicate extensions.zip_file pair"
			}
			cur.zipTmp = tmp.Name()
			cur.gotZip = true
		case "extensions.name":
			if cur == nil {
				cur = &pend{}
			}
			b, err := io.ReadAll(part)
			if err != nil {
				return "read extensions.name"
			}
			nm := strings.TrimSpace(string(b))
			if nm == "" || !nameRegex.MatchString(nm) {
				return "invalid extensions.name"
			}
			if cur.name != "" {
				return "duplicate extensions.name in pair"
			}
			cur.name = nm
		default:
			return fmt.Sprintf("unknown form field %q", name)
		}
		if cur != nil && cur.gotZip && cur.name != "" {
			st.extItems = append(st.extItems, extensionZipItem{zipTemp: cur.zipTmp, name: cur.name})
			cur = nil
		}
	}
	if cur != nil && (!cur.gotZip || cur.name == "") {
		return "each extension pair needs extensions.zip_file plus extensions.name"
	}
	return ""
}

func chromiumApplyProfileArchive(profilePath string, strip int) error {
	if err := os.RemoveAll(userDataProfileDir); err != nil {
		return fmt.Errorf("clear user-data: %w", err)
	}
	if err := os.MkdirAll(userDataProfileDir, 0o755); err != nil {
		return fmt.Errorf("mkdir user-data: %w", err)
	}
	f, err := os.Open(profilePath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := zstdutil.UntarZstd(f, userDataProfileDir, strip); err != nil {
		return fmt.Errorf("extract profile archive: %w", err)
	}
	out, err := exec.Command("chown", "-R", "kernel:kernel", userDataProfileDir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chown user-data: %w (%s)", err, string(out))
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

func chromiumDisplayApplyWhileStopped(ctx context.Context, s *ApiService, body *oapi.PatchDisplayRequest) oapi.ChromiumConfigureResponseObject {
	if body.Width == nil || body.Height == nil {
		return nil
	}
	w, h := *body.Width, *body.Height
	if w <= 0 || h <= 0 {
		return cfg400("display width and height must be positive")
	}
	mode := s.detectDisplayMode(ctx)
	rr := 60
	if body.RefreshRate != nil {
		rr = int(*body.RefreshRate)
	}
	if mode == "xvfb" {
		s.xvfbResizeMu.Lock()
		err := s.resizeXvfb(ctx, w, h)
		s.xvfbResizeMu.Unlock()
		if err != nil {
			return cfg500Configure(err.Error())
		}
		s.clearViewportOverride()
		return nil
	}
	var err error
	if s.isNekoEnabled() {
		err = s.setResolutionViaNeko(ctx, w, h, rr)
	} else {
		err = s.setResolutionXorgViaXrandr(ctx, w, h, rr, false)
	}
	if err != nil {
		return cfg500Configure(err.Error())
	}
	return nil
}

func chromiumRunPatchDisplay(ctx context.Context, s *ApiService, body *oapi.PatchDisplayJSONRequestBody) oapi.ChromiumConfigureResponseObject {
	resp, err := s.PatchDisplay(ctx, oapi.PatchDisplayRequestObject{Body: body})
	if err != nil {
		return cfg500Configure(err.Error())
	}
	switch r := resp.(type) {
	case oapi.PatchDisplay200JSONResponse:
		return nil
	case oapi.PatchDisplay400JSONResponse:
		return cfg400(r.Message)
	case oapi.PatchDisplay409JSONResponse:
		return oapi.ChromiumConfigure409JSONResponse{ConflictErrorJSONResponse: r.ConflictErrorJSONResponse}
	case oapi.PatchDisplay500JSONResponse:
		return cfg500Configure(r.Message)
	default:
		return cfg500Configure("unexpected PatchDisplay response")
	}
}

func chromiumApplyPolicies(ctx context.Context, s *ApiService, raw *string) string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(*raw), &m); err != nil {
		return "bad:invalid chrome_policies JSON"
	}
	if len(m) == 0 {
		return ""
	}
	overrides, err := policy.NewChromiumPolicyOverrides(m)
	if err != nil {
		if strings.Contains(err.Error(), "cannot be overridden") || strings.Contains(err.Error(), "invalid chromium policy overrides") {
			return "bad:" + err.Error()
		}
		return "int:" + err.Error()
	}
	if err := s.policy.ApplyOverrides(overrides); err != nil {
		if strings.Contains(err.Error(), "cannot be overridden") || strings.Contains(err.Error(), "invalid chromium policy overrides") {
			return "bad:" + err.Error()
		}
		return "int:" + err.Error()
	}
	return ""
}

func policyDisposition(msgs string) oapi.ChromiumConfigureResponseObject {
	if strings.HasPrefix(msgs, "bad:") {
		return cfg400(strings.TrimPrefix(msgs, "bad:"))
	}
	return cfg500Configure(strings.TrimPrefix(msgs, "int:"))
}

func chromiumApplyExtensions(ctx context.Context, s *ApiService, items []extensionZipItem) (string, error) {
	if len(items) == 0 {
		return "", nil
	}
	return s.applyExtensionZipItems(ctx, items)
}

func chromiumMergeFlagsRaw(ctx context.Context, s *ApiService, raw *string) string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return ""
	}
	var body struct {
		Flags []string `json:"flags"`
	}
	if err := json.Unmarshal([]byte(*raw), &body); err != nil {
		return "bad:invalid chromium_flags JSON"
	}
	if len(body.Flags) == 0 {
		return "bad:chromium_flags requires at least one flag"
	}
	for _, flag := range body.Flags {
		t := strings.TrimSpace(flag)
		if t == "" {
			return "bad:empty flag in chromium_flags"
		}
		if !strings.HasPrefix(t, "--") {
			return fmt.Sprintf("bad:invalid flag format: %s (must start with --)", flag)
		}
	}
	if _, err := s.mergeAndWriteChromiumFlags(ctx, body.Flags); err != nil {
		return "int:" + err.Error()
	}
	return ""
}
