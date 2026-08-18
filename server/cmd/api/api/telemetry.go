package api

import (
	"context"
	"time"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/nrednav/cuid2"
	"github.com/samber/lo"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/telemetry"
)

// otlpStopTimeout bounds how long a runtime export toggle-off waits to drain.
const otlpStopTimeout = 5 * time.Second

// GetTelemetry handles GET /telemetry.
// Returns the current telemetry configuration. Returns 404 if telemetry is not configured.
func (s *ApiService) GetTelemetry(_ context.Context, _ oapi.GetTelemetryRequestObject) (oapi.GetTelemetryResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if !s.telemetrySession.Active() {
		return oapi.GetTelemetry404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "telemetry is not configured"}}, nil
	}
	return oapi.GetTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
}

// PutTelemetry handles PUT /telemetry.
// Sets the telemetry configuration. Returns 201 if not previously configured, 200 if it was.
// Setting every configurable category to enabled:false clears the configuration (200).
func (s *ApiService) PutTelemetry(ctx context.Context, req oapi.PutTelemetryRequestObject) (oapi.PutTelemetryResponseObject, error) {
	// Reconcile export after monitorMu is released (defers run LIFO), so the
	// toggle-off drain does not hold the API-wide lock. It reads the committed
	// desired state from the session, so it is correct regardless of how this
	// request exits.
	defer s.reconcileExport(ctx)
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	cfg, allDisabled, err := telemetryConfigFromOAPI(req.Body)
	if err != nil {
		return oapi.PutTelemetry400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
	}

	wasActive := s.telemetrySession.Active()

	if allDisabled {
		if wasActive {
			s.telemetrySession.Stop()
			s.stopTelemetryState()
		}
		return oapi.PutTelemetry200JSONResponse(s.stoppedTelemetryResponse()), nil
	}

	// Commit the config first so the filter is live before the collector emits,
	// then reconcile. On collector-start failure, roll back to the prior state
	// so a 500 never leaves telemetry half-applied.
	var prev telemetry.TelemetryConfig
	if wasActive {
		prev = s.telemetrySession.Config()
		s.telemetrySession.UpdateConfig(cfg)
	} else {
		s.telemetrySession.Start(cuid2.Generate(), cfg)
	}

	if err := s.reconcileTelemetryState(cfg.Categories); err != nil {
		s.rollbackTelemetry(wasActive, prev)
		logger.FromContext(ctx).Error("failed to apply telemetry state", "err", err)
		return oapi.PutTelemetry500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start telemetry"}}, nil
	}

	if wasActive {
		return oapi.PutTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
	}
	return oapi.PutTelemetry201JSONResponse(s.buildTelemetryResponse()), nil
}

// PatchTelemetry handles PATCH /telemetry.
// Partially updates the telemetry configuration. Returns 404 if not configured.
// Setting every configurable category to enabled:false clears the configuration (200).
func (s *ApiService) PatchTelemetry(ctx context.Context, req oapi.PatchTelemetryRequestObject) (oapi.PatchTelemetryResponseObject, error) {
	// See PutTelemetry: reconcile export after monitorMu is released.
	defer s.reconcileExport(ctx)
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if !s.telemetrySession.Active() {
		return oapi.PatchTelemetry404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "telemetry is not configured"}}, nil
	}

	// Nothing to merge when neither the category block nor the export toggle is
	// present; skip the reconcile and echo current state, as before.
	if req.Body == nil || (req.Body.Browser == nil && req.Body.Export == nil) {
		return oapi.PatchTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
	}

	prev := s.telemetrySession.Config()
	cfg, allDisabled := mergeTelemetryConfig(prev, req.Body)
	if allDisabled {
		s.telemetrySession.Stop()
		s.stopTelemetryState()
		return oapi.PatchTelemetry200JSONResponse(s.stoppedTelemetryResponse()), nil
	}

	// Commit first so the filter is live before the collector emits, then
	// reconcile and roll back on collector-start failure.
	s.telemetrySession.UpdateConfig(cfg)
	if err := s.reconcileTelemetryState(cfg.Categories); err != nil {
		s.rollbackTelemetry(true, prev)
		logger.FromContext(ctx).Error("failed to apply telemetry state", "err", err)
		return oapi.PatchTelemetry500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to apply telemetry"}}, nil
	}
	return oapi.PatchTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
}

