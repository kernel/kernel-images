package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kernel/kernel-images/server/lib/browserlocation"
	"github.com/kernel/kernel-images/server/lib/cdpclient"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/metrics"
	"github.com/kernel/kernel-images/server/lib/oapi"
	"golang.org/x/text/language"
)

const (
	browserLocationApplyTimeout = 5 * time.Second
	// browserLocationRoundTimeout bounds how long one attempt waits for
	// Chromium's acknowledgements before it sends the location again.
	browserLocationRoundTimeout = 1500 * time.Millisecond
	// browserLocationPendingRendererWait is how long a round waits for
	// renderers that haven't acknowledged before it counts them as pending:
	// blocked by a dialog or a long task, with the location queued for when
	// they resume.
	browserLocationPendingRendererWait = time.Second
	// browserLocationPendingPoll is how often an applied location's pending
	// renderers are checked until they acknowledge it.
	browserLocationPendingPoll = 500 * time.Millisecond
	// maxBrowserLocationGeneration is the largest integer a CDP number holds
	// exactly, so the largest generation Chromium accepts.
	maxBrowserLocationGeneration = 1<<53 - 1
)

type browserLocationDurableState struct {
	Accepted *oapi.BrowserLocationBundle `json:"accepted,omitempty"`
}

// setBrowserLocaltime is replaced in tests, which must not change the host's
// /etc/localtime.
var setBrowserLocaltime = browserlocation.SetLocaltime

var (
	errStaleBrowserLocation       = errors.New("browser location generation is stale")
	errConflictBrowserLocation    = errors.New("browser location generation conflicts with accepted payload")
	errBrowserLocationUnsupported = errors.New("this browser cannot change location")
	// errBrowserLocationNotRestored means Chromium restarted and is running,
	// but without the accepted location.
	errBrowserLocationNotRestored = errors.New("browser location was not restored after chromium restart")
)

func validateBrowserLocationBundle(bundle oapi.BrowserLocationBundle) error {
	if bundle.Generation < 1 {
		return fmt.Errorf("location generation must be positive")
	}
	if bundle.Generation > maxBrowserLocationGeneration {
		return fmt.Errorf("location generation must not exceed 2^53-1")
	}
	capabilities := browserlocation.Current()
	if !capabilities.SupportsTimeZone(bundle.Timezone) {
		return fmt.Errorf("unsupported location timezone")
	}
	zonePath, err := browserlocation.ZonePath(bundle.Timezone)
	if err != nil {
		return err
	}
	if _, err := os.Stat(zonePath); err != nil {
		return fmt.Errorf("unsupported location timezone")
	}
	if !capabilities.SupportsLocale(bundle.Locale) {
		return fmt.Errorf("unsupported location locale")
	}
	if len(bundle.Languages) == 0 || bundle.Languages[0] != bundle.Locale {
		return fmt.Errorf("location languages must start with locale")
	}
	for _, value := range bundle.Languages {
		tag, err := language.Parse(value)
		if err != nil || tag.String() != value {
			return fmt.Errorf("location languages must be canonical BCP 47")
		}
	}
	return nil
}

func (s *ApiService) acceptBrowserLocation(bundle oapi.BrowserLocationBundle) error {
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
			// A resend confirms the location again, so a caller waiting on it
			// can't succeed on an earlier confirmation the browser has since lost.
			s.browserLocation.retries.Add(1)
			s.browserLocation.applied = nil
			s.browserLocation.components = oapi.BrowserLocationComponents{}
			s.startBrowserLocationReconcileLocked(bundle)
			return nil
		}
	}
	copy := bundle
	if err := s.persistBrowserLocationState(browserLocationDurableState{Accepted: &copy}); err != nil {
		return err
	}
	s.browserLocation.accepted = &copy
	s.browserLocation.chromiumGeneration = uint64(copy.Generation)
	s.browserLocation.applied = nil
	s.browserLocation.components = oapi.BrowserLocationComponents{}
	s.browserLocation.acceptedCount.Add(1)
	s.startBrowserLocationReconcileLocked(bundle)
	return nil
}

