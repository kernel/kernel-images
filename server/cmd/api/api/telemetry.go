package api

import (
	"context"

	"github.com/nrednav/cuid2"
	oapi "github.com/kernel/kernel-images/server/lib/oapi"

	"github.com/kernel/kernel-images/server/lib/events"
	"github.com/kernel/kernel-images/server/lib/logger"
	"github.com/kernel/kernel-images/server/lib/telemetry"
)

// GetTelemetry handles GET /telemetry.
// Returns the current telemetry state. Returns 404 if telemetry is not active.
func (s *ApiService) GetTelemetry(_ context.Context, _ oapi.GetTelemetryRequestObject) (oapi.GetTelemetryResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if !s.telemetrySession.Active() {
		return oapi.GetTelemetry404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "telemetry is not active"}}, nil
	}
	return oapi.GetTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
}

// PutTelemetry handles PUT /telemetry.
// Creates (201) or replaces (200) the telemetry session with the given config.
// Setting all four categories to enabled:false stops an active session (200, status stopped).
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
			return oapi.PutTelemetry400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "cannot start telemetry with all categories disabled"}}, nil
		}
		// All categories disabled: stop the running session.
		s.cdpMonitor.Stop()
		resp := s.buildTelemetryResponse()
		resp.Status = oapi.TelemetryStateStatusStopped
		s.telemetrySession.Stop()
		return oapi.PutTelemetry200JSONResponse(resp), nil
	}

	if wasActive {
		// Replace config on the running session.
		s.telemetrySession.UpdateConfig(cfg)
		return oapi.PutTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
	}

	// Start a new telemetry session.
	id := cuid2.Generate()
	s.telemetrySession.Start(id, cfg)

	if err := s.cdpMonitor.Start(s.lifecycleCtx); err != nil {
		// Roll back: clear the session so a retry can succeed.
		s.telemetrySession.Stop()
		logger.FromContext(ctx).Error("failed to start telemetry monitor", "err", err)
		return oapi.PutTelemetry500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start telemetry"}}, nil
	}

	return oapi.PutTelemetry201JSONResponse(s.buildTelemetryResponse()), nil
}

// PatchTelemetry handles PATCH /telemetry.
// Updates the configuration of the active telemetry session. Returns 404 if not active.
// Setting all four categories to enabled:false stops the session (200, status stopped).
func (s *ApiService) PatchTelemetry(_ context.Context, req oapi.PatchTelemetryRequestObject) (oapi.PatchTelemetryResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if !s.telemetrySession.Active() {
		return oapi.PatchTelemetry404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "telemetry is not active"}}, nil
	}

	if req.Body != nil && req.Body.Browser != nil {
		// PATCH merges: only categories explicitly set in the request are updated;
		// omitted categories retain their current enabled/disabled state.
		current := s.telemetrySession.Config()
		cfg, allDisabled := mergeTelemetryConfig(current, req.Body.Browser)
		if allDisabled {
			// All categories disabled: stop the session.
			s.cdpMonitor.Stop()
			// Snapshot the final state before clearing the session ID so buildTelemetryResponse
			// can still read it. Force status to stopped because cdpMonitor.Stop may
			// tear down asynchronously, leaving IsRunning briefly true.
			resp := s.buildTelemetryResponse()
			resp.Status = oapi.TelemetryStateStatusStopped
			s.telemetrySession.Stop()
			return oapi.PatchTelemetry200JSONResponse(resp), nil
		}
		s.telemetrySession.UpdateConfig(cfg)
	}

	return oapi.PatchTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
}

// buildTelemetryResponse constructs a TelemetryState response from the current session state.
func (s *ApiService) buildTelemetryResponse() oapi.TelemetryState {
	cfg := s.telemetrySession.Config()

	status := oapi.TelemetryStateStatusStopped
	if s.cdpMonitor.IsRunning() {
		status = oapi.TelemetryStateStatusRunning
	}

	return oapi.TelemetryState{
		Id:        s.telemetrySession.ID(),
		Status:    status,
		Config:    telemetryConfigToOAPI(cfg),
		Seq:       int64(s.telemetrySession.Seq()),
		CreatedAt: s.telemetrySession.CreatedAt(),
	}
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

	allDisabled := !consoleOn && !networkOn && !pageOn && !interactionOn
	if allDisabled {
		return telemetry.TelemetryConfig{}, true, nil
	}

	cats := make([]events.EventCategory, 0, 5)
	if consoleOn {
		cats = append(cats, events.CategoryConsole)
	}
	if networkOn {
		cats = append(cats, events.CategoryNetwork)
	}
	if pageOn {
		cats = append(cats, events.CategoryPage)
	}
	if interactionOn {
		cats = append(cats, events.CategoryInteraction)
	}
	// CategorySystem is always appended by TelemetrySession.Start/UpdateConfig;
	// no need to include it here.
	return telemetry.TelemetryConfig{Categories: cats}, false, nil
}

// mergeTelemetryConfig applies patch overrides onto current, returning the merged config and
// whether all user-facing categories ended up disabled (stop signal). Only categories with an
// explicit Enabled field in patch are changed; omitted categories keep their current state.
func mergeTelemetryConfig(current telemetry.TelemetryConfig, patch *oapi.BrowserTelemetryCategoriesConfig) (telemetry.TelemetryConfig, bool) {
	active := make(map[events.EventCategory]struct{}, len(current.Categories))
	for _, c := range current.Categories {
		if c != events.CategorySystem { // system is managed internally by TelemetrySession
			active[c] = struct{}{}
		}
	}

	override := func(cat events.EventCategory, field *oapi.BrowserTelemetryCategoryConfig) {
		if field == nil || field.Enabled == nil {
			return // not mentioned in patch — keep current state
		}
		if *field.Enabled {
			active[cat] = struct{}{}
		} else {
			delete(active, cat)
		}
	}

	override(events.CategoryConsole, patch.Console)
	override(events.CategoryNetwork, patch.Network)
	override(events.CategoryPage, patch.Page)
	override(events.CategoryInteraction, patch.Interaction)

	// CategorySystem is managed internally by TelemetrySession; exclude from the
	// user-facing allDisabled check.
	userCats := []events.EventCategory{
		events.CategoryConsole,
		events.CategoryNetwork,
		events.CategoryPage,
		events.CategoryInteraction,
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

	cats := make([]events.EventCategory, 0, len(active))
	for c := range active {
		cats = append(cats, c)
	}
	return telemetry.TelemetryConfig{Categories: cats}, false
}

// telemetryConfigToOAPI converts a telemetry.TelemetryConfig to an oapi.BrowserTelemetryConfig
// suitable for API responses.
func telemetryConfigToOAPI(cfg telemetry.TelemetryConfig) oapi.BrowserTelemetryConfig {
	// Build a set of active categories for O(1) lookup.
	active := make(map[events.EventCategory]struct{}, len(cfg.Categories))
	for _, c := range cfg.Categories {
		active[c] = struct{}{}
	}

	enabled := func(cat events.EventCategory) *oapi.BrowserTelemetryCategoryConfig {
		_, on := active[cat]
		return &oapi.BrowserTelemetryCategoryConfig{Enabled: &on}
	}

	return oapi.BrowserTelemetryConfig{
		Browser: &oapi.BrowserTelemetryCategoriesConfig{
			Console:     enabled(events.CategoryConsole),
			Network:     enabled(events.CategoryNetwork),
			Page:        enabled(events.CategoryPage),
			Interaction: enabled(events.CategoryInteraction),
		},
	}
}

