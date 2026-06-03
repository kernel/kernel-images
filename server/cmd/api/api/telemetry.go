package api

import (
	"context"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/nrednav/cuid2"
	"github.com/samber/lo"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/telemetry"
)

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
		return oapi.PutTelemetry200JSONResponse(oapi.TelemetryState{Config: disabledConfig(), Seq: int64(s.telemetrySession.Seq())}), nil
	}

	// Reconcile the collector before committing the config so a startup failure
	// leaves no partial state: the session keeps its prior config on error.
	if err := s.reconcileTelemetryState(cfg.Categories); err != nil {
		logger.FromContext(ctx).Error("failed to apply telemetry state", "err", err)
		return oapi.PutTelemetry500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start telemetry"}}, nil
	}

	if wasActive {
		s.telemetrySession.UpdateConfig(cfg)
		return oapi.PutTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
	}
	s.telemetrySession.Start(cuid2.Generate(), cfg)
	return oapi.PutTelemetry201JSONResponse(s.buildTelemetryResponse()), nil
}

// PatchTelemetry handles PATCH /telemetry.
// Partially updates the telemetry configuration. Returns 404 if not configured.
// Setting every configurable category to enabled:false clears the configuration (200).
func (s *ApiService) PatchTelemetry(ctx context.Context, req oapi.PatchTelemetryRequestObject) (oapi.PatchTelemetryResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if !s.telemetrySession.Active() {
		return oapi.PatchTelemetry404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "telemetry is not configured"}}, nil
	}

	if req.Body == nil || req.Body.Browser == nil {
		return oapi.PatchTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
	}

	cfg, allDisabled := mergeTelemetryConfig(s.telemetrySession.Config(), req.Body.Browser)
	if allDisabled {
		s.telemetrySession.Stop()
		s.stopTelemetryState()
		return oapi.PatchTelemetry200JSONResponse(oapi.TelemetryState{Config: disabledConfig(), Seq: int64(s.telemetrySession.Seq())}), nil
	}

	// Reconcile before committing so a collector startup failure leaves the
	// session on its prior config rather than half-applied.
	if err := s.reconcileTelemetryState(cfg.Categories); err != nil {
		logger.FromContext(ctx).Error("failed to apply telemetry state", "err", err)
		return oapi.PatchTelemetry500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to apply telemetry"}}, nil
	}
	s.telemetrySession.UpdateConfig(cfg)
	return oapi.PatchTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
}

// reconcileTelemetryState reconciles the CDP collector and the api_call
// (control) middleware to the desired category set. The collector runs iff a
// CDP category is captured; the middleware emits iff the control category is.
// It returns an error only when the collector fails to start, and performs the
// fallible collector start before any other state change so callers can invoke
// it before committing the config and leave nothing half-applied on failure.
func (s *ApiService) reconcileTelemetryState(cats []oapi.TelemetryEventCategory) error {
	switch {
	case events.HasCDPCategory(cats) && !s.cdpMonitor.IsRunning():
		if err := s.cdpMonitor.Start(s.lifecycleCtx); err != nil {
			return err
		}
	case !events.HasCDPCategory(cats) && s.cdpMonitor.IsRunning():
		s.cdpMonitor.Stop()
	}

	if containsCategory(cats, events.Control) {
		EnableTelemetryMiddleware()
	} else {
		DisableTelemetryMiddleware()
	}
	return nil
}

// stopTelemetryState tears down the collector and middleware after a session is
// cleared.
func (s *ApiService) stopTelemetryState() {
	if s.cdpMonitor.IsRunning() {
		s.cdpMonitor.Stop()
	}
	DisableTelemetryMiddleware()
}

// buildTelemetryResponse constructs a TelemetryState response from the current configuration.
func (s *ApiService) buildTelemetryResponse() oapi.TelemetryState {
	resp := oapi.TelemetryState{
		Config: telemetryConfigToOAPI(s.telemetrySession.Config()),
		Seq:    int64(s.telemetrySession.Seq()),
	}
	if appliedAt := s.telemetrySession.AppliedAt(); !appliedAt.IsZero() {
		resp.AppliedAt = &appliedAt
	}
	return resp
}

