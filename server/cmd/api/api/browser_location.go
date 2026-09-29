package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/metrics"
	"golang.org/x/text/language"
)

const (
	browserLocationApplyTimeout     = 5 * time.Second
	defaultBrowserLocationStatePath = "/run/kernel/browser-location.json"
)

type browserLocationBundle struct {
	Epoch      string   `json:"epoch"`
	Generation uint64   `json:"generation"`
	TimeZone   string   `json:"timezone"`
	Locale     string   `json:"locale"`
	Languages  []string `json:"languages"`
}

type browserLocationComponents struct {
	TimeZone        bool   `json:"timezone"`
	Browser         bool   `json:"browser"`
	Renderers       bool   `json:"renderers"`
	NetworkContexts bool   `json:"network_contexts"`
	DateTimeLocale  string `json:"date_time_locale,omitempty"`
	NumberLocale    string `json:"number_locale,omitempty"`
	CollatorLocale  string `json:"collator_locale,omitempty"`
}

type browserLocationStatus struct {
	ActiveEpoch string                    `json:"active_epoch,omitempty"`
	Accepted    *browserLocationBundle    `json:"accepted,omitempty"`
	Applied     *browserLocationBundle    `json:"applied,omitempty"`
	Components  browserLocationComponents `json:"components"`
	Error       string                    `json:"error,omitempty"`
}

type browserLocationDurableState struct {
	ActiveEpoch string                 `json:"active_epoch"`
	Accepted    *browserLocationBundle `json:"accepted,omitempty"`
}

var (
	errStaleBrowserLocation    = errors.New("browser location generation is stale")
	errConflictBrowserLocation = errors.New("browser location generation conflicts with accepted payload")
	errBrowserLocationEpoch    = errors.New("browser location epoch is not active")
)

func validateBrowserLocationBundle(raw string) (browserLocationBundle, error) {
	var bundle browserLocationBundle
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil {
		return bundle, fmt.Errorf("invalid browser_location JSON")
	}
	if bundle.Epoch == "" || bundle.Generation == 0 {
		return bundle, fmt.Errorf("browser_location epoch and generation are required")
	}
	zonePath, err := browserLocationZonePath(bundle.TimeZone)
	if err != nil {
		return bundle, err
	}
	if _, err := os.Stat(zonePath); err != nil {
		return bundle, fmt.Errorf("unsupported browser_location timezone")
	}
	locale, err := language.Parse(bundle.Locale)
	if err != nil || locale.String() != bundle.Locale {
		return bundle, fmt.Errorf("browser_location locale must be canonical BCP 47")
	}
	if len(bundle.Languages) == 0 || bundle.Languages[0] != bundle.Locale {
		return bundle, fmt.Errorf("browser_location languages must start with locale")
	}
	for _, value := range bundle.Languages {
		tag, err := language.Parse(value)
		if err != nil || tag.String() != value {
			return bundle, fmt.Errorf("browser_location languages must be canonical BCP 47")
		}
	}
	return bundle, nil
}

func browserLocationZonePath(timezone string) (string, error) {
	if timezone == "" || filepath.IsAbs(timezone) || strings.Contains(timezone, "..") || strings.ContainsRune(timezone, '\x00') {
		return "", fmt.Errorf("invalid browser_location timezone")
	}
	root := "/usr/share/zoneinfo"
	path := filepath.Join(root, filepath.FromSlash(timezone))
	if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid browser_location timezone")
	}
	return path, nil
}

func (s *ApiService) validateBrowserLocationSupport(ctx context.Context, bundle browserLocationBundle) error {
	var resolution cdpclient.BrowserLocationResolution
	err := s.withCDPClientTimeout(ctx, time.Second, func(cdpCtx context.Context, client *cdpclient.Client) error {
		var err error
		resolution, err = client.ValidateBrowserLocation(cdpCtx, bundle.Locale)
		return err
	})
	if err != nil {
		return fmt.Errorf("validate browser_location locale: %w", err)
	}
	observed := cdpclient.BrowserLocation{
		DateTimeLocale: resolution.DateTimeLocale,
		NumberLocale:   resolution.NumberLocale,
		CollatorLocale: resolution.CollatorLocale,
	}
	if !resolvedLocalesMatch(bundle.Locale, observed) {
		return fmt.Errorf("browser_location locale does not resolve consistently")
	}
	return nil
}

