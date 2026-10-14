package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kernel/kernel-images/server/lib/browserlocation"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/metrics"
	"golang.org/x/text/language"
)

const (
	browserLocationApplyTimeout = 5 * time.Second
	// browserLocationRoundTimeout bounds how long one attempt waits for
	// Chromium's acknowledgements before it sends the location again.
	browserLocationRoundTimeout = 1500 * time.Millisecond
	// maxBrowserLocationGeneration is the largest integer a CDP number holds
	// exactly, so the largest generation Chromium accepts.
	maxBrowserLocationGeneration = 1<<53 - 1
)

type browserLocationBundle struct {
	Generation uint64   `json:"generation"`
	TimeZone   string   `json:"timezone"`
	Locale     string   `json:"locale"`
	Languages  []string `json:"languages"`
}

// browserLocationComponents records the acknowledgements Chromium returned for
// the accepted bundle's generation.
type browserLocationComponents struct {
	// TimeZone: /etc/localtime was replaced and every renderer observed it.
	TimeZone bool `json:"timezone"`
	// Browser: Chromium holds this generation's locale and languages.
	Browser bool `json:"browser"`
	// Renderers: every renderer applied them to its pages and existing workers.
	Renderers       bool `json:"renderers"`
	NetworkContexts bool `json:"network_contexts"`
}

type browserLocationStatus struct {
	Accepted            *browserLocationBundle    `json:"accepted,omitempty"`
	Applied             *browserLocationBundle    `json:"applied,omitempty"`
	Components          browserLocationComponents `json:"components"`
	Error               string                    `json:"error,omitempty"`
	CapabilitiesVersion string                    `json:"capabilities_version"`
}

type browserLocationDurableState struct {
	Accepted *browserLocationBundle `json:"accepted,omitempty"`
}

// setBrowserLocaltime is replaced in tests, which must not change the host's
// /etc/localtime.
var setBrowserLocaltime = browserlocation.SetLocaltime

var (
	errStaleBrowserLocation       = errors.New("browser location generation is stale")
	errConflictBrowserLocation    = errors.New("browser location generation conflicts with accepted payload")
	errBrowserLocationUnsupported = errors.New("this browser cannot change location")
)

