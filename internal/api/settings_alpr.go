package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/settings"
)

// ALPRDisclaimerCurrentVersion is the disclaimer revision the operator must
// have acknowledged before ALPR can be enabled. The alpr-disclaimer-gate
// feature owns the actual ack flow; until that lands, this value is the
// constant returned by GET and the precondition checked by PUT (which means
// enable=true will fail with missing_prerequisites: ["disclaimer"] until
// the gate feature is implemented -- intentionally).
const ALPRDisclaimerCurrentVersion = "1.0"

// alprEngineReachabilityTTL is how long a /health probe result is cached
// before the next call re-probes. The status endpoint is cheap enough that
// dashboard polling at a higher rate would otherwise hammer the engine.
const alprEngineReachabilityTTL = 30 * time.Second

// alprEngineProbeTimeout caps a single /health request; slow engines should
// surface as engine_reachable=false rather than blocking the status response.
const alprEngineProbeTimeout = 1500 * time.Millisecond

// alprBoundsFPSMin/Max bound the runtime override for frames-per-second.
// Outside this range the extractor either misses plates (too slow) or
// thrashes the engine (too fast).
const (
	alprBoundsFPSMin        = 0.5
	alprBoundsFPSMax        = 4.0
	alprBoundsConfidenceMin = 0.5
	alprBoundsConfidenceMax = 0.95
)

// engineReachability is a small cached probe of <ALPR_ENGINE_URL>/health.
// The mutex guards both the cached result and the in-flight probe so
// concurrent dashboard polls coalesce into a single outbound request.
type engineReachability struct {
	mu       sync.Mutex
	last     time.Time
	reachOK  bool
	endpoint string
	client   *http.Client
}

func newEngineReachability(engineURL string) *engineReachability {
	return &engineReachability{
		endpoint: engineURL,
		client:   &http.Client{Timeout: alprEngineProbeTimeout},
	}
}

// Check returns whether the engine /health endpoint responded with a 2xx
// status within the last alprEngineReachabilityTTL. It blocks for at most
// alprEngineProbeTimeout when the cached result is stale.
func (e *engineReachability) Check(ctx context.Context) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.last.IsZero() && time.Since(e.last) < alprEngineReachabilityTTL {
		return e.reachOK
	}
	if e.endpoint == "" {
		e.last = time.Now()
		e.reachOK = false
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, alprEngineProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, e.endpoint+"/health", nil)
	if err != nil {
		e.last = time.Now()
		e.reachOK = false
		return false
	}
	resp, err := e.client.Do(req)
	e.last = time.Now()
	if err != nil {
		e.reachOK = false
		return false
	}
	defer resp.Body.Close()
	e.reachOK = resp.StatusCode >= 200 && resp.StatusCode < 300
	return e.reachOK
}

// ALPRSettingsHandler exposes GET/PUT /v1/settings/alpr. It bridges the
// deployment-time ALPRConfig (env vars) with the runtime overrides stored
// in the settings table, and returns a status response that includes
// derived fields (encryption_key_configured, engine_reachable, disclaimer
// state).
type ALPRSettingsHandler struct {
	store *settings.Store
	cfg   *config.ALPRConfig
	probe *engineReachability
}

// NewALPRSettingsHandler wires the given settings.Store with the ALPRConfig
// loaded from env. cfg may be nil in tests that do not need an engine probe.
func NewALPRSettingsHandler(store *settings.Store, cfg *config.ALPRConfig) *ALPRSettingsHandler {
	engineURL := ""
	if cfg != nil {
		engineURL = cfg.EngineURL
	}
	return &ALPRSettingsHandler{
		store: store,
		cfg:   cfg,
		probe: newEngineReachability(engineURL),
	}
}