func (s *ApiService) resetBrowserLocation(previousEpoch string, bundle browserLocationBundle) error {
	if bundle.Generation != 1 {
		return fmt.Errorf("browser location reset requires generation 1")
	}
	s.browserLocationApplyMu.Lock()
	defer s.browserLocationApplyMu.Unlock()
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	if s.browserLocation.activeEpoch == bundle.Epoch {
		if s.browserLocation.accepted != nil && browserLocationBundlesEqual(*s.browserLocation.accepted, bundle) {
			s.browserLocation.retries.Add(1)
			s.startBrowserLocationReconcileLocked(bundle)
			return nil
		}
		return errConflictBrowserLocation
	}
	if s.browserLocation.activeEpoch != previousEpoch {
		s.browserLocation.epochRejects.Add(1)
		return errBrowserLocationEpoch
	}
	copy := bundle
	if err := s.persistBrowserLocationState(browserLocationDurableState{ActiveEpoch: bundle.Epoch, Accepted: &copy}); err != nil {
		return err
	}
	if s.browserLocation.cancel != nil {
		s.browserLocation.cancel()
	}
	s.browserLocation.activeEpoch = bundle.Epoch
	s.browserLocation.accepted = &copy
	s.browserLocation.applied = nil
	s.browserLocation.components = browserLocationComponents{}
	s.browserLocation.lastError = ""
	s.browserLocation.acceptedCount.Add(1)
	s.startBrowserLocationReconcileLocked(bundle)
	return nil
}

func (s *ApiService) acceptBrowserLocation(bundle browserLocationBundle) error {
	s.browserLocationApplyMu.Lock()
	defer s.browserLocationApplyMu.Unlock()
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	if s.browserLocation.activeEpoch == "" || s.browserLocation.activeEpoch != bundle.Epoch {
		s.browserLocation.epochRejects.Add(1)
		return errBrowserLocationEpoch
	}
	current := s.browserLocation.accepted
	if current != nil {
		if bundle.Generation < current.Generation {
			s.browserLocation.stale.Add(1)
			return errStaleBrowserLocation
		}
		if bundle.Generation == current.Generation {
			if !browserLocationBundlesEqual(*current, bundle) {
				s.browserLocation.conflicts.Add(1)
				return errConflictBrowserLocation
			}
			s.browserLocation.retries.Add(1)
			s.startBrowserLocationReconcileLocked(bundle)
			return nil
		}
	}
	copy := bundle
	if err := s.persistBrowserLocationState(browserLocationDurableState{ActiveEpoch: s.browserLocation.activeEpoch, Accepted: &copy}); err != nil {
		return err
	}
	s.browserLocation.accepted = &copy
	s.browserLocation.applied = nil
	s.browserLocation.components = browserLocationComponents{}
	s.browserLocation.lastError = ""
	s.browserLocation.acceptedCount.Add(1)
	s.startBrowserLocationReconcileLocked(bundle)
	return nil
}

func (s *ApiService) startBrowserLocationReconcileLocked(bundle browserLocationBundle) {
	s.browserLocation.lastAttempt = time.Now()
	if s.browserLocation.cancel != nil {
		s.browserLocation.cancel()
	}
	ctx, cancel := context.WithCancel(s.lifecycleCtx)
	s.browserLocation.cancel = cancel
	reconcile := s.browserLocationReconcile
	if reconcile == nil {
		reconcile = s.reconcileBrowserLocation
	}
	go reconcile(ctx, bundle)
}