func (s *ApiService) startBrowserLocationReconcileLocked(bundle oapi.BrowserLocationBundle) {
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

func (s *ApiService) reconcileBrowserLocation(ctx context.Context, bundle oapi.BrowserLocationBundle) {
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
			pending := false
			s.browserLocationMu.Lock()
			// A resend that started after this attempt cancelled it.
			if accepted := s.browserLocation.accepted; ctx.Err() == nil && accepted != nil && browserLocationBundlesEqual(*accepted, bundle) {
				copy := bundle
				s.browserLocation.applied = &copy
				s.browserLocation.lastError = ""
				s.browserLocation.appliedCount.Add(1)
				s.browserLocation.convergenceMs.Store(uint64(time.Since(started).Milliseconds()))
				pending = s.browserLocation.components.RenderersPending > 0
			}
			s.browserLocationMu.Unlock()
			if pending {
				s.watchPendingRenderers(ctx, bundle)
			}
			return
		}
		if errors.Is(err, errBrowserLocationUnsupported) || time.Now().After(deadline) {
			s.browserLocationMu.Lock()
			if accepted := s.browserLocation.accepted; ctx.Err() == nil && accepted != nil && browserLocationBundlesEqual(*accepted, bundle) {
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

// watchPendingRenderers follows the renderers that were pending when bundle
// applied until they acknowledge it. If the browser then reports a component
// that didn't converge, the location is no longer applied and is sent again.
func (s *ApiService) watchPendingRenderers(ctx context.Context, bundle oapi.BrowserLocationBundle) {
	ticker := time.NewTicker(browserLocationPendingPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		components, confirmed, err := s.browserLocationLive(ctx, bundle)
		if err != nil {
			continue
		}
		s.browserLocationMu.Lock()
		if accepted := s.browserLocation.accepted; ctx.Err() != nil || accepted == nil || !browserLocationBundlesEqual(*accepted, bundle) {
			s.browserLocationMu.Unlock()
			return
		}
		s.browserLocation.components = components
		if !confirmed {
			s.browserLocation.applied = nil
			s.startBrowserLocationReconcileLocked(bundle)
		}
		s.browserLocationMu.Unlock()
		if !confirmed || components.Renderers {
			return
		}
	}
}

// sendBrowserLocation points /etc/localtime at the bundle's timezone and sends
// the bundle to Chromium once.
func (s *ApiService) sendBrowserLocation(ctx context.Context, bundle oapi.BrowserLocationBundle) error {
	s.browserLocationApplyMu.Lock()
	defer s.browserLocationApplyMu.Unlock()
	if ctx.Err() != nil || !s.browserLocationCurrent(bundle) {
		return errStaleBrowserLocation
	}
	if err := setBrowserLocaltime(bundle.Timezone); err != nil {
		return err
	}
	return s.withCDPClientTimeout(ctx, time.Second, func(cdpCtx context.Context, client *cdpclient.Client) error {
		languages := strings.Join(bundle.Languages, ",")
		timezone := browserlocation.HostTimeZone(bundle.Timezone)
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
// bundle, or until deadline. Renderers that haven't acknowledged within
// browserLocationPendingRendererWait while Chromium reports them pending don't
// hold it back.
func (s *ApiService) waitBrowserLocation(ctx context.Context, bundle oapi.BrowserLocationBundle, deadline time.Time) error {
	started := time.Now()
	return s.withCDPClientTimeout(ctx, time.Until(deadline), func(cdpCtx context.Context, client *cdpclient.Client) error {
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
			components := browserLocationComponents(observed, bundle)
			s.browserLocationMu.Lock()
			if accepted := s.browserLocation.accepted; accepted != nil && browserLocationBundlesEqual(*accepted, bundle) {
				s.browserLocation.components = components
			}
			s.browserLocationMu.Unlock()
			if browserLocationConfirmed(components, time.Since(started) >= browserLocationPendingRendererWait) {
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
func (s *ApiService) chromiumGeneration(bundle oapi.BrowserLocationBundle) uint64 {
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	if accepted := s.browserLocation.accepted; accepted != nil && browserLocationBundlesEqual(*accepted, bundle) && s.browserLocation.chromiumGeneration > uint64(bundle.Generation) {
		return s.browserLocation.chromiumGeneration
	}
	return uint64(bundle.Generation)
}

func (s *ApiService) setChromiumGeneration(bundle oapi.BrowserLocationBundle, generation uint64) {
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
func (s *ApiService) browserLocationCurrent(bundle oapi.BrowserLocationBundle) bool {
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
	for {
		select {
		case <-s.lifecycleCtx.Done():
			return
		case <-ticker.C:
			if upstream := s.upstreamMgr.Current(); upstream != "" {
				s.observeChromiumUpstream(upstream)
			}
		}
	}
}

// observeChromiumUpstream applies the accepted location to a Chromium process
// it hasn't been applied to, and retries a failed apply.
func (s *ApiService) observeChromiumUpstream(upstream string) {
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	upstreamChanged := upstream != s.browserLocation.upstream
	if upstreamChanged {
		s.browserLocation.upstream = upstream
		// A new Chromium process has not acknowledged anything yet.
		s.browserLocation.applied = nil
		s.browserLocation.components = oapi.BrowserLocationComponents{}
	}
	unsupported := s.browserLocation.probedUpstream == upstream && !s.browserLocation.supported
	needsRetry := s.browserLocation.accepted != nil && s.browserLocation.applied == nil && s.browserLocation.lastError != "" && !unsupported && time.Since(s.browserLocation.lastAttempt) >= browserLocationApplyTimeout
	if s.browserLocation.accepted != nil && (upstreamChanged || needsRetry) {
		s.startBrowserLocationReconcileLocked(*s.browserLocation.accepted)
	}
}

func (s *ApiService) browserLocationSnapshot() oapi.BrowserLocationStatus {
	s.browserLocationMu.Lock()
	defer s.browserLocationMu.Unlock()
	return oapi.BrowserLocationStatus{
		Accepted:            s.browserLocation.accepted,
		Applied:             s.browserLocation.applied,
		Components:          s.browserLocation.components,
		Error:               browserLocationError(s.browserLocation.lastError),
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
		TimeZoneConverged:  components.Timezone,
		BrowserConverged:   components.Browser,
		RenderersConverged: components.Renderers,
		NetworkConverged:   components.NetworkContexts,
	}
}

// GetBrowserLocation reports accepted and applied component state. Applied and
// its components are read from the running Chromium, so a restarted or
// unreachable browser is never reported as applied.
func (s *ApiService) GetBrowserLocation(ctx context.Context, _ oapi.GetBrowserLocationRequestObject) (oapi.GetBrowserLocationResponseObject, error) {
	if !s.browserLocationSupported(ctx) {
		return oapi.GetBrowserLocation501JSONResponse{Message: errBrowserLocationUnsupported.Error()}, nil
	}
	status := s.browserLocationSnapshot()
	if status.Applied != nil {
		components, live, err := s.browserLocationLive(ctx, *status.Applied)
		if err != nil || !s.browserLocationCurrent(*status.Applied) || !live {
			status.Applied = nil
			status.Components = oapi.BrowserLocationComponents{}
		} else {
			status.Components = components
		}
	}
	return oapi.GetBrowserLocation200JSONResponse(status), nil
}

// SetBrowserLocation accepts a location and returns once the running browser
// confirms it.
func (s *ApiService) SetBrowserLocation(ctx context.Context, request oapi.SetBrowserLocationRequestObject) (oapi.SetBrowserLocationResponseObject, error) {
	if request.Body == nil {
		return oapi.SetBrowserLocation400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "request body is required"}}, nil
	}
	bundle := *request.Body
	if err := validateBrowserLocationBundle(bundle); err != nil {
		return oapi.SetBrowserLocation400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
	}
	return s.applyBrowserLocation(ctx, bundle), nil
}

// applyBrowserLocation accepts a validated bundle and waits until the running
// browser confirms it.
func (s *ApiService) applyBrowserLocation(ctx context.Context, bundle oapi.BrowserLocationBundle) oapi.SetBrowserLocationResponseObject {
	if !s.browserLocationSupported(ctx) {
		return oapi.SetBrowserLocation501JSONResponse{Message: errBrowserLocationUnsupported.Error()}
	}
	err := s.acceptBrowserLocation(bundle)
	if err == nil {
		err = s.waitBrowserLocationApplied(ctx, bundle)
	}
	switch {
	case err == nil:
		return oapi.SetBrowserLocation200JSONResponse(s.browserLocationSnapshot())
	case errors.Is(err, errStaleBrowserLocation), errors.Is(err, errConflictBrowserLocation):
		return oapi.SetBrowserLocation409JSONResponse{Message: err.Error()}
	}
	// Includes a browser found unsupported after the location was accepted,
	// which keeps it accepted.
	logger.FromContext(ctx).Error("browser location did not apply", "error", err, "generation", bundle.Generation)
	return oapi.SetBrowserLocation500JSONResponse{Message: err.Error()}
}

// reapplyBrowserLocation applies the accepted location to a Chromium the image
// has just restarted, before the restart completes, so nothing that follows it
// sees the browser without its location. Chromium starts with the location's
// timezone and Accept-Language but not its Intl locale, which on Linux follows
// the environment, so a failure fails the restart; the lifecycle loop keeps
// retrying.
func (s *ApiService) reapplyBrowserLocation(ctx context.Context) error {
	s.browserLocationMu.Lock()
	if s.browserLocation.accepted == nil {
		s.browserLocationMu.Unlock()
		return nil
	}
	bundle := *s.browserLocation.accepted
	// The previous Chromium's confirmation says nothing about this one. The
	// lifecycle loop must not start over once this one confirms.
	s.browserLocation.applied = nil
	s.browserLocation.components = oapi.BrowserLocationComponents{}
	if s.upstreamMgr != nil {
		s.browserLocation.upstream = s.upstreamMgr.Current()
	}
	s.startBrowserLocationReconcileLocked(bundle)
	s.browserLocationMu.Unlock()
	if err := s.waitBrowserLocationApplied(ctx, bundle); err != nil {
		logger.FromContext(ctx).Error("browser location did not apply after chromium restart", "error", err, "generation", bundle.Generation)
		return fmt.Errorf("%w: %v", errBrowserLocationNotRestored, err)
	}
	return nil
}

func browserLocationError(message string) *string {
	if message == "" {
		return nil
	}
	return &message
}

// waitBrowserLocationApplied waits until the running browser confirms bundle,
// or reports why it didn't.
func (s *ApiService) waitBrowserLocationApplied(ctx context.Context, bundle oapi.BrowserLocationBundle) error {
	ctx, cancel := context.WithTimeout(ctx, browserLocationApplyTimeout+time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.browserLocationMu.Lock()
		accepted, applied, lastError := s.browserLocation.accepted, s.browserLocation.applied, s.browserLocation.lastError
		s.browserLocationMu.Unlock()
		switch {
		case accepted == nil || !browserLocationBundlesEqual(*accepted, bundle):
			return errConflictBrowserLocation
		case applied != nil && browserLocationBundlesEqual(*applied, bundle):
			return nil
		case lastError != "":
			return errors.New(lastError)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("browser location did not apply: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// browserLocationLive returns the running Chromium's components for bundle,
// and whether it holds bundle with every component confirmed. Pending
// renderers have it queued, so they don't count against it. An error means
// Chromium couldn't be asked.
func (s *ApiService) browserLocationLive(ctx context.Context, bundle oapi.BrowserLocationBundle) (oapi.BrowserLocationComponents, bool, error) {
	if s.upstreamMgr == nil {
		return oapi.BrowserLocationComponents{}, false, errors.New("chromium is not running")
	}
	var observed cdpclient.BrowserLocation
	err := s.withCDPClientTimeout(ctx, 500*time.Millisecond, func(cdpCtx context.Context, client *cdpclient.Client) error {
		var err error
		observed, err = client.GetBrowserLocation(cdpCtx)
		return err
	})
	if err != nil {
		return oapi.BrowserLocationComponents{}, false, err
	}
	if observed.Generation != s.chromiumGeneration(bundle) {
		return oapi.BrowserLocationComponents{}, false, nil
	}
	components := browserLocationComponents(observed, bundle)
	return components, browserLocationConfirmed(components, true), nil
}

// browserLocationComponents compares what Chromium reports for the generation
// it holds bundle under with bundle.
func browserLocationComponents(observed cdpclient.BrowserLocation, bundle oapi.BrowserLocationBundle) oapi.BrowserLocationComponents {
	return oapi.BrowserLocationComponents{
		Browser:          observed.Locale == bundle.Locale && observed.AcceptLanguages == strings.Join(bundle.Languages, ","),
		Timezone:         observed.TimeZone == browserlocation.HostTimeZone(bundle.Timezone) && observed.TimeZoneConverged,
		Renderers:        observed.RenderersConverged,
		RenderersPending: observed.RenderersPending,
		NetworkContexts:  observed.NetworkContextsConverged,
	}
}

// browserLocationConfirmed reports whether every component confirmed the
// location, allowing renderers Chromium reports pending when allowPending.
func browserLocationConfirmed(c oapi.BrowserLocationComponents, allowPending bool) bool {
	if !c.Browser || !c.Timezone || !c.NetworkContexts {
		return false
	}
	return c.Renderers || (allowPending && c.RenderersPending > 0)
}

func browserLocationBundlesEqual(a, b oapi.BrowserLocationBundle) bool {
	return a.Generation == b.Generation && a.Timezone == b.Timezone && a.Locale == b.Locale && slices.Equal(a.Languages, b.Languages)
}
