package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/alpr/heuristic"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// alprTuningAuditAction is the alpr_audit_log.action string written
// every time PUT /v1/settings/alpr/tuning persists a change. The
// "Recent tuning changes" UI filters on this value, so the constant is
// the single source of truth shared with handlers, tests, and the
// frontend's apiFetch URL.
const alprTuningAuditAction = "tuning_change"

// alprTuningKnob is one editable knob in the tuning surface. The
// metadata captured here drives both the JSON response (current /
// default / bounds) and the validation pass (Min/Max for numeric
// knobs). Keeping it as data rather than a switch tree makes the
// reset-all path -- which simply iterates the slice and writes each
// default -- trivial.
type alprTuningKnob struct {
	// Key is the settings-table key the knob persists under. Also
	// the JSON object key in the GET / PUT bodies.
	Key string

	// Default is the value DefaultThresholds() carries for this
	// knob, plus the env-derived defaults for the
	// non-heuristic knobs (frame_rate / confidence_min /
	// encounter_gap_seconds / notify_min_severity).
	Default float64

	// Min / Max are the validated runtime bounds. A request whose
	// value falls outside the inclusive range is rejected with a
	// per-field error in the 422 response body.
	Min float64
	Max float64

	// IsInt is true when the knob stores an integer value rather
	// than a float (e.g. turns_min). The stringification path uses
	// Itoa so settings rows do not gain "1.0"-style suffixes.
	IsInt bool
}

// alprTuningResponse is the JSON body returned from GET
// /v1/settings/alpr/tuning. The shape is symmetric with the PUT
// request body so the dashboard can round-trip a fetched config back
// without translation.
type alprTuningResponse struct {
	FrameRate                      float64        `json:"frame_rate"`
	ConfidenceMin                  float64        `json:"confidence_min"`
	EncounterGapSeconds            float64        `json:"encounter_gap_seconds"`
	HeuristicTurnsMin              int            `json:"alpr_heuristic_turns_min"`
	HeuristicPersistenceMinutesMin float64        `json:"alpr_heuristic_persistence_minutes_min"`
	HeuristicDistinctRoutesMin     int            `json:"alpr_heuristic_distinct_routes_min"`
	HeuristicDistinctAreasMin      int            `json:"alpr_heuristic_distinct_areas_min"`
	HeuristicAreaCellKm            float64        `json:"alpr_heuristic_area_cell_km"`
	SeverityBucketSev2             float64        `json:"severity_bucket_sev2"`
	SeverityBucketSev3             float64        `json:"severity_bucket_sev3"`
	SeverityBucketSev4             float64        `json:"severity_bucket_sev4"`
	SeverityBucketSev5             float64        `json:"severity_bucket_sev5"`
	NotifyMinSeverity              int            `json:"notify_min_severity"`
	Defaults                       map[string]any `json:"defaults"`
}

// alprTuningRequest mirrors alprTuningResponse but every field is a
// pointer so omitting a knob in a PUT body keeps its prior value. The
// reset-all path calls SetTuning with a fully-populated struct
// constructed from defaults; the per-knob path can submit a single
// changed key in the same body shape.
type alprTuningRequest struct {
	FrameRate                      *float64 `json:"frame_rate,omitempty"`
	ConfidenceMin                  *float64 `json:"confidence_min,omitempty"`
	EncounterGapSeconds            *float64 `json:"encounter_gap_seconds,omitempty"`
	HeuristicTurnsMin              *int     `json:"alpr_heuristic_turns_min,omitempty"`
	HeuristicPersistenceMinutesMin *float64 `json:"alpr_heuristic_persistence_minutes_min,omitempty"`
	HeuristicDistinctRoutesMin     *int     `json:"alpr_heuristic_distinct_routes_min,omitempty"`
	HeuristicDistinctAreasMin      *int     `json:"alpr_heuristic_distinct_areas_min,omitempty"`
	HeuristicAreaCellKm            *float64 `json:"alpr_heuristic_area_cell_km,omitempty"`
	SeverityBucketSev2             *float64 `json:"severity_bucket_sev2,omitempty"`
	SeverityBucketSev3             *float64 `json:"severity_bucket_sev3,omitempty"`
	SeverityBucketSev4             *float64 `json:"severity_bucket_sev4,omitempty"`
	SeverityBucketSev5             *float64 `json:"severity_bucket_sev5,omitempty"`
	NotifyMinSeverity              *int     `json:"notify_min_severity,omitempty"`
}