func validateBrowserLocationBundle(raw string) (browserLocationBundle, error) {
	var bundle browserLocationBundle
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil {
		return bundle, fmt.Errorf("invalid browser_location JSON")
	}
	if bundle.Generation == 0 {
		return bundle, fmt.Errorf("browser_location generation is required")
	}
	if bundle.Generation > maxBrowserLocationGeneration {
		return bundle, fmt.Errorf("browser_location generation must not exceed 2^53-1")
	}
	capabilities := browserlocation.Current()
	if !capabilities.SupportsTimeZone(bundle.TimeZone) {
		return bundle, fmt.Errorf("unsupported browser_location timezone")
	}
	zonePath, err := browserlocation.ZonePath(bundle.TimeZone)
	if err != nil {
		return bundle, err
	}
	if _, err := os.Stat(zonePath); err != nil {
		return bundle, fmt.Errorf("unsupported browser_location timezone")
	}
	if !capabilities.SupportsLocale(bundle.Locale) {
		return bundle, fmt.Errorf("unsupported browser_location locale")
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

func (s *ApiService) acceptBrowserLocation(bundle browserLocationBundle) error {
	s.browserLocationApplyMu.Lock()
	defer s.browserLocationApplyMu.Unlock()
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
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
	if err := s.persistBrowserLocationState(browserLocationDurableState{Accepted: &copy}); err != nil {
		return err
	}
	s.browserLocation.accepted = &copy
	s.browserLocation.chromiumGeneration = copy.Generation
	s.browserLocation.applied = nil
	s.browserLocation.components = browserLocationComponents{}
	s.browserLocation.acceptedCount.Add(1)
	s.startBrowserLocationReconcileLocked(bundle)
	return nil
}

func (s *ApiService) startBrowserLocationReconcileLocked(bundle browserLocationBundle) {
	s.browserLocation.lastAttempt = time.Now()
	// The error belongs to the attempt this one replaces.
	s.browserLocation.lastError = ""
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
	for {
		if ctx.Err() != nil || !s.browserLocationCurrent(bundle) {
			return
		}
		err := s.sendBrowserLocation(ctx, bundle)
		if err == nil {
			roundDeadline := time.Now().Add(browserLocationRoundTimeout)
			if roundDeadline.After(deadline) {
				roundDeadline = deadline
			}
			err = s.waitBrowserLocation(ctx, bundle, roundDeadline)
		}
		if ctx.Err() != nil || !s.browserLocationCurrent(bundle) {
			return
		}
		if err == nil {
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
		if errors.Is(err, errBrowserLocationUnsupported) || time.Now().After(deadline) {
			s.browserLocationMu.Lock()
			if accepted := s.browserLocation.accepted; accepted != nil && browserLocationBundlesEqual(*accepted, bundle) {
				s.browserLocation.lastError = err.Error()
				s.browserLocation.failures.Add(1)
			}
			s.browserLocationMu.Unlock()
			logger.FromContext(ctx).Error("browser location did not converge", "error", err, "generation", bundle.Generation)
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

// sendBrowserLocation points /etc/localtime at the bundle's timezone and sends
// the bundle to Chromium once.
func (s *ApiService) sendBrowserLocation(ctx context.Context, bundle browserLocationBundle) error {
	s.browserLocationApplyMu.Lock()
	defer s.browserLocationApplyMu.Unlock()
	if ctx.Err() != nil || !s.browserLocationCurrent(bundle) {
		return errStaleBrowserLocation
	}
	if err := setBrowserLocaltime(bundle.TimeZone); err != nil {
		return err
	}
	return s.withCDPClientTimeout(ctx, time.Second, func(cdpCtx context.Context, client *cdpclient.Client) error {
		languages := strings.Join(bundle.Languages, ",")
		timezone := browserlocation.HostTimeZone(bundle.TimeZone)
		generation := s.chromiumGeneration(bundle)
		err := client.SetBrowserLocation(cdpCtx, generation, bundle.Locale, languages, timezone)
		if isCDPMethodNotFound(err) {
			s.recordBrowserLocationSupport(false)
			return errBrowserLocationUnsupported
		}
		if !isCDPInvalidParams(err) {
			return err
		}
		// Chromium holds a generation the image did not send, or the same
		// generation with other values. Move above it.
		observed, getErr := client.GetBrowserLocation(cdpCtx)
		if getErr != nil || observed.Generation < generation || observed.Generation >= maxBrowserLocationGeneration {
			return err
		}
		generation = observed.Generation + 1
		s.setChromiumGeneration(bundle, generation)
		return client.SetBrowserLocation(cdpCtx, generation, bundle.Locale, languages, timezone)
	})
}

// waitBrowserLocation polls Chromium until every component acknowledges the
// bundle, or until deadline.
func (s *ApiService) waitBrowserLocation(ctx context.Context, bundle browserLocationBundle, deadline time.Time) error {
	return s.withCDPClientTimeout(ctx, time.Until(deadline), func(cdpCtx context.Context, client *cdpclient.Client) error {
		languages := strings.Join(bundle.Languages, ",")
		generation := s.chromiumGeneration(bundle)
		for {
			observed, err := client.GetBrowserLocation(cdpCtx)
			if err != nil {
				if cdpCtx.Err() != nil {
					return fmt.Errorf("browser location components have not converged")
				}
				return err
			}
			if observed.Generation != generation {
				return fmt.Errorf("browser location generation %d is not current", bundle.Generation)
			}
			components := browserLocationComponents{
				Browser:         observed.Locale == bundle.Locale && observed.AcceptLanguages == languages,
				TimeZone:        observed.TimeZone == browserlocation.HostTimeZone(bundle.TimeZone) && observed.TimeZoneConverged,
				Renderers:       observed.RenderersConverged,
				NetworkContexts: observed.NetworkContextsConverged,
			}
			s.browserLocationMu.Lock()
			if accepted := s.browserLocation.accepted; accepted != nil && browserLocationBundlesEqual(*accepted, bundle) {
				s.browserLocation.components = components
			}
			s.browserLocationMu.Unlock()
			if components.Browser && components.TimeZone && components.Renderers && components.NetworkContexts {
				return nil
			}
			select {
			case <-cdpCtx.Done():
				return fmt.Errorf("browser location components have not converged")
			case <-time.After(25 * time.Millisecond):
			}
		}
	})
}

// chromiumGeneration is the generation Chromium holds the accepted bundle
// under. It is the bundle's own unless Chromium was already past it.
func (s *ApiService) chromiumGeneration(bundle browserLocationBundle) uint64 {
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	if accepted := s.browserLocation.accepted; accepted != nil && browserLocationBundlesEqual(*accepted, bundle) && s.browserLocation.chromiumGeneration > bundle.Generation {
		return s.browserLocation.chromiumGeneration
	}
	return bundle.Generation
}

func (s *ApiService) setChromiumGeneration(bundle browserLocationBundle, generation uint64) {
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	if accepted := s.browserLocation.accepted; accepted != nil && browserLocationBundlesEqual(*accepted, bundle) {
		s.browserLocation.chromiumGeneration = generation
	}
}

func isCDPMethodNotFound(err error) bool {
	var cdpErr *cdpclient.Error
	return errors.As(err, &cdpErr) && cdpErr.Code == -32601
}

func isCDPInvalidParams(err error) bool {
	var cdpErr *cdpclient.Error
	return errors.As(err, &cdpErr) && cdpErr.Code == -32602
}

// browserLocationSupported reports false once the running Chromium has
// answered that it lacks the location commands, and true otherwise. Each
// Chromium process is probed once.
func (s *ApiService) browserLocationSupported(ctx context.Context) bool {
	if s.upstreamMgr == nil {
		return true
	}
	upstream := s.upstreamMgr.Current()
	if upstream == "" {
		return true
	}
	s.browserLocationMu.Lock()
	probed, supported := s.browserLocation.probedUpstream == upstream, s.browserLocation.supported
	s.browserLocationMu.Unlock()
	if probed {
		return supported
	}
	err := s.withCDPClientTimeout(ctx, time.Second, func(cdpCtx context.Context, client *cdpclient.Client) error {
		_, err := client.GetBrowserLocation(cdpCtx)
		return err
	})
	if err != nil && !isCDPMethodNotFound(err) {
		return true
	}
	s.recordBrowserLocationSupport(err == nil)
	return err == nil
}

// recordBrowserLocationSupport records whether the running Chromium has the
// location commands.
func (s *ApiService) recordBrowserLocationSupport(supported bool) {
	if s.upstreamMgr == nil {
		return
	}
	upstream := s.upstreamMgr.Current()
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	s.browserLocation.probedUpstream = upstream
	s.browserLocation.supported = supported
}

// browserLocationCurrent reports whether bundle is still the accepted one.
func (s *ApiService) browserLocationCurrent(bundle browserLocationBundle) bool {
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	return s.browserLocation.accepted != nil && browserLocationBundlesEqual(*s.browserLocation.accepted, bundle)
}

func (s *ApiService) browserLocationStatePath() string {
	return browserlocation.StatePath()
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
			if upstreamChanged {
				// A new Chromium process has not acknowledged anything yet.
				s.browserLocation.applied = nil
				s.browserLocation.components = browserLocationComponents{}
			}
			unsupported := s.browserLocation.probedUpstream == upstream && !s.browserLocation.supported
			needsRetry := s.browserLocation.accepted != nil && s.browserLocation.applied == nil && s.browserLocation.lastError != "" && !unsupported && time.Since(s.browserLocation.lastAttempt) >= browserLocationApplyTimeout
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
		Accepted:            s.browserLocation.accepted,
		Applied:             s.browserLocation.applied,
		Components:          s.browserLocation.components,
		Error:               s.browserLocation.lastError,
		CapabilitiesVersion: browserlocation.Current().Version,
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
		Failures:           s.browserLocation.failures.Load(),
		ConvergenceMs:      s.browserLocation.convergenceMs.Load(),
		Converged:          converged,
		TimeZoneConverged:  components.TimeZone,
		BrowserConverged:   components.Browser,
		RenderersConverged: components.Renderers,
		NetworkConverged:   components.NetworkContexts,
	}
}

// GetBrowserLocationHTTP exposes accepted/applied component state for internal
// reconciliation. Applied is confirmed against the running Chromium, so a
// restarted or unreachable browser is never reported as applied.
func (s *ApiService) GetBrowserLocationHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !s.browserLocationSupported(r.Context()) {
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "unsupported", "message": errBrowserLocationUnsupported.Error()})
		return
	}
	status := s.browserLocationSnapshot()
	if status.Applied != nil {
		if !s.browserLocationCurrent(*status.Applied) || !s.browserLocationLive(r.Context(), *status.Applied) {
			status.Applied = nil
			status.Components = browserLocationComponents{}
		}
	}
	_ = json.NewEncoder(w).Encode(status)
}

// browserLocationLive reports whether the running Chromium holds bundle with
// every component acknowledged.
func (s *ApiService) browserLocationLive(ctx context.Context, bundle browserLocationBundle) bool {
	if s.upstreamMgr == nil {
		return false
	}
	var observed cdpclient.BrowserLocation
	err := s.withCDPClientTimeout(ctx, 500*time.Millisecond, func(cdpCtx context.Context, client *cdpclient.Client) error {
		var err error
		observed, err = client.GetBrowserLocation(cdpCtx)
		return err
	})
	return err == nil && observed.Generation == s.chromiumGeneration(bundle) &&
		observed.Locale == bundle.Locale && observed.AcceptLanguages == strings.Join(bundle.Languages, ",") &&
		observed.TimeZone == browserlocation.HostTimeZone(bundle.TimeZone) && observed.TimeZoneConverged &&
		observed.RenderersConverged && observed.NetworkContextsConverged
}

func browserLocationBundlesEqual(a, b browserLocationBundle) bool {
	return a.Generation == b.Generation && a.TimeZone == b.TimeZone && a.Locale == b.Locale && slices.Equal(a.Languages, b.Languages)
}