// reconcileTelemetryState reconciles the CDP collector and the api_call
// middleware to the desired category set. The collector runs iff a CDP category
// is captured; the middleware emits iff control or platform is, since it is the
// sole producer of both api_call and platform_api_call. Callers commit the
// session config first so the filter is live before the collector emits; this
// returns an error only when the collector fails to start, leaving the caller to
// roll back.
func (s *ApiService) reconcileTelemetryState(cats []oapi.TelemetryEventCategory) error {
	if containsCategory(cats, events.Control) || containsCategory(cats, events.Platform) {
		EnableTelemetryMiddleware()
	} else {
		DisableTelemetryMiddleware()
	}

	switch {
	case events.HasCDPCategory(cats) && !s.cdpMonitor.IsRunning():
		return s.cdpMonitor.Start(s.lifecycleCtx)
	case !events.HasCDPCategory(cats) && s.cdpMonitor.IsRunning():
		s.cdpMonitor.Stop()
	}
	return nil
}

// rollbackTelemetry restores telemetry to its prior state after a failed apply.
// A fresh session is torn down; an updated session is reverted to prev. Reverting
// never requires a fallible collector start (the failed start left it stopped),
// so the reconcile here cannot fail.
func (s *ApiService) rollbackTelemetry(wasActive bool, prev telemetry.TelemetryConfig) {
	if !wasActive {
		s.telemetrySession.Stop()
		s.stopTelemetryState()
		return
	}
	s.telemetrySession.UpdateConfig(prev)
	_ = s.reconcileTelemetryState(prev.Categories)
}

// reconcileExport starts or stops the OTLP export sink to match the committed
// telemetry config. It reads the desired state from the session rather than a
// parameter, and holds exportMu (not monitorMu) so the toggle-off drain does
// not block concurrent telemetry reads/writes; whichever call wins exportMu
// last observes the final committed state, so the sink converges. Callers run
// it after releasing monitorMu (via defer). Export is best-effort: a failed
// start is logged, never surfaced. No-op when no export destination is
// configured.
func (s *ApiService) reconcileExport(ctx context.Context) {
	s.exportMu.Lock()
	defer s.exportMu.Unlock()

	enabled := s.telemetrySession.Config().ExportOTLP
	if s.otlpExport == nil {
		if enabled {
			logger.FromContext(ctx).Warn("otlp export enabled but no destination configured; export inactive")
		}
		return
	}
	switch {
	case enabled && !s.otlpExport.Running():
		// Root the export on the app lifecycle, not this request; only its logs
		// carry the request context.
		if err := s.otlpExport.Start(s.lifecycleCtx); err != nil {
			logger.FromContext(ctx).Error("otlp export failed to start", "err", err)
		}
	case !enabled && s.otlpExport.Running():
		// Draining must outlive the request, so bound it independently.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), otlpStopTimeout)
		defer cancel()
		_ = s.otlpExport.Stop(stopCtx)
	}
}

// stopTelemetryState tears down the collector and middleware after a session is
// cleared. Export is reconciled separately, after monitorMu is released.
func (s *ApiService) stopTelemetryState() {
	if s.cdpMonitor.IsRunning() {
		s.cdpMonitor.Stop()
	}
	DisableTelemetryMiddleware()
}

// buildTelemetryResponse constructs a TelemetryState response from the current configuration.
func (s *ApiService) buildTelemetryResponse() oapi.TelemetryState {
	resp := oapi.TelemetryState{
		Config:        telemetryConfigToOAPI(s.telemetrySession.Config()),
		Seq:           int64(s.telemetrySession.Seq()),
		DroppedEvents: lo.ToPtr(int64(s.telemetrySession.DroppedEvents())),
	}
	if appliedAt := s.telemetrySession.AppliedAt(); !appliedAt.IsZero() {
		resp.AppliedAt = &appliedAt
	}
	return resp
}

// stoppedTelemetryResponse reports the cleared configuration. Seq and the
// dropped count are process-scoped, so they survive a session ending.
func (s *ApiService) stoppedTelemetryResponse() oapi.TelemetryState {
	return oapi.TelemetryState{
		Config:        disabledConfig(),
		Seq:           int64(s.telemetrySession.Seq()),
		DroppedEvents: lo.ToPtr(int64(s.telemetrySession.DroppedEvents())),
	}
}

