package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// ALPRDisclaimerCurrentVersion is the disclaimer revision the operator must
// have acknowledged before ALPR can be enabled. Bumping this value (in
// response to a material legal text change) re-prompts every operator on
// their next enable: the prior ack is preserved as an audit trail but does
// not satisfy the gate at the new version.
//
// This is a package-level var rather than a const so tests can simulate a
// version bump without rebuilding the binary; production code must never
// mutate it.
var ALPRDisclaimerCurrentVersion = "2026-04-v1"

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

// alprAuditQuerier is the slice of *db.Queries this handler needs in order
// to write to alpr_audit_log. Extracted as an interface so tests can pass
// a small in-memory fake instead of standing up Postgres.
type alprAuditQuerier interface {
	InsertAudit(ctx context.Context, arg db.InsertAuditParams) (db.AlprAuditLog, error)
}

// ALPRSettingsHandler exposes GET/PUT /v1/settings/alpr and POST
// /v1/settings/alpr/disclaimer/ack. It bridges the deployment-time
// ALPRConfig (env vars) with the runtime overrides stored in the settings
// table, and returns a status response that includes derived fields
// (encryption_key_configured, engine_reachable, disclaimer state).
type ALPRSettingsHandler struct {
	store *settings.Store
	cfg   *config.ALPRConfig
	probe *engineReachability
	audit alprAuditQuerier
}

// NewALPRSettingsHandler wires the given settings.Store with the ALPRConfig
// loaded from env. cfg may be nil in tests that do not need an engine probe.
// audit may be nil in tests that do not exercise the audit log; the
// disclaimer ack handler tolerates a nil audit by skipping the audit-log
// writes (and logging a warning) rather than failing the user request.
func NewALPRSettingsHandler(store *settings.Store, cfg *config.ALPRConfig, audit alprAuditQuerier) *ALPRSettingsHandler {
	engineURL := ""
	if cfg != nil {
		engineURL = cfg.EngineURL
	}
	return &ALPRSettingsHandler{
		store: store,
		cfg:   cfg,
		probe: newEngineReachability(engineURL),
		audit: audit,
	}
}