// alprTuningValidationResponse is the 422 body. The map values are
// human-readable strings ("must be between 0.5 and 4 fps"); the
// frontend renders each entry next to the offending control.
type alprTuningValidationResponse struct {
	Error       string            `json:"error"`
	Code        int               `json:"code"`
	FieldErrors map[string]string `json:"field_errors"`
}

// alprTuningKnobs is the static catalogue of editable knobs. The
// order matches the visual order of the tuning UI so server logs
// and responses follow the user's mental model.
var alprTuningKnobs = []alprTuningKnob{
	{Key: settings.KeyALPRFramesPerSecond, Default: 2.0, Min: alprBoundsFPSMin, Max: alprBoundsFPSMax},
	{Key: settings.KeyALPRConfidenceMin, Default: 0.75, Min: alprBoundsConfidenceMin, Max: alprBoundsConfidenceMax},
	{Key: settings.KeyALPREncounterGapSeconds, Default: 60, Min: 15, Max: 300},
	{Key: settings.KeyALPRHeuristicTurnsMin, Default: float64(heuristic.DefaultTurnsMin), Min: 1, Max: 8, IsInt: true},
	{Key: settings.KeyALPRHeuristicPersistenceMinutesMin, Default: heuristic.DefaultPersistenceMinutesMid, Min: 3, Max: 30},
	{Key: settings.KeyALPRHeuristicDistinctRoutesMid, Default: float64(heuristic.DefaultDistinctRoutesMid), Min: 2, Max: 10, IsInt: true},
	{Key: settings.KeyALPRHeuristicDistinctAreasMin, Default: float64(heuristic.DefaultDistinctAreasMin), Min: 1, Max: 5, IsInt: true},
	{Key: settings.KeyALPRHeuristicAreaCellKm, Default: heuristic.DefaultAreaCellKm, Min: 1, Max: 20},
	{Key: settings.KeyALPRHeuristicSeverityBucketSev2, Default: heuristic.DefaultSeverityBucketSev2, Min: 0, Max: 100},
	{Key: settings.KeyALPRHeuristicSeverityBucketSev3, Default: heuristic.DefaultSeverityBucketSev3, Min: 0, Max: 100},
	{Key: settings.KeyALPRHeuristicSeverityBucketSev4, Default: heuristic.DefaultSeverityBucketSev4, Min: 0, Max: 100},
	{Key: settings.KeyALPRHeuristicSeverityBucketSev5, Default: heuristic.DefaultSeverityBucketSev5, Min: 0, Max: 100},
	{Key: settings.KeyALPRNotifyMinSeverity, Default: 4, Min: 2, Max: 5, IsInt: true},
}

// alprTuningKnobByKey is a lookup helper for the validation pass and
// the audit-diff computation. Built lazily on first access via init.
var alprTuningKnobByKey map[string]alprTuningKnob

func init() {
	alprTuningKnobByKey = make(map[string]alprTuningKnob, len(alprTuningKnobs))
	for _, k := range alprTuningKnobs {
		alprTuningKnobByKey[k.Key] = k
	}
}

// alprTuningDefaultsMap builds the {key: default} map embedded in the
// GET response so the frontend can render "Reset to default" links
// without hard-coding the constants in TypeScript.
func alprTuningDefaultsMap() map[string]any {
	out := make(map[string]any, len(alprTuningKnobs))
	for _, k := range alprTuningKnobs {
		if k.IsInt {
			out[k.Key] = int(k.Default)
		} else {
			out[k.Key] = k.Default
		}
	}
	return out
}

// GetTuning handles GET /v1/settings/alpr/tuning. Returns the merged
// effective tuning values (settings overlaid on the package defaults)
// plus the defaults map so the UI can render Reset-to-default links.
func (h *ALPRSettingsHandler) GetTuning(c echo.Context) error {
	ctx := c.Request().Context()
	resp, err := h.loadTuning(ctx)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read alpr tuning",
			Code:  http.StatusInternalServerError,
		})
	}
	return c.JSON(http.StatusOK, resp)
}