// categoryField pairs a category with its enabled flag so the helpers can walk
// the configurable categories without enumerating them inline. The flag rather
// than the config, because control carries settings the others do not.
type categoryField struct {
	category oapi.TelemetryEventCategory
	enabled  *bool
}

func categoryFields(b *oapi.BrowserTelemetryCategoriesConfig) []categoryField {
	flag := func(c *oapi.BrowserTelemetryCategoryConfig) *bool {
		if c == nil {
			return nil
		}
		return c.Enabled
	}
	var control *bool
	if b.Control != nil {
		control = b.Control.Enabled
	}
	return []categoryField{
		{events.Console, flag(b.Console)},
		{events.Network, flag(b.Network)},
		{events.Page, flag(b.Page)},
		{events.Interaction, flag(b.Interaction)},
		{events.Control, control},
		{events.Platform, flag(b.Platform)},
		{events.Connection, flag(b.Connection)},
		{events.System, flag(b.System)},
		{events.Screenshot, flag(b.Screenshot)},
		{events.Captcha, flag(b.Captcha)},
	}
}

// excludedCdpMethodsFromOAPI reads the cdp_command exclusion list, which only
// the control category carries.
func excludedCdpMethodsFromOAPI(cfg *oapi.BrowserTelemetryConfig) []oapi.BrowserCdpCommandMethod {
	if cfg == nil || cfg.Browser == nil || cfg.Browser.Control == nil ||
		cfg.Browser.Control.Cdp == nil || cfg.Browser.Control.Cdp.ExcludedMethods == nil {
		return nil
	}
	return *cfg.Browser.Control.Cdp.ExcludedMethods
}

func categorySetOf(cats []oapi.TelemetryEventCategory) map[oapi.TelemetryEventCategory]bool {
	set := make(map[oapi.TelemetryEventCategory]bool, len(cats))
	for _, c := range cats {
		set[c] = true
	}
	return set
}

func containsCategory(cats []oapi.TelemetryEventCategory, target oapi.TelemetryEventCategory) bool {
	for _, c := range cats {
		if c == target {
			return true
		}
	}
	return false
}

// telemetryConfigFromOAPI converts an *oapi.BrowserTelemetryConfig to a telemetry.TelemetryConfig.
// Selection is opt-in: with no browser config the default set is used; with a browser config only
// the categories explicitly enabled there are captured (anything omitted is off). Returns the
// config, whether the result is empty (stop signal), and any error.
func telemetryConfigFromOAPI(cfg *oapi.BrowserTelemetryConfig) (telemetry.TelemetryConfig, bool, error) {
	exportOTLP := exportOTLPFromOAPI(cfg)
	if cfg == nil || cfg.Browser == nil {
		// No per-category settings: resolve to the explicit default set so the
		// effective categories are known before the collector is reconciled.
		cats := append([]oapi.TelemetryEventCategory(nil), events.DefaultCategories...)
		return telemetry.TelemetryConfig{Categories: cats, ExportOTLP: exportOTLP}, false, nil
	}

	cats := make([]oapi.TelemetryEventCategory, 0, len(events.UserCategories))
	for _, f := range categoryFields(cfg.Browser) {
		if f.enabled != nil && *f.enabled {
			cats = append(cats, f.category)
		}
	}
	if len(cats) == 0 {
		return telemetry.TelemetryConfig{}, true, nil
	}
	return telemetry.TelemetryConfig{
		Categories:         cats,
		ExportOTLP:         exportOTLP,
		ExcludedCdpMethods: excludedCdpMethodsFromOAPI(cfg),
	}, false, nil
}

// exportOTLPFromOAPI reads the OTLP export toggle from a config, defaulting to
// false (off) when the export block is omitted.
func exportOTLPFromOAPI(cfg *oapi.BrowserTelemetryConfig) bool {
	if cfg != nil && cfg.Export != nil && cfg.Export.Otlp != nil && cfg.Export.Otlp.Enabled != nil {
		return *cfg.Export.Otlp.Enabled
	}
	return false
}