// alprSettingsResponse is the JSON body returned from GET /v1/settings/alpr.
//
// disclaimer_acked_at and disclaimer_acked_jurisdiction are *string so a
// missing ack serializes as JSON null rather than an empty string. The
// encryption key is intentionally never exposed -- only the boolean
// encryption_key_configured.
type alprSettingsResponse struct {
	Enabled                     bool    `json:"enabled"`
	EngineURL                   string  `json:"engine_url"`
	Region                      string  `json:"region"`
	FramesPerSecond             float64 `json:"frames_per_second"`
	ConfidenceMin               float64 `json:"confidence_min"`
	RetentionDaysUnflagged      int     `json:"retention_days_unflagged"`
	RetentionDaysFlagged        int     `json:"retention_days_flagged"`
	NotifyMinSeverity           int     `json:"notify_min_severity"`
	EncryptionKeyConfigured     bool    `json:"encryption_key_configured"`
	EngineReachable             bool    `json:"engine_reachable"`
	DisclaimerRequired          bool    `json:"disclaimer_required"`
	DisclaimerVersion           string  `json:"disclaimer_version"`
	DisclaimerAckedAt           *string `json:"disclaimer_acked_at"`
	DisclaimerAckedJurisdiction *string `json:"disclaimer_acked_jurisdiction"`
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
//
// CurrentVersion is always set when "disclaimer" is in the missing list so
// the frontend can immediately re-prompt the operator with the right ack
// payload after a server-side version bump. The Error string is set to
// "alpr_disclaimer_required" when disclaimer is the (or a) missing
// prerequisite so the UI can branch on it without inspecting the list.
type preconditionResponse struct {
	Error                string   `json:"error"`
	Code                 int      `json:"code"`
	MissingPrerequisites []string `json:"missing_prerequisites"`
	CurrentVersion       string   `json:"current_version,omitempty"`
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
		// DisclaimerRequired is recomputed below once we know whether the
		// stored ack matches CurrentDisclaimerVersion.
		DisclaimerRequired: true,
		DisclaimerVersion:  ALPRDisclaimerCurrentVersion,
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
	storedVersion, err := h.store.StringOr(ctx, settings.KeyALPRDisclaimerVersion, "")
	if err != nil {
		return resp, err
	}
	if ok {
		s := ackedAt.UTC().Format(time.RFC3339)
		resp.DisclaimerAckedAt = &s
	}
	jurisdiction, err := h.store.StringOr(ctx, settings.KeyALPRDisclaimerAckedJurisdiction, "")
	if err != nil {
		return resp, err
	}
	if jurisdiction != "" {
		j := jurisdiction
		resp.DisclaimerAckedJurisdiction = &j
	}
	// disclaimer_required is true when the operator has either never
	// acknowledged at all, or acknowledged a prior version (post-bump).
	resp.DisclaimerRequired = !ok || storedVersion != ALPRDisclaimerCurrentVersion

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
			resp := preconditionResponse{
				Error:                "alpr cannot be enabled until prerequisites are satisfied",
				Code:                 http.StatusPreconditionFailed,
				MissingPrerequisites: missing,
			}
			// When disclaimer is among the missing prereqs, surface the
			// disclaimer-specific error string and the current version so
			// the frontend can immediately re-prompt the operator without
			// a separate GET round-trip.
			if slices.Contains(missing, "disclaimer") {
				resp.Error = "alpr_disclaimer_required"
				resp.CurrentVersion = ALPRDisclaimerCurrentVersion
			}
			return c.JSON(http.StatusPreconditionFailed, resp)
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
// be able to flip enable on or off, nor record a disclaimer ack on the
// operator's behalf.
func (h *ALPRSettingsHandler) RegisterMutationRoutes(g *echo.Group) {
	g.PUT("/settings/alpr", h.SetALPR)
	g.POST("/settings/alpr/disclaimer/ack", h.AckDisclaimer)
	// Tuning surface: full-shape PUT, defaults reset, and recent-
	// changes audit list. All three are session-only -- a device JWT
	// must never be able to rewrite the heuristic thresholds the
	// operator calibrated against false-positive history.
	g.GET("/settings/alpr/tuning", h.GetTuning)
	g.PUT("/settings/alpr/tuning", h.SetTuning)
	g.POST("/settings/alpr/tuning/reset", h.ResetTuning)
	g.GET("/settings/alpr/tuning/audit", h.ListTuningAudit)
}

// validJurisdictions is the closed set accepted by the disclaimer ack
// endpoint. The set is wider than ALPRRegion (us|eu|uk|other) because the
// jurisdiction is a self-declaration of the operator's legal context for
// audit purposes -- Canada and Australia have meaningfully different
// privacy regimes and we want them recorded distinctly even though the
// detector currently has no AU/CA model.
var validJurisdictions = map[string]struct{}{
	"us":    {},
	"eu":    {},
	"uk":    {},
	"ca":    {},
	"au":    {},
	"other": {},
}

// disclaimerAckRequest is the body accepted by POST
// /v1/settings/alpr/disclaimer/ack. version must equal
// ALPRDisclaimerCurrentVersion or the request fails with 409 Conflict.
type disclaimerAckRequest struct {
	Jurisdiction string `json:"jurisdiction"`
	Version      string `json:"version"`
}

// disclaimerAckResponse is the body returned on a successful ack. It
// echoes the persisted state so the frontend can stop polling GET
// immediately after the POST resolves.
type disclaimerAckResponse struct {
	Version      string `json:"version"`
	AckedAt      string `json:"acked_at"`
	Jurisdiction string `json:"jurisdiction"`
}

// disclaimerVersionMismatchResponse is the 409 body returned when the
// client's posted version does not match the server's current version.
// The current_version field lets the frontend immediately re-fetch the
// matching disclaimer text without an extra GET.
type disclaimerVersionMismatchResponse struct {
	Error          string `json:"error"`
	Code           int    `json:"code"`
	CurrentVersion string `json:"current_version"`
}

// AckDisclaimer handles POST /v1/settings/alpr/disclaimer/ack. The endpoint
// records that the operator has acknowledged the current disclaimer
// version, capturing the declared jurisdiction. If the operator had a
// prior ack for a different version, a 'disclaimer_ack_superseded' audit
// row is written before the new ack so the audit trail preserves both the
// old and new state. A successful ack always emits an 'alpr_disclaimer_ack'
// audit row.
//
// The handler is session-only -- a device JWT must never satisfy the
// operator's legal acknowledgement on their behalf.
func (h *ALPRSettingsHandler) AckDisclaimer(c echo.Context) error {
	var req disclaimerAckRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	if _, ok := validJurisdictions[req.Jurisdiction]; !ok {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "jurisdiction must be one of us|eu|uk|ca|au|other",
			Code:  http.StatusBadRequest,
		})
	}

	if req.Version != ALPRDisclaimerCurrentVersion {
		return c.JSON(http.StatusConflict, disclaimerVersionMismatchResponse{
			Error:          "disclaimer_version_mismatch",
			Code:           http.StatusConflict,
			CurrentVersion: ALPRDisclaimerCurrentVersion,
		})
	}

	ctx := c.Request().Context()

	// Detect a superseded ack BEFORE we overwrite the row, so the audit
	// trail captures the prior version transparently. An empty/missing
	// stored version means there was no prior ack -- skip the supersede
	// row in that case (it would be misleading noise).
	priorVersion, err := h.store.StringOr(ctx, settings.KeyALPRDisclaimerVersion, "")
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read prior disclaimer version",
			Code:  http.StatusInternalServerError,
		})
	}
	priorAckedAt, priorAckedOK, err := h.store.TimeOrZero(ctx, settings.KeyALPRDisclaimerAckedAt)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read prior disclaimer ack",
			Code:  http.StatusInternalServerError,
		})
	}
	priorJurisdiction, err := h.store.StringOr(ctx, settings.KeyALPRDisclaimerAckedJurisdiction, "")
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read prior disclaimer jurisdiction",
			Code:  http.StatusInternalServerError,
		})
	}
	if priorAckedOK && priorVersion != "" && priorVersion != ALPRDisclaimerCurrentVersion {
		h.writeAudit(ctx, c, "disclaimer_ack_superseded", map[string]any{
			"prior_version":      priorVersion,
			"prior_acked_at":     priorAckedAt.UTC().Format(time.RFC3339),
			"prior_jurisdiction": priorJurisdiction,
			"new_version":        ALPRDisclaimerCurrentVersion,
		})
	}

	now := time.Now().UTC()
	ackedAtStr := now.Format(time.RFC3339)

	if err := h.store.Set(ctx, settings.KeyALPRDisclaimerVersion, ALPRDisclaimerCurrentVersion); err != nil {
		return h.dbError(c, err)
	}
	if err := h.store.Set(ctx, settings.KeyALPRDisclaimerAckedAt, ackedAtStr); err != nil {
		return h.dbError(c, err)
	}
	if err := h.store.Set(ctx, settings.KeyALPRDisclaimerAckedJurisdiction, req.Jurisdiction); err != nil {
		return h.dbError(c, err)
	}

	h.writeAudit(ctx, c, "alpr_disclaimer_ack", map[string]any{
		"jurisdiction": req.Jurisdiction,
		"version":      ALPRDisclaimerCurrentVersion,
	})

	return c.JSON(http.StatusOK, disclaimerAckResponse{
		Version:      ALPRDisclaimerCurrentVersion,
		AckedAt:      ackedAtStr,
		Jurisdiction: req.Jurisdiction,
	})
}