// loadTuning resolves every tuning knob by reading the settings store
// with each knob's documented default as the fallback. Used by GET
// (response body), the PUT diff (prior values for the audit log) and
// the reset-all path (composing the all-defaults request).
func (h *ALPRSettingsHandler) loadTuning(ctx context.Context) (alprTuningResponse, error) {
	resp := alprTuningResponse{Defaults: alprTuningDefaultsMap()}
	if h.store == nil {
		return resp, errors.New("settings store unavailable")
	}

	get := func(key string, def float64) (float64, error) {
		return h.store.FloatOr(ctx, key, def)
	}
	getInt := func(key string, def int) (int, error) {
		return h.store.IntOr(ctx, key, def)
	}

	var err error
	if resp.FrameRate, err = get(settings.KeyALPRFramesPerSecond, alprDefaultFor(settings.KeyALPRFramesPerSecond)); err != nil {
		return resp, err
	}
	if resp.ConfidenceMin, err = get(settings.KeyALPRConfidenceMin, alprDefaultFor(settings.KeyALPRConfidenceMin)); err != nil {
		return resp, err
	}
	if resp.EncounterGapSeconds, err = get(settings.KeyALPREncounterGapSeconds, alprDefaultFor(settings.KeyALPREncounterGapSeconds)); err != nil {
		return resp, err
	}
	if resp.HeuristicTurnsMin, err = getInt(settings.KeyALPRHeuristicTurnsMin, int(alprDefaultFor(settings.KeyALPRHeuristicTurnsMin))); err != nil {
		return resp, err
	}
	if resp.HeuristicPersistenceMinutesMin, err = get(settings.KeyALPRHeuristicPersistenceMinutesMin, alprDefaultFor(settings.KeyALPRHeuristicPersistenceMinutesMin)); err != nil {
		return resp, err
	}
	if resp.HeuristicDistinctRoutesMin, err = getInt(settings.KeyALPRHeuristicDistinctRoutesMid, int(alprDefaultFor(settings.KeyALPRHeuristicDistinctRoutesMid))); err != nil {
		return resp, err
	}
	if resp.HeuristicDistinctAreasMin, err = getInt(settings.KeyALPRHeuristicDistinctAreasMin, int(alprDefaultFor(settings.KeyALPRHeuristicDistinctAreasMin))); err != nil {
		return resp, err
	}
	if resp.HeuristicAreaCellKm, err = get(settings.KeyALPRHeuristicAreaCellKm, alprDefaultFor(settings.KeyALPRHeuristicAreaCellKm)); err != nil {
		return resp, err
	}
	if resp.SeverityBucketSev2, err = get(settings.KeyALPRHeuristicSeverityBucketSev2, alprDefaultFor(settings.KeyALPRHeuristicSeverityBucketSev2)); err != nil {
		return resp, err
	}
	if resp.SeverityBucketSev3, err = get(settings.KeyALPRHeuristicSeverityBucketSev3, alprDefaultFor(settings.KeyALPRHeuristicSeverityBucketSev3)); err != nil {
		return resp, err
	}
	if resp.SeverityBucketSev4, err = get(settings.KeyALPRHeuristicSeverityBucketSev4, alprDefaultFor(settings.KeyALPRHeuristicSeverityBucketSev4)); err != nil {
		return resp, err
	}
	if resp.SeverityBucketSev5, err = get(settings.KeyALPRHeuristicSeverityBucketSev5, alprDefaultFor(settings.KeyALPRHeuristicSeverityBucketSev5)); err != nil {
		return resp, err
	}
	if resp.NotifyMinSeverity, err = getInt(settings.KeyALPRNotifyMinSeverity, int(alprDefaultFor(settings.KeyALPRNotifyMinSeverity))); err != nil {
		return resp, err
	}
	return resp, nil
}

// alprDefaultFor returns the catalogued default for the given key, or
// 0 when the key is unknown (which only happens if a caller passes a
// typo'd key string -- guarded by the unit test suite).
func alprDefaultFor(key string) float64 {
	if k, ok := alprTuningKnobByKey[key]; ok {
		return k.Default
	}
	return 0
}