// mergeTelemetryConfig applies patch overrides onto current, returning the merged config and
// whether every configurable category ended up disabled (stop signal). Only categories with an
// explicit Enabled field in patch are changed; omitted categories keep their current state.
func mergeTelemetryConfig(current telemetry.TelemetryConfig, patch *oapi.BrowserTelemetryConfig) (telemetry.TelemetryConfig, bool) {
	userCat := categorySetOf(events.UserCategories)
	active := make(map[oapi.TelemetryEventCategory]struct{}, len(current.Categories))
	for _, c := range current.Categories {
		if userCat[c] { // ignore the auto-managed Monitor category
			active[c] = struct{}{}
		}
	}

	if patch.Browser != nil {
		for _, f := range categoryFields(patch.Browser) {
			if f.enabled == nil {
				continue // not mentioned in patch; keep current state
			}
			if *f.enabled {
				active[f.category] = struct{}{}
			} else {
				delete(active, f.category)
			}
		}
	}

	// Export follows the same patch semantics: an omitted toggle is unchanged.
	exportOTLP := current.ExportOTLP
	if patch.Export != nil && patch.Export.Otlp != nil && patch.Export.Otlp.Enabled != nil {
		exportOTLP = *patch.Export.Otlp.Enabled
	}

	// So do the cdp_command exclusions: an omitted list is unchanged, an empty
	// one clears them.
	excluded := current.ExcludedCdpMethods
	if patched := excludedCdpMethodsFromOAPI(patch); patched != nil {
		excluded = patched
	}

	if len(active) == 0 {
		return telemetry.TelemetryConfig{}, true
	}
	cats := make([]oapi.TelemetryEventCategory, 0, len(active))
	for c := range active {
		cats = append(cats, c)
	}
	return telemetry.TelemetryConfig{Categories: cats, ExportOTLP: exportOTLP, ExcludedCdpMethods: excluded}, false
}

// disabledConfig returns a BrowserTelemetryConfig with every configurable category explicitly disabled.
func disabledConfig() oapi.BrowserTelemetryConfig {
	off := func() *oapi.BrowserTelemetryCategoryConfig {
		return &oapi.BrowserTelemetryCategoryConfig{Enabled: lo.ToPtr(false)}
	}
	return oapi.BrowserTelemetryConfig{
		Browser: &oapi.BrowserTelemetryCategoriesConfig{
			Console:     off(),
			Network:     off(),
			Page:        off(),
			Interaction: off(),
			Control:     &oapi.BrowserTelemetryControlConfig{Enabled: lo.ToPtr(false)},
			Platform:    off(),
			Connection:  off(),
			System:      off(),
			Screenshot:  off(),
			Captcha:     off(),
		},
		Export: exportConfigToOAPI(false),
	}
}

// exportConfigToOAPI renders the OTLP export toggle for API responses.
func exportConfigToOAPI(enabled bool) *oapi.BrowserTelemetryExportConfig {
	return &oapi.BrowserTelemetryExportConfig{
		Otlp: &oapi.BrowserTelemetryOTLPExportConfig{Enabled: lo.ToPtr(enabled)},
	}
}

// telemetryConfigToOAPI converts a telemetry.TelemetryConfig to an oapi.BrowserTelemetryConfig
// suitable for API responses. The auto-managed Monitor category is not represented.
func telemetryConfigToOAPI(cfg telemetry.TelemetryConfig) oapi.BrowserTelemetryConfig {
	active := categorySetOf(cfg.Categories)
	enabled := func(cat oapi.TelemetryEventCategory) *oapi.BrowserTelemetryCategoryConfig {
		on := active[cat]
		return &oapi.BrowserTelemetryCategoryConfig{Enabled: &on}
	}
	control := &oapi.BrowserTelemetryControlConfig{Enabled: lo.ToPtr(active[events.Control])}
	if len(cfg.ExcludedCdpMethods) > 0 {
		control.Cdp = &oapi.BrowserTelemetryCdpControlConfig{ExcludedMethods: &cfg.ExcludedCdpMethods}
	}
	return oapi.BrowserTelemetryConfig{
		Browser: &oapi.BrowserTelemetryCategoriesConfig{
			Console:     enabled(events.Console),
			Network:     enabled(events.Network),
			Page:        enabled(events.Page),
			Interaction: enabled(events.Interaction),
			Control:     control,
			Platform:    enabled(events.Platform),
			Connection:  enabled(events.Connection),
			System:      enabled(events.System),
			Screenshot:  enabled(events.Screenshot),
			Captcha:     enabled(events.Captcha),
		},
		Export: exportConfigToOAPI(cfg.ExportOTLP),
	}
}