func (s *ApiService) reconcileBrowserLocation(ctx context.Context, bundle browserLocationBundle) {
	started := time.Now()
	deadline := started.Add(browserLocationApplyTimeout)
	var lastErr error
	for {
		s.browserLocationApplyMu.Lock()
		if ctx.Err() != nil || !s.browserLocationIsCurrent(bundle) {
			s.browserLocationApplyMu.Unlock()
			return
		}
		components := browserLocationComponents{}
		if err := rewriteLocaltime(bundle.TimeZone); err != nil {
			lastErr = err
		} else {
			components.TimeZone = true
			lastErr = s.withCDPClientTimeout(ctx, time.Second, func(cdpCtx context.Context, client *cdpclient.Client) error {
				languages := strings.Join(bundle.Languages, ",")
				if err := client.SetBrowserLocation(cdpCtx, bundle.Locale, languages); err != nil {
					return err
				}
				observed, err := client.GetBrowserLocation(cdpCtx)
				if err != nil {
					return err
				}
				components.Browser = observed.Locale == bundle.Locale && observed.AcceptLanguages == languages && observed.TimeZone == bundle.TimeZone
				components.Renderers = observed.RenderersConverged
				components.NetworkContexts = observed.NetworkContextsConverged
				components.DateTimeLocale = observed.DateTimeLocale
				components.NumberLocale = observed.NumberLocale
				components.CollatorLocale = observed.CollatorLocale
				if !components.Browser || !components.Renderers || !components.NetworkContexts || !resolvedLocalesMatch(bundle.Locale, observed) {
					return fmt.Errorf("browser location components have not converged")
				}
				return nil
			})
		}
		s.browserLocationApplyMu.Unlock()
		s.browserLocationMu.Lock()
		if accepted := s.browserLocation.accepted; accepted != nil && browserLocationBundlesEqual(*accepted, bundle) {
			s.browserLocation.components = components
		}
		s.browserLocationMu.Unlock()
		if lastErr == nil {
			s.browserLocationMu.Lock()
			if accepted := s.browserLocation.accepted; accepted != nil && browserLocationBundlesEqual(*accepted, bundle) {
				copy := bundle
				s.browserLocation.applied = &copy
				s.browserLocation.lastError = ""
				s.browserLocation.appliedCount.Add(1)
				s.browserLocation.convergenceMs.Store(uint64(time.Since(started).Milliseconds()))
			}
			s.browserLocationMu.Unlock()
			return
		}
		if time.Now().After(deadline) {
			s.browserLocationMu.Lock()
			if accepted := s.browserLocation.accepted; accepted != nil && browserLocationBundlesEqual(*accepted, bundle) {
				s.browserLocation.lastError = lastErr.Error()
				s.browserLocation.failures.Add(1)
			}
			s.browserLocationMu.Unlock()
			logger.FromContext(ctx).Error("browser location did not converge", "error", lastErr, "generation", bundle.Generation)
			return
		}
		s.browserLocation.retries.Add(1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *ApiService) browserLocationIsCurrent(bundle browserLocationBundle) bool {
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	return s.browserLocation.accepted != nil && browserLocationBundlesEqual(*s.browserLocation.accepted, bundle)
}

func resolvedLocalesMatch(expected string, observed cdpclient.BrowserLocation) bool {
	want, err := language.Parse(expected)
	if err != nil {
		return false
	}
	wantBase, _ := want.Base()
	for _, value := range []string{observed.DateTimeLocale, observed.NumberLocale, observed.CollatorLocale} {
		tag, err := language.Parse(value)
		if err != nil {
			return false
		}
		base, _ := tag.Base()
		if base != wantBase {
			return false
		}
	}
	return true
}

func rewriteLocaltime(timezone string) error {
	target, err := browserLocationZonePath(timezone)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("/etc/.localtime-kernel-%d", time.Now().UnixNano())
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("stage localtime: %w", err)
	}
	defer os.Remove(tmp)
	if err := os.Rename(tmp, "/etc/localtime"); err != nil {
		return fmt.Errorf("replace localtime: %w", err)
	}
	return nil
}

func (s *ApiService) browserLocationStatePath() string {
	if value := strings.TrimSpace(os.Getenv("KERNEL_BROWSER_LOCATION_STATE_PATH")); value != "" {
		return value
	}
	return defaultBrowserLocationStatePath
}

func (s *ApiService) persistBrowserLocationState(state browserLocationDurableState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path := s.browserLocationStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create browser location state directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".browser-location-*")
	if err != nil {
		return fmt.Errorf("create browser location state: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace browser location state: %w", err)
	}
	return nil
}