// SetTuning handles PUT /v1/settings/alpr/tuning. Applies the request
// in a single validation pass, returns 422 with field-level errors on
// any failure, and writes both the settings rows and an
// alpr_audit_log entry when validation succeeds.
func (h *ALPRSettingsHandler) SetTuning(c echo.Context) error {
	var req alprTuningRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()

	// Snapshot prior values so the audit-log diff has a "from" side
	// for each changed key.
	prior, err := h.loadTuning(ctx)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read prior alpr tuning",
			Code:  http.StatusInternalServerError,
		})
	}

	// Build the proposed full-shape config. Every field starts at
	// the prior value; the request overrides whatever it specifies.
	proposed := prior
	if req.FrameRate != nil {
		proposed.FrameRate = *req.FrameRate
	}
	if req.ConfidenceMin != nil {
		proposed.ConfidenceMin = *req.ConfidenceMin
	}
	if req.EncounterGapSeconds != nil {
		proposed.EncounterGapSeconds = *req.EncounterGapSeconds
	}
	if req.HeuristicTurnsMin != nil {
		proposed.HeuristicTurnsMin = *req.HeuristicTurnsMin
	}
	if req.HeuristicPersistenceMinutesMin != nil {
		proposed.HeuristicPersistenceMinutesMin = *req.HeuristicPersistenceMinutesMin
	}
	if req.HeuristicDistinctRoutesMin != nil {
		proposed.HeuristicDistinctRoutesMin = *req.HeuristicDistinctRoutesMin
	}
	if req.HeuristicDistinctAreasMin != nil {
		proposed.HeuristicDistinctAreasMin = *req.HeuristicDistinctAreasMin
	}
	if req.HeuristicAreaCellKm != nil {
		proposed.HeuristicAreaCellKm = *req.HeuristicAreaCellKm
	}
	if req.SeverityBucketSev2 != nil {
		proposed.SeverityBucketSev2 = *req.SeverityBucketSev2
	}
	if req.SeverityBucketSev3 != nil {
		proposed.SeverityBucketSev3 = *req.SeverityBucketSev3
	}
	if req.SeverityBucketSev4 != nil {
		proposed.SeverityBucketSev4 = *req.SeverityBucketSev4
	}
	if req.SeverityBucketSev5 != nil {
		proposed.SeverityBucketSev5 = *req.SeverityBucketSev5
	}
	if req.NotifyMinSeverity != nil {
		proposed.NotifyMinSeverity = *req.NotifyMinSeverity
	}

	if errs := validateALPRTuning(proposed); len(errs) > 0 {
		return c.JSON(http.StatusUnprocessableEntity, alprTuningValidationResponse{
			Error:       "invalid alpr tuning",
			Code:        http.StatusUnprocessableEntity,
			FieldErrors: errs,
		})
	}

	// Diff prior vs proposed so the audit log captures only the
	// changed keys (callers commonly PUT a single knob; logging
	// every key on every save would inflate the log without value).
	diff := diffALPRTuning(prior, proposed)

	if err := writeALPRTuning(ctx, h.store, proposed); err != nil {
		return h.dbError(c, err)
	}

	if len(diff) > 0 {
		payload := map[string]any{"changed_keys": diff}
		body, err := json.Marshal(payload)
		if err != nil {
			c.Logger().Errorf("alpr tuning audit marshal: %v", err)
		} else if h.audit != nil {
			if _, err := h.audit.InsertAudit(ctx, db.InsertAuditParams{
				Action:  alprTuningAuditAction,
				Actor:   actorFromContext(c),
				Payload: body,
			}); err != nil {
				c.Logger().Errorf("alpr tuning audit insert: %v", err)
			}
		}
	}

	resp, err := h.loadTuning(ctx)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read alpr tuning after update",
			Code:  http.StatusInternalServerError,
		})
	}
	return c.JSON(http.StatusOK, resp)
}

