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
// Setting all five categories to enabled:false clears the configuration (200).
func (s *ApiService) PutTelemetry(ctx context.Context, req oapi.PutTelemetryRequestObject) (oapi.PutTelemetryResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	cfg, allDisabled, err := telemetryConfigFromOAPI(req.Body)
	if err != nil {
		return oapi.PutTelemetry400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: err.Error()}}, nil
	}

	wasActive := s.telemetrySession.Active()

	if allDisabled {
		if !wasActive {
			// Already cleared; all-disabled is idempotent.
			return oapi.PutTelemetry200JSONResponse(oapi.TelemetryState{Config: disabledConfig(), Seq: int64(s.telemetrySession.Seq())}), nil
		}
		// All categories disabled: clear the configuration.
		s.cdpMonitor.Stop()
		s.telemetrySession.Stop()
		s.applyTelemetryMiddlewareState()
		return oapi.PutTelemetry200JSONResponse(oapi.TelemetryState{Config: disabledConfig(), Seq: int64(s.telemetrySession.Seq())}), nil
	}

	if wasActive {
		// Replace config on the running session.
		s.telemetrySession.UpdateConfig(cfg)
		s.applyTelemetryMiddlewareState()
		return oapi.PutTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
	}

	// Start a new telemetry session.
	id := cuid2.Generate()
	s.telemetrySession.Start(id, cfg)

	if err := s.cdpMonitor.Start(s.lifecycleCtx); err != nil {
		// Roll back: clear the session so a retry can succeed.
		s.telemetrySession.Stop()
		s.applyTelemetryMiddlewareState()
		logger.FromContext(ctx).Error("failed to start telemetry monitor", "err", err)
		return oapi.PutTelemetry500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start telemetry"}}, nil
	}

	s.applyTelemetryMiddlewareState()
	return oapi.PutTelemetry201JSONResponse(s.buildTelemetryResponse()), nil
}

// PatchTelemetry handles PATCH /telemetry.
// Partially updates the telemetry configuration. Returns 404 if not configured.
// Setting all five categories to enabled:false clears the configuration (200).
func (s *ApiService) PatchTelemetry(_ context.Context, req oapi.PatchTelemetryRequestObject) (oapi.PatchTelemetryResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if !s.telemetrySession.Active() {
		return oapi.PatchTelemetry404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "telemetry is not configured"}}, nil
	}

	if req.Body != nil && req.Body.Browser != nil {
		// PATCH merges: only categories explicitly set in the request are updated;
		// omitted categories retain their current enabled/disabled state.
		current := s.telemetrySession.Config()
		cfg, allDisabled := mergeTelemetryConfig(current, req.Body.Browser)
		if allDisabled {
			// All categories disabled: clear the configuration.
			s.cdpMonitor.Stop()
			s.telemetrySession.Stop()
			s.applyTelemetryMiddlewareState()
			return oapi.PatchTelemetry200JSONResponse(oapi.TelemetryState{Config: disabledConfig(), Seq: int64(s.telemetrySession.Seq())}), nil
		}
		s.telemetrySession.UpdateConfig(cfg)
		s.applyTelemetryMiddlewareState()
	}

	return oapi.PatchTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
}