// writeAudit emits a single alpr_audit_log row with the given action and
// payload. Failures are logged via the Echo logger but do NOT fail the
// caller's request -- the audit log is observability, not a hard
// invariant, and dropping a row is preferable to refusing a legitimate
// disclaimer ack because of a transient DB issue. A nil h.audit is
// tolerated for tests that do not exercise the audit path.
func (h *ALPRSettingsHandler) writeAudit(ctx context.Context, c echo.Context, action string, payload map[string]any) {
	if h.audit == nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		c.Logger().Errorf("alpr audit payload marshal failed: %v", err)
		return
	}
	if _, err := h.audit.InsertAudit(ctx, db.InsertAuditParams{
		Action:  action,
		Actor:   actorFromContext(c),
		Payload: body,
	}); err != nil {
		c.Logger().Errorf("alpr audit insert failed (action=%s): %v", action, err)
	}
}

// actorFromContext records who performed the action. Mirrors
// requesterFromContext in route_data_request.go: session callers are
// stamped "user:<id>" (the username is not in the Echo context); a missing
// user id yields a NULL actor. This handler is session-only so a JWT
// caller is never expected here, but if one slipped through we still want
// the audit row to be NULL rather than misleadingly attributed.
func actorFromContext(c echo.Context) pgtype.Text {
	mode, _ := c.Get(middleware.ContextKeyAuthMode).(string)
	if mode != middleware.AuthModeSession {
		return pgtype.Text{}
	}
	userID, ok := c.Get(middleware.ContextKeyUserID).(int32)
	if !ok || userID == 0 {
		return pgtype.Text{}
	}
	return pgtype.Text{String: fmt.Sprintf("user:%d", userID), Valid: true}
}