// ResetTuning handles POST /v1/settings/alpr/tuning/reset. Builds an
// all-defaults payload and forwards through SetTuning's audit + write
// path so the reset is observable in the same way as a manual
// per-knob save.
func (h *ALPRSettingsHandler) ResetTuning(c echo.Context) error {
	ctx := c.Request().Context()
	defaultResp := alprTuningResponse{
		FrameRate:                      alprDefaultFor(settings.KeyALPRFramesPerSecond),
		ConfidenceMin:                  alprDefaultFor(settings.KeyALPRConfidenceMin),
		EncounterGapSeconds:            alprDefaultFor(settings.KeyALPREncounterGapSeconds),
		HeuristicTurnsMin:              int(alprDefaultFor(settings.KeyALPRHeuristicTurnsMin)),
		HeuristicPersistenceMinutesMin: alprDefaultFor(settings.KeyALPRHeuristicPersistenceMinutesMin),
		HeuristicDistinctRoutesMin:     int(alprDefaultFor(settings.KeyALPRHeuristicDistinctRoutesMid)),
		HeuristicDistinctAreasMin:      int(alprDefaultFor(settings.KeyALPRHeuristicDistinctAreasMin)),
		HeuristicAreaCellKm:            alprDefaultFor(settings.KeyALPRHeuristicAreaCellKm),
		SeverityBucketSev2:             alprDefaultFor(settings.KeyALPRHeuristicSeverityBucketSev2),
		SeverityBucketSev3:             alprDefaultFor(settings.KeyALPRHeuristicSeverityBucketSev3),
		SeverityBucketSev4:             alprDefaultFor(settings.KeyALPRHeuristicSeverityBucketSev4),
		SeverityBucketSev5:             alprDefaultFor(settings.KeyALPRHeuristicSeverityBucketSev5),
		NotifyMinSeverity:              int(alprDefaultFor(settings.KeyALPRNotifyMinSeverity)),
	}
	prior, err := h.loadTuning(ctx)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read prior alpr tuning",
			Code:  http.StatusInternalServerError,
		})
	}
	diff := diffALPRTuning(prior, defaultResp)
	if err := writeALPRTuning(ctx, h.store, defaultResp); err != nil {
		return h.dbError(c, err)
	}
	if len(diff) > 0 && h.audit != nil {
		payload := map[string]any{"changed_keys": diff, "reset_all": true}
		body, mErr := json.Marshal(payload)
		if mErr == nil {
			if _, ierr := h.audit.InsertAudit(ctx, db.InsertAuditParams{
				Action:  alprTuningAuditAction,
				Actor:   actorFromContext(c),
				Payload: body,
			}); ierr != nil {
				c.Logger().Errorf("alpr tuning reset audit insert: %v", ierr)
			}
		}
	}
	return c.JSON(http.StatusOK, defaultResp)
}

// validateALPRTuning checks the proposed config against per-knob
// bounds and the cross-field monotonic invariant on severity buckets.
// Returns a map keyed on the request's JSON field name (so the
// dashboard can light up the offending input directly).
func validateALPRTuning(p alprTuningResponse) map[string]string {
	errs := make(map[string]string)
	checkF := func(field string, value, minV, maxV float64) {
		if value < minV || value > maxV {
			errs[field] = fmt.Sprintf("must be between %g and %g", minV, maxV)
		}
	}
	checkI := func(field string, value, minV, maxV int) {
		if value < minV || value > maxV {
			errs[field] = fmt.Sprintf("must be between %d and %d", minV, maxV)
		}
	}
	checkF("frame_rate", p.FrameRate, alprBoundsFPSMin, alprBoundsFPSMax)
	checkF("confidence_min", p.ConfidenceMin, alprBoundsConfidenceMin, alprBoundsConfidenceMax)
	checkF("encounter_gap_seconds", p.EncounterGapSeconds, 15, 300)
	checkI("alpr_heuristic_turns_min", p.HeuristicTurnsMin, 1, 8)
	checkF("alpr_heuristic_persistence_minutes_min", p.HeuristicPersistenceMinutesMin, 3, 30)
	checkI("alpr_heuristic_distinct_routes_min", p.HeuristicDistinctRoutesMin, 2, 10)
	checkI("alpr_heuristic_distinct_areas_min", p.HeuristicDistinctAreasMin, 1, 5)
	checkF("alpr_heuristic_area_cell_km", p.HeuristicAreaCellKm, 1, 20)
	checkF("severity_bucket_sev2", p.SeverityBucketSev2, 0, 100)
	checkF("severity_bucket_sev3", p.SeverityBucketSev3, 0, 100)
	checkF("severity_bucket_sev4", p.SeverityBucketSev4, 0, 100)
	checkF("severity_bucket_sev5", p.SeverityBucketSev5, 0, 100)
	checkI("notify_min_severity", p.NotifyMinSeverity, 2, 5)

	// Monotonic invariant on the four severity bucket lower edges:
	// each tier must score >= the prior tier or the bucket mapping
	// becomes ill-defined (a lower-tier promotion would skip the
	// next bucket). We only flag the first violating field so the
	// UI can display a single arrow on the broken pair.
	if !(p.SeverityBucketSev2 <= p.SeverityBucketSev3) {
		errs["severity_bucket_sev3"] = "must be greater than or equal to severity_bucket_sev2"
	}
	if !(p.SeverityBucketSev3 <= p.SeverityBucketSev4) {
		errs["severity_bucket_sev4"] = "must be greater than or equal to severity_bucket_sev3"
	}
	if !(p.SeverityBucketSev4 <= p.SeverityBucketSev5) {
		errs["severity_bucket_sev5"] = "must be greater than or equal to severity_bucket_sev4"
	}
	return errs
}