// alprSettingsResponse is the JSON body returned from GET /v1/settings/alpr.
//
// disclaimer_acked_at is a *string so a missing ack serializes as JSON null
// rather than an empty string. The encryption key is intentionally never
// exposed -- only the boolean encryption_key_configured.
type alprSettingsResponse struct {
	Enabled                 bool    `json:"enabled"`
	EngineURL               string  `json:"engine_url"`
	Region                  string  `json:"region"`
	FramesPerSecond         float64 `json:"frames_per_second"`
	ConfidenceMin           float64 `json:"confidence_min"`
	RetentionDaysUnflagged  int     `json:"retention_days_unflagged"`
	RetentionDaysFlagged    int     `json:"retention_days_flagged"`
	NotifyMinSeverity       int     `json:"notify_min_severity"`
	EncryptionKeyConfigured bool    `json:"encryption_key_configured"`
	EngineReachable         bool    `json:"engine_reachable"`
	DisclaimerRequired      bool    `json:"disclaimer_required"`
	DisclaimerVersion       string  `json:"disclaimer_version"`
	DisclaimerAckedAt       *string `json:"disclaimer_acked_at"`
}

// alprSettingsRequest is the JSON body accepted by PUT /v1/settings/alpr.
// Pointer fields let the handler distinguish "field omitted" from a deliberate
// zero/empty value. Computed/server-side fields (encryption_key_configured,
// engine_reachable, disclaimer_*) are not accepted in the body.
type alprSettingsRequest struct {
	Enabled                *bool    `json:"enabled,omitempty"`
	Region                 *string  `json:"region,omitempty"`
	FramesPerSecond        *float64 `json:"frames_per_second,omitempty"`
	ConfidenceMin          *float64 `json:"confidence_min,omitempty"`
	RetentionDaysUnflagged *int     `json:"retention_days_unflagged,omitempty"`
	RetentionDaysFlagged   *int     `json:"retention_days_flagged,omitempty"`
	NotifyMinSeverity      *int     `json:"notify_min_severity,omitempty"`
}

// preconditionResponse is the body returned for 412 Precondition Failed
// when an enable=true call is missing prerequisites. The list is stable
// for clients to map to UI hints (e.g. "Configure key before enabling").
type preconditionResponse struct {
	Error                string   `json:"error"`
	Code                 int      `json:"code"`
	MissingPrerequisites []string `json:"missing_prerequisites"`
}

// validRegions is the closed set of region values accepted by both env and
// runtime overrides. Anything else is rejected so the engine never sees
// garbage.
var validRegions = map[string]struct{}{
	"us":    {},
	"eu":    {},
	"uk":    {},
	"other": {},
}

// effectiveSettings collapses env defaults and stored overrides into the
// values that should be returned by GET. It does NOT touch the
// engine-reachability probe; callers run that separately.
func (h *ALPRSettingsHandler) effectiveSettings(ctx context.Context) (alprSettingsResponse, error) {
	cfg := h.cfg
	if cfg == nil {
		cfg = &config.ALPRConfig{}
	}
	resp := alprSettingsResponse{
		EngineURL:               cfg.EngineURL,
		EncryptionKeyConfigured: cfg.EncryptionKeyConfigured(),
		DisclaimerRequired:      true,
		DisclaimerVersion:       ALPRDisclaimerCurrentVersion,
	}

	enabled, err := h.store.BoolOr(ctx, settings.KeyALPREnabled, false)
	if err != nil {
		return resp, err
	}
	resp.Enabled = enabled

	region, err := h.store.StringOr(ctx, settings.KeyALPRRegion, cfg.Region)
	if err != nil {
		return resp, err
	}
	resp.Region = region

	fps, err := h.store.FloatOr(ctx, settings.KeyALPRFramesPerSecond, cfg.FramesPerSecond)
	if err != nil {
		return resp, err
	}
	resp.FramesPerSecond = fps

	conf, err := h.store.FloatOr(ctx, settings.KeyALPRConfidenceMin, cfg.ConfidenceMin)
	if err != nil {
		return resp, err
	}
	resp.ConfidenceMin = conf

	rdu, err := h.store.IntOr(ctx, settings.KeyALPRRetentionDaysUnflagged, cfg.RetentionDaysUnflagged)
	if err != nil {
		return resp, err
	}
	resp.RetentionDaysUnflagged = rdu

	rdf, err := h.store.IntOr(ctx, settings.KeyALPRRetentionDaysFlagged, cfg.RetentionDaysFlagged)
	if err != nil {
		return resp, err
	}
	resp.RetentionDaysFlagged = rdf

	sev, err := h.store.IntOr(ctx, settings.KeyALPRNotifyMinSeverity, cfg.NotifyMinSeverity)
	if err != nil {
		return resp, err
	}
	resp.NotifyMinSeverity = sev

	ackedAt, ok, err := h.store.TimeOrZero(ctx, settings.KeyALPRDisclaimerAckedAt)
	if err != nil {
		return resp, err
	}
	if ok {
		s := ackedAt.UTC().Format(time.RFC3339)
		resp.DisclaimerAckedAt = &s
	}

	return resp, nil
}