// categoryField pairs a category with its config field so the helpers can walk
// the configurable categories without enumerating them inline.
type categoryField struct {
	category oapi.TelemetryEventCategory
	config   *oapi.BrowserTelemetryCategoryConfig
}

func categoryFields(b *oapi.BrowserTelemetryCategoriesConfig) []categoryField {
	return []categoryField{
		{events.Console, b.Console},
		{events.Network, b.Network},
		{events.Page, b.Page},
		{events.Interaction, b.Interaction},
		{events.Control, b.Control},
		{events.Connection, b.Connection},
		{events.System, b.System},
		{events.Screenshot, b.Screenshot},
		{events.Captcha, b.Captcha},
	}
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
// An omitted category resolves to its default state (events.DefaultCategories). Returns the
// config, whether every configurable category ended up disabled (stop signal), and any error.
func telemetryConfigFromOAPI(cfg *oapi.BrowserTelemetryConfig) (telemetry.TelemetryConfig, bool, error) {
	if cfg == nil || cfg.Browser == nil {
		// No per-category settings: resolve to the explicit default set so the
		// effective categories are known before the collector is reconciled.
		cats := append([]oapi.TelemetryEventCategory(nil), events.DefaultCategories...)
		return telemetry.TelemetryConfig{Categories: cats}, false, nil
	}

	defaultOn := categorySetOf(events.DefaultCategories)
	cats := make([]oapi.TelemetryEventCategory, 0, len(events.UserCategories))
	for _, f := range categoryFields(cfg.Browser) {
		on := defaultOn[f.category]
		if f.config != nil && f.config.Enabled != nil {
			on = *f.config.Enabled
		}
		if on {
			cats = append(cats, f.category)
		}
	}
	if len(cats) == 0 {
		return telemetry.TelemetryConfig{}, true, nil
	}
	return telemetry.TelemetryConfig{Categories: cats}, false, nil
}

// mergeTelemetryConfig applies patch overrides onto current, returning the merged config and
// whether every configurable category ended up disabled (stop signal). Only categories with an
// explicit Enabled field in patch are changed; omitted categories keep their current state.
func mergeTelemetryConfig(current telemetry.TelemetryConfig, patch *oapi.BrowserTelemetryCategoriesConfig) (telemetry.TelemetryConfig, bool) {
	userCat := categorySetOf(events.UserCategories)
	active := make(map[oapi.TelemetryEventCategory]struct{}, len(current.Categories))
	for _, c := range current.Categories {
		if userCat[c] { // ignore the auto-managed Monitor category
			active[c] = struct{}{}
		}
	}

	for _, f := range categoryFields(patch) {
		if f.config == nil || f.config.Enabled == nil {
			continue // not mentioned in patch — keep current state
		}
		if *f.config.Enabled {
			active[f.category] = struct{}{}
		} else {
			delete(active, f.category)
		}
	}

	if len(active) == 0 {
		return telemetry.TelemetryConfig{}, true
	}
	cats := make([]oapi.TelemetryEventCategory, 0, len(active))
	for c := range active {
		cats = append(cats, c)
	}
	return telemetry.TelemetryConfig{Categories: cats}, false
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
			Control:     off(),
			Connection:  off(),
			System:      off(),
			Screenshot:  off(),
			Captcha:     off(),
		},
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
	return oapi.BrowserTelemetryConfig{
		Browser: &oapi.BrowserTelemetryCategoriesConfig{
			Console:     enabled(events.Console),
			Network:     enabled(events.Network),
			Page:        enabled(events.Page),
			Interaction: enabled(events.Interaction),
			Control:     enabled(events.Control),
			Connection:  enabled(events.Connection),
			System:      enabled(events.System),
			Screenshot:  enabled(events.Screenshot),
			Captcha:     enabled(events.Captcha),
		},
	}
}