// writeALPRTuning persists the validated config. Each row is written
// independently; a transient DB failure mid-batch leaves a partially-
// applied state, but the next save converges it. Severity-bucket
// rows are written together (they are the only cross-field group)
// rather than via a single tx because the underlying settings.Store
// API is per-key and the rest of the codebase tolerates per-field
// retries the same way.
func writeALPRTuning(ctx context.Context, store *settings.Store, p alprTuningResponse) error {
	type rec struct {
		key   string
		value string
	}
	rows := []rec{
		{settings.KeyALPRFramesPerSecond, formatFloatALPR(p.FrameRate)},
		{settings.KeyALPRConfidenceMin, formatFloatALPR(p.ConfidenceMin)},
		{settings.KeyALPREncounterGapSeconds, formatFloatALPR(p.EncounterGapSeconds)},
		{settings.KeyALPRHeuristicTurnsMin, strconv.Itoa(p.HeuristicTurnsMin)},
		{settings.KeyALPRHeuristicPersistenceMinutesMin, formatFloatALPR(p.HeuristicPersistenceMinutesMin)},
		{settings.KeyALPRHeuristicDistinctRoutesMid, strconv.Itoa(p.HeuristicDistinctRoutesMin)},
		{settings.KeyALPRHeuristicDistinctAreasMin, strconv.Itoa(p.HeuristicDistinctAreasMin)},
		{settings.KeyALPRHeuristicAreaCellKm, formatFloatALPR(p.HeuristicAreaCellKm)},
		{settings.KeyALPRHeuristicSeverityBucketSev2, formatFloatALPR(p.SeverityBucketSev2)},
		{settings.KeyALPRHeuristicSeverityBucketSev3, formatFloatALPR(p.SeverityBucketSev3)},
		{settings.KeyALPRHeuristicSeverityBucketSev4, formatFloatALPR(p.SeverityBucketSev4)},
		{settings.KeyALPRHeuristicSeverityBucketSev5, formatFloatALPR(p.SeverityBucketSev5)},
		{settings.KeyALPRNotifyMinSeverity, strconv.Itoa(p.NotifyMinSeverity)},
	}
	for _, r := range rows {
		if err := store.Set(ctx, r.key, r.value); err != nil {
			return err
		}
	}
	return nil
}