// applyTelemetryMiddlewareState turns the api_call middleware on iff the
// session is active and the api category is enabled. Call after any config
// change.
func (s *ApiService) applyTelemetryMiddlewareState() {
	if !s.telemetrySession.Active() {
		DisableTelemetryMiddleware()
		return
	}
	for _, c := range s.telemetrySession.Config().Categories {
		if c == events.Api {
			EnableTelemetryMiddleware()
			return
		}
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

// telemetryConfigFromOAPI converts an *oapi.BrowserTelemetryConfig to a telemetry.TelemetryConfig.
// Returns the config, a boolean indicating whether all user-facing categories are explicitly
// disabled (stop signal), and any validation error.
func telemetryConfigFromOAPI(cfg *oapi.BrowserTelemetryConfig) (telemetry.TelemetryConfig, bool, error) {
	if cfg == nil || cfg.Browser == nil {
		// No config provided: capture all categories.
		return telemetry.TelemetryConfig{}, false, nil
	}

	b := cfg.Browser
	// A nil or omitted Enabled field defaults to true (capture the category).
	isEnabled := func(c *oapi.BrowserTelemetryCategoryConfig) bool {
		return c == nil || c.Enabled == nil || *c.Enabled
	}

	consoleOn := isEnabled(b.Console)
	networkOn := isEnabled(b.Network)
	pageOn := isEnabled(b.Page)
	interactionOn := isEnabled(b.Interaction)
	apiOn := isEnabled(b.Api)

	allDisabled := !consoleOn && !networkOn && !pageOn && !interactionOn && !apiOn
	if allDisabled {
		return telemetry.TelemetryConfig{}, true, nil
	}

	cats := make([]oapi.TelemetryEventCategory, 0, 6)
	if consoleOn {
		cats = append(cats, events.Console)
	}
	if networkOn {
		cats = append(cats, events.Network)
	}
	if pageOn {
		cats = append(cats, events.Page)
	}
	if interactionOn {
		cats = append(cats, events.Interaction)
	}
	if apiOn {
		cats = append(cats, events.Api)
	}
	// CategorySystem is always appended by TelemetrySession.Start/UpdateConfig;
	// no need to include it here.
	return telemetry.TelemetryConfig{Categories: cats}, false, nil
}

// mergeTelemetryConfig applies patch overrides onto current, returning the merged config and
// whether all user-facing categories ended up disabled (stop signal). Only categories with an
// explicit Enabled field in patch are changed; omitted categories keep their current state.
func mergeTelemetryConfig(current telemetry.TelemetryConfig, patch *oapi.BrowserTelemetryCategoriesConfig) (telemetry.TelemetryConfig, bool) {
	active := make(map[oapi.TelemetryEventCategory]struct{}, len(current.Categories))
	for _, c := range current.Categories {
		if c != events.System { // system is managed internally by TelemetrySession
			active[c] = struct{}{}
		}
	}

	override := func(cat oapi.TelemetryEventCategory, field *oapi.BrowserTelemetryCategoryConfig) {
		if field == nil || field.Enabled == nil {
			return // not mentioned in patch — keep current state
		}
		if *field.Enabled {
			active[cat] = struct{}{}
		} else {
			delete(active, cat)
		}
	}

	override(events.Console, patch.Console)
	override(events.Network, patch.Network)
	override(events.Page, patch.Page)
	override(events.Interaction, patch.Interaction)
	override(events.Api, patch.Api)

	// CategorySystem is managed internally by TelemetrySession; exclude from the
	// user-facing allDisabled check.
	userCats := []oapi.TelemetryEventCategory{
		events.Console,
		events.Network,
		events.Page,
		events.Interaction,
		events.Api,
	}
	allDisabled := true
	for _, c := range userCats {
		if _, ok := active[c]; ok {
			allDisabled = false
			break
		}
	}
	if allDisabled {
		return telemetry.TelemetryConfig{}, true
	}

	cats := make([]oapi.TelemetryEventCategory, 0, len(active))
	for c := range active {
		cats = append(cats, c)
	}
	return telemetry.TelemetryConfig{Categories: cats}, false
}

// disabledConfig returns a BrowserTelemetryConfig with all five user-facing categories explicitly disabled.
func disabledConfig() oapi.BrowserTelemetryConfig {
	return oapi.BrowserTelemetryConfig{
		Browser: &oapi.BrowserTelemetryCategoriesConfig{
			Console:     &oapi.BrowserTelemetryCategoryConfig{Enabled: lo.ToPtr(false)},
			Network:     &oapi.BrowserTelemetryCategoryConfig{Enabled: lo.ToPtr(false)},
			Page:        &oapi.BrowserTelemetryCategoryConfig{Enabled: lo.ToPtr(false)},
			Interaction: &oapi.BrowserTelemetryCategoryConfig{Enabled: lo.ToPtr(false)},
			Api:         &oapi.BrowserTelemetryCategoryConfig{Enabled: lo.ToPtr(false)},
		},
	}
}

// telemetryConfigToOAPI converts a telemetry.TelemetryConfig to an oapi.BrowserTelemetryConfig
// suitable for API responses.
func telemetryConfigToOAPI(cfg telemetry.TelemetryConfig) oapi.BrowserTelemetryConfig {
	// Build a set of active categories for O(1) lookup.
	active := make(map[oapi.TelemetryEventCategory]struct{}, len(cfg.Categories))
	for _, c := range cfg.Categories {
		active[c] = struct{}{}
	}

	enabled := func(cat oapi.TelemetryEventCategory) *oapi.BrowserTelemetryCategoryConfig {
		_, on := active[cat]
		return &oapi.BrowserTelemetryCategoryConfig{Enabled: &on}
	}

	return oapi.BrowserTelemetryConfig{
		Browser: &oapi.BrowserTelemetryCategoriesConfig{
			Console:     enabled(events.Console),
			Network:     enabled(events.Network),
			Page:        enabled(events.Page),
			Interaction: enabled(events.Interaction),
			Api:         enabled(events.Api),
		},
	}
}