// GetALPR handles GET /v1/settings/alpr. The response includes the merged
// effective settings plus computed status fields (encryption_key_configured,
// engine_reachable, disclaimer state). The encryption key itself is never
// returned.
func (h *ALPRSettingsHandler) GetALPR(c echo.Context) error {
	resp, err := h.effectiveSettings(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read alpr settings",
			Code:  http.StatusInternalServerError,
		})
	}
	resp.EngineReachable = h.probe.Check(c.Request().Context())
	return c.JSON(http.StatusOK, resp)
}

// SetALPR handles PUT /v1/settings/alpr. It validates each provided field
// against bounds, then enforces the enable preconditions: a true value for
// `enabled` requires both an encryption key and an acknowledged disclaimer
// at the current version. Either failure returns 412 with the list of
// missing prerequisites.
func (h *ALPRSettingsHandler) SetALPR(c echo.Context) error {
	var req alprSettingsRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	// Validate scalar bounds before we touch the store. This keeps the
	// settings table free of values the rest of the pipeline would have
	// to defensively re-clamp.
	if req.Region != nil {
		if _, ok := validRegions[*req.Region]; !ok {
			return c.JSON(http.StatusBadRequest, errorResponse{
				Error: "region must be one of us|eu|uk|other",
				Code:  http.StatusBadRequest,
			})
		}
	}
	if req.FramesPerSecond != nil {
		if *req.FramesPerSecond < alprBoundsFPSMin || *req.FramesPerSecond > alprBoundsFPSMax {
			return c.JSON(http.StatusBadRequest, errorResponse{
				Error: "frames_per_second must be in [0.5, 4]",
				Code:  http.StatusBadRequest,
			})
		}
	}
	if req.ConfidenceMin != nil {
		if *req.ConfidenceMin < alprBoundsConfidenceMin || *req.ConfidenceMin > alprBoundsConfidenceMax {
			return c.JSON(http.StatusBadRequest, errorResponse{
				Error: "confidence_min must be in [0.5, 0.95]",
				Code:  http.StatusBadRequest,
			})
		}
	}
	if req.RetentionDaysUnflagged != nil && *req.RetentionDaysUnflagged < 0 {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "retention_days_unflagged must be >= 0",
			Code:  http.StatusBadRequest,
		})
	}
	if req.RetentionDaysFlagged != nil && *req.RetentionDaysFlagged < 0 {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "retention_days_flagged must be >= 0",
			Code:  http.StatusBadRequest,
		})
	}
	if req.NotifyMinSeverity != nil && (*req.NotifyMinSeverity < 1 || *req.NotifyMinSeverity > 5) {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "notify_min_severity must be in [1, 5]",
			Code:  http.StatusBadRequest,
		})
	}

	// If the caller is flipping the master flag on, enforce both
	// prerequisites before writing anything. Reading the existing
	// settings here keeps the precondition check honest even when
	// callers send a partial body.
	if req.Enabled != nil && *req.Enabled {
		missing, err := h.missingEnablePrerequisites(c.Request().Context())
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse{
				Error: "failed to check alpr prerequisites",
				Code:  http.StatusInternalServerError,
			})
		}
		if len(missing) > 0 {
			return c.JSON(http.StatusPreconditionFailed, preconditionResponse{
				Error:                "alpr cannot be enabled until prerequisites are satisfied",
				Code:                 http.StatusPreconditionFailed,
				MissingPrerequisites: missing,
			})
		}
	}

	ctx := c.Request().Context()

	// Apply each provided field. We deliberately do not roll back on a
	// later failure: the only way to get into a half-applied state is a
	// transient DB error mid-batch, in which case the next PUT will
	// converge the row. The schema is small enough that any partial write
	// is observable via GET.
	if req.Enabled != nil {
		if err := h.store.SetBool(ctx, settings.KeyALPREnabled, *req.Enabled); err != nil {
			return h.dbError(c, err)
		}
	}
	if req.Region != nil {
		if err := h.store.Set(ctx, settings.KeyALPRRegion, *req.Region); err != nil {
			return h.dbError(c, err)
		}
	}
	if req.FramesPerSecond != nil {
		if err := h.store.SetFloat(ctx, settings.KeyALPRFramesPerSecond, *req.FramesPerSecond); err != nil {
			return h.dbError(c, err)
		}
	}
	if req.ConfidenceMin != nil {
		if err := h.store.SetFloat(ctx, settings.KeyALPRConfidenceMin, *req.ConfidenceMin); err != nil {
			return h.dbError(c, err)
		}
	}
	if req.RetentionDaysUnflagged != nil {
		if err := h.store.SetInt(ctx, settings.KeyALPRRetentionDaysUnflagged, *req.RetentionDaysUnflagged); err != nil {
			return h.dbError(c, err)
		}
	}
	if req.RetentionDaysFlagged != nil {
		if err := h.store.SetInt(ctx, settings.KeyALPRRetentionDaysFlagged, *req.RetentionDaysFlagged); err != nil {
			return h.dbError(c, err)
		}
	}
	if req.NotifyMinSeverity != nil {
		if err := h.store.SetInt(ctx, settings.KeyALPRNotifyMinSeverity, *req.NotifyMinSeverity); err != nil {
			return h.dbError(c, err)
		}
	}

	resp, err := h.effectiveSettings(ctx)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read alpr settings after update",
			Code:  http.StatusInternalServerError,
		})
	}
	resp.EngineReachable = h.probe.Check(ctx)
	return c.JSON(http.StatusOK, resp)
}

