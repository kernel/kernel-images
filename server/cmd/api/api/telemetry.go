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
// Setting all four categories to enabled:false clears the configuration (200).
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
		return oapi.PutTelemetry200JSONResponse(oapi.TelemetryState{Config: disabledConfig(), Seq: int64(s.telemetrySession.Seq())}), nil
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
// Partially updates the telemetry configuration. Returns 404 if not configured.
// Setting all four categories to enabled:false clears the configuration (200).
// Browser categories merge per-key; attributes is a whole-map field that
// replaces current when present and is a no-op when omitted.
func (s *ApiService) PatchTelemetry(_ context.Context, req oapi.PatchTelemetryRequestObject) (oapi.PatchTelemetryResponseObject, error) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()

	if !s.telemetrySession.Active() {
		return oapi.PatchTelemetry404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "telemetry is not configured"}}, nil
	}

	if req.Body == nil {
		return oapi.PatchTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
	}

	current := s.telemetrySession.Config()
	cfg, allDisabled := mergeTelemetryConfig(current, req.Body)
	if allDisabled {
		// All categories disabled: clear the configuration.
		s.cdpMonitor.Stop()
		s.telemetrySession.Stop()
		return oapi.PatchTelemetry200JSONResponse(oapi.TelemetryState{Config: disabledConfig(), Seq: int64(s.telemetrySession.Seq())}), nil
	}
	s.telemetrySession.UpdateConfig(cfg)

	return oapi.PatchTelemetry200JSONResponse(s.buildTelemetryResponse()), nil
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
	if cfg == nil {
		// No config provided: capture all categories, no attributes.
		return telemetry.TelemetryConfig{}, false, nil
	}

	attrs := attributesFromOAPI(cfg.Attributes)

	if cfg.Browser == nil {
		return telemetry.TelemetryConfig{Attributes: attrs}, false, nil
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

	cats := make([]oapi.TelemetryEventCategory, 0, 5)
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
	// CategorySystem is always appended by TelemetrySession.Start/UpdateConfig;
	// no need to include it here.
	return telemetry.TelemetryConfig{Categories: cats, Attributes: attrs}, false, nil
}

// mergeTelemetryConfig applies a PATCH body onto current. Categories merge
// per-key; attributes is whole-map (replace when non-nil, preserve when nil).
// Returns the merged config and an allDisabled stop signal indicating the
// caller should tear down the session.
func mergeTelemetryConfig(current telemetry.TelemetryConfig, patch *oapi.BrowserTelemetryConfig) (telemetry.TelemetryConfig, bool) {
	active := make(map[oapi.TelemetryEventCategory]struct{}, len(current.Categories))
	for _, c := range current.Categories {
		if c != events.System { // system is managed internally by TelemetrySession
			active[c] = struct{}{}
		}
	}

	if patch.Browser != nil {
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

		override(events.Console, patch.Browser.Console)
		override(events.Network, patch.Browser.Network)
		override(events.Page, patch.Browser.Page)
		override(events.Interaction, patch.Browser.Interaction)
	}

	// CategorySystem is managed internally by TelemetrySession; exclude from the
	// user-facing allDisabled check.
	userCats := []oapi.TelemetryEventCategory{
		events.Console,
		events.Network,
		events.Page,
		events.Interaction,
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

	attrs := current.Attributes
	if patch.Attributes != nil {
		attrs = attributesFromOAPI(patch.Attributes)
	}
	return telemetry.TelemetryConfig{Categories: cats, Attributes: attrs}, false
}

// disabledConfig returns a BrowserTelemetryConfig with all four user-facing categories explicitly disabled.
func disabledConfig() oapi.BrowserTelemetryConfig {
	f := false
	cat := &oapi.BrowserTelemetryCategoryConfig{Enabled: &f}
	return oapi.BrowserTelemetryConfig{
		Browser: &oapi.BrowserTelemetryCategoriesConfig{
			Console:     cat,
			Network:     cat,
			Page:        cat,
			Interaction: cat,
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
		},
		Attributes: attributesToOAPI(cfg.Attributes),
	}
}

// attributesFromOAPI returns a defensive copy of in, or nil for nil/empty.
func attributesFromOAPI(in *map[string]string) map[string]string {
	if in == nil || len(*in) == 0 {
		return nil
	}
	out := make(map[string]string, len(*in))
	for k, v := range *in {
		out[k] = v
	}
	return out
}

// attributesToOAPI converts in to the OAPI pointer shape, returning nil
// when empty so the field is omitted from JSON responses.
func attributesToOAPI(in map[string]string) *map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return &out
}