func (s *ApiService) initializeBrowserLocation() error {
	data, err := os.ReadFile(s.browserLocationStatePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read browser location state: %w", err)
	}
	if err == nil {
		var state browserLocationDurableState
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("decode browser location state: %w", err)
		}
		if state.Accepted != nil && state.ActiveEpoch != state.Accepted.Epoch {
			return fmt.Errorf("browser location state epoch mismatch")
		}
		s.browserLocation.activeEpoch = state.ActiveEpoch
		s.browserLocation.accepted = state.Accepted
	}
	if s.upstreamMgr != nil {
		go s.browserLocationLifecycleLoop()
	}
	return nil
}

func (s *ApiService) browserLocationLifecycleLoop() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastUpstream := ""
	for {
		select {
		case <-s.lifecycleCtx.Done():
			return
		case <-ticker.C:
			upstream := s.upstreamMgr.Current()
			if upstream == "" {
				continue
			}
			s.browserLocationMu.Lock()
			upstreamChanged := upstream != lastUpstream
			if upstreamChanged {
				lastUpstream = upstream
			}
			needsRetry := s.browserLocation.accepted != nil && s.browserLocation.applied == nil && s.browserLocation.lastError != "" && time.Since(s.browserLocation.lastAttempt) >= browserLocationApplyTimeout
			if s.browserLocation.accepted != nil && (upstreamChanged || needsRetry) {
				s.startBrowserLocationReconcileLocked(*s.browserLocation.accepted)
			}
			s.browserLocationMu.Unlock()
		}
	}
}

func (s *ApiService) browserLocationSnapshot() browserLocationStatus {
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	return browserLocationStatus{
		ActiveEpoch: s.browserLocation.activeEpoch,
		Accepted:    s.browserLocation.accepted,
		Applied:     s.browserLocation.applied,
		Components:  s.browserLocation.components,
		Error:       s.browserLocation.lastError,
	}
}

func (s *ApiService) BrowserLocationMetrics() metrics.BrowserLocationSnapshot {
	s.browserLocationMu.Lock()
	components := s.browserLocation.components
	converged := s.browserLocation.applied != nil && s.browserLocation.accepted != nil && browserLocationBundlesEqual(*s.browserLocation.applied, *s.browserLocation.accepted)
	s.browserLocationMu.Unlock()
	return metrics.BrowserLocationSnapshot{
		Accepted:           s.browserLocation.acceptedCount.Load(),
		Applied:            s.browserLocation.appliedCount.Load(),
		Retries:            s.browserLocation.retries.Load(),
		Stale:              s.browserLocation.stale.Load(),
		Conflicts:          s.browserLocation.conflicts.Load(),
		EpochRejects:       s.browserLocation.epochRejects.Load(),
		Failures:           s.browserLocation.failures.Load(),
		ConvergenceMs:      s.browserLocation.convergenceMs.Load(),
		Converged:          converged,
		TimeZoneConverged:  components.TimeZone,
		BrowserConverged:   components.Browser,
		RenderersConverged: components.Renderers,
		NetworkConverged:   components.NetworkContexts,
	}
}

// ResetBrowserLocationHTTP is the lease-authoritative epoch transition. Ordinary
// configure requests cannot change epochs. The instance JWT is not exposed on
// customer CDP or session APIs and binds the reset to this VM lease.
func (s *ApiService) ResetBrowserLocationHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	expected := os.Getenv("KERNEL_INSTANCE_JWT")
	if expected == "" || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	defer r.Body.Close()
	var reset struct {
		PreviousEpoch string                `json:"previous_epoch"`
		Bundle        browserLocationBundle `json:"bundle"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reset); err != nil {
		http.Error(w, "invalid browser location reset", http.StatusBadRequest)
		return
	}
	raw, _ := json.Marshal(reset.Bundle)
	validated, err := validateBrowserLocationBundle(string(raw))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	validate := s.browserLocationValidate
	if validate == nil {
		validate = s.validateBrowserLocationSupport
	}
	if err := validate(r.Context(), validated); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.resetBrowserLocation(reset.PreviousEpoch, validated); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// GetBrowserLocationHTTP exposes accepted/applied component state for internal reconciliation.
func (s *ApiService) GetBrowserLocationHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.browserLocationSnapshot())
}

func browserLocationBundlesEqual(a, b browserLocationBundle) bool {
	return a.Epoch == b.Epoch && a.Generation == b.Generation && a.TimeZone == b.TimeZone && a.Locale == b.Locale && slices.Equal(a.Languages, b.Languages)
}