// missingEnablePrerequisites returns the list of unmet preconditions that
// would block enable=true. The list is stable so the frontend can map each
// entry to a fix-it hint.
func (h *ALPRSettingsHandler) missingEnablePrerequisites(ctx context.Context) ([]string, error) {
	missing := []string{}
	if h.cfg == nil || !h.cfg.EncryptionKeyConfigured() {
		missing = append(missing, "encryption_key")
	}

	disclaimerOK, err := h.disclaimerSatisfied(ctx)
	if err != nil {
		return nil, err
	}
	if !disclaimerOK {
		missing = append(missing, "disclaimer")
	}
	return missing, nil
}

// disclaimerSatisfied reports whether the operator has acknowledged the
// current disclaimer version. The alpr-disclaimer-gate feature owns the
// write path; until that lands, this always returns false (an explicit
// design choice -- enable=true must remain blocked).
func (h *ALPRSettingsHandler) disclaimerSatisfied(ctx context.Context) (bool, error) {
	version, err := h.store.StringOr(ctx, settings.KeyALPRDisclaimerVersion, "")
	if err != nil {
		return false, err
	}
	if version != ALPRDisclaimerCurrentVersion {
		return false, nil
	}
	_, ok, err := h.store.TimeOrZero(ctx, settings.KeyALPRDisclaimerAckedAt)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// dbError is a small helper that maps either an ErrNotFound (which should
// not happen on writes) or a generic store failure to a 500 response.
func (h *ALPRSettingsHandler) dbError(c echo.Context, err error) error {
	if errors.Is(err, settings.ErrNotFound) {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "alpr setting not found",
			Code:  http.StatusNotFound,
		})
	}
	return c.JSON(http.StatusInternalServerError, errorResponse{
		Error: "failed to update alpr setting",
		Code:  http.StatusInternalServerError,
	})
}

// RegisterReadRoutes wires up the read-only ALPR settings endpoint. The
// group is expected to allow either a session cookie or a device JWT so
// the dashboard and devices both work.
func (h *ALPRSettingsHandler) RegisterReadRoutes(g *echo.Group) {
	g.GET("/settings/alpr", h.GetALPR)
}

// RegisterMutationRoutes wires up the ALPR settings mutation endpoint. The
// group must require an operator session cookie -- a device JWT must never
// be able to flip enable on or off.
func (h *ALPRSettingsHandler) RegisterMutationRoutes(g *echo.Group) {
	g.PUT("/settings/alpr", h.SetALPR)
}