// formatFloatALPR is the shared float-to-string formatting used by
// SetFloat-style writes. Matches settings.SetFloat's convention so a
// row written here is read back identically by the heuristic worker.
func formatFloatALPR(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// diffALPRTuning returns a {key: {from, to}} map covering every knob
// whose value changed between prior and proposed. Equality uses the
// concrete field type (float64 / int) so a 2.0 -> 2 type round-trip
// inside JSON does not produce a phantom change.
func diffALPRTuning(prior, proposed alprTuningResponse) map[string]map[string]any {
	diff := make(map[string]map[string]any)
	addF := func(key string, from, to float64) {
		if from != to {
			diff[key] = map[string]any{"from": from, "to": to}
		}
	}
	addI := func(key string, from, to int) {
		if from != to {
			diff[key] = map[string]any{"from": from, "to": to}
		}
	}
	addF(settings.KeyALPRFramesPerSecond, prior.FrameRate, proposed.FrameRate)
	addF(settings.KeyALPRConfidenceMin, prior.ConfidenceMin, proposed.ConfidenceMin)
	addF(settings.KeyALPREncounterGapSeconds, prior.EncounterGapSeconds, proposed.EncounterGapSeconds)
	addI(settings.KeyALPRHeuristicTurnsMin, prior.HeuristicTurnsMin, proposed.HeuristicTurnsMin)
	addF(settings.KeyALPRHeuristicPersistenceMinutesMin, prior.HeuristicPersistenceMinutesMin, proposed.HeuristicPersistenceMinutesMin)
	addI(settings.KeyALPRHeuristicDistinctRoutesMid, prior.HeuristicDistinctRoutesMin, proposed.HeuristicDistinctRoutesMin)
	addI(settings.KeyALPRHeuristicDistinctAreasMin, prior.HeuristicDistinctAreasMin, proposed.HeuristicDistinctAreasMin)
	addF(settings.KeyALPRHeuristicAreaCellKm, prior.HeuristicAreaCellKm, proposed.HeuristicAreaCellKm)
	addF(settings.KeyALPRHeuristicSeverityBucketSev2, prior.SeverityBucketSev2, proposed.SeverityBucketSev2)
	addF(settings.KeyALPRHeuristicSeverityBucketSev3, prior.SeverityBucketSev3, proposed.SeverityBucketSev3)
	addF(settings.KeyALPRHeuristicSeverityBucketSev4, prior.SeverityBucketSev4, proposed.SeverityBucketSev4)
	addF(settings.KeyALPRHeuristicSeverityBucketSev5, prior.SeverityBucketSev5, proposed.SeverityBucketSev5)
	addI(settings.KeyALPRNotifyMinSeverity, prior.NotifyMinSeverity, proposed.NotifyMinSeverity)
	return diff
}

// alprAuditListQuerier is the slice of *db.Queries this handler needs
// for the "Recent tuning changes" UI section. Carved out as an
// interface mirroring alprAuditQuerier so tests can pass the same
// fake.
type alprAuditListQuerier interface {
	alprAuditQuerier
	ListAudit(ctx context.Context, arg db.ListAuditParams) ([]db.AlprAuditLog, error)
}

// auditListItem is one row in the GET /v1/settings/alpr/tuning/audit
// response.
type auditListItem struct {
	ID        int64           `json:"id"`
	Action    string          `json:"action"`
	Actor     *string         `json:"actor"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

// ListTuningAudit handles GET /v1/settings/alpr/tuning/audit. Returns
// the most recent N tuning_change entries from alpr_audit_log.
func (h *ALPRSettingsHandler) ListTuningAudit(c echo.Context) error {
	q, ok := h.audit.(alprAuditListQuerier)
	if !ok || q == nil {
		// No list capability wired (e.g. test fake without ListAudit).
		// Returning an empty list keeps the UI rendering instead of
		// crashing.
		return c.JSON(http.StatusOK, []auditListItem{})
	}
	limit := int32(10)
	if raw := c.QueryParam("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 100 {
			limit = int32(n)
		}
	}
	rows, err := q.ListAudit(c.Request().Context(), db.ListAuditParams{
		Limit:        limit,
		Offset:       0,
		ActionFilter: pgtype.Text{String: alprTuningAuditAction, Valid: true},
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list tuning audit",
			Code:  http.StatusInternalServerError,
		})
	}
	out := make([]auditListItem, 0, len(rows))
	for _, r := range rows {
		item := auditListItem{
			ID:      r.ID,
			Action:  r.Action,
			Payload: json.RawMessage(r.Payload),
		}
		if r.Actor.Valid {
			s := r.Actor.String
			item.Actor = &s
		}
		if r.CreatedAt.Valid {
			item.CreatedAt = r.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, item)
	}
	return c.JSON(http.StatusOK, out)
}
