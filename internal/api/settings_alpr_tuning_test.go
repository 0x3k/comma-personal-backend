package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// alprTuningFakeQuerier extends alprFakeQuerier with the ListAudit
// method so the tuning handler's ListTuningAudit endpoint can be
// exercised against a fully in-memory backend.
type alprTuningFakeQuerier struct {
	*alprFakeQuerier
}

func (f *alprTuningFakeQuerier) ListAudit(_ context.Context, arg db.ListAuditParams) ([]db.AlprAuditLog, error) {
	if f.auditErr != nil {
		return nil, f.auditErr
	}
	out := make([]db.AlprAuditLog, 0, len(f.audit))
	for i := len(f.audit) - 1; i >= 0; i-- {
		row := f.audit[i]
		if arg.ActionFilter.Valid && row.Action != arg.ActionFilter.String {
			continue
		}
		out = append(out, row)
		if int32(len(out)) >= arg.Limit {
			break
		}
	}
	return out, nil
}

func newALPRTuningTestHandler(t *testing.T, rows map[string]string) (*ALPRSettingsHandler, *alprTuningFakeQuerier) {
	t.Helper()
	base := newALPRFakeQuerier()
	for k, v := range rows {
		base.rows[k] = v
	}
	q := &alprTuningFakeQuerier{alprFakeQuerier: base}
	cfg := &config.ALPRConfig{
		EngineURL:       "http://alpr:8081",
		Region:          "us",
		FramesPerSecond: 2,
		ConfidenceMin:   0.75,
	}
	return NewALPRSettingsHandler(settings.New(q), cfg, q), q
}

// doPutTuning issues a PUT against /v1/settings/alpr/tuning with the
// supplied body and returns the ResponseRecorder.
func doPutTuning(t *testing.T, h *ALPRSettingsHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/v1/settings/alpr/tuning", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(middleware.ContextKeyUserID, int32(7))
	c.Set(middleware.ContextKeyAuthMode, middleware.AuthModeSession)
	if err := h.SetTuning(c); err != nil {
		t.Fatalf("SetTuning returned error: %v", err)
	}
	return rec
}

func doGetTuning(t *testing.T, h *ALPRSettingsHandler) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/settings/alpr/tuning", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.GetTuning(c); err != nil {
		t.Fatalf("GetTuning error: %v", err)
	}
	return rec
}

func doPostTuningReset(t *testing.T, h *ALPRSettingsHandler) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/settings/alpr/tuning/reset", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(middleware.ContextKeyUserID, int32(7))
	c.Set(middleware.ContextKeyAuthMode, middleware.AuthModeSession)
	if err := h.ResetTuning(c); err != nil {
		t.Fatalf("ResetTuning error: %v", err)
	}
	return rec
}

func decodeTuningResponse(t *testing.T, rec *httptest.ResponseRecorder) alprTuningResponse {
	t.Helper()
	var resp alprTuningResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tuning response: %v\nbody=%s", err, rec.Body.String())
	}
	return resp
}

func TestGetTuning_ReturnsDefaultsAndDefaultsMap(t *testing.T) {
	h, _ := newALPRTuningTestHandler(t, nil)
	rec := doGetTuning(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeTuningResponse(t, rec)
	if got.FrameRate != 2.0 {
		t.Errorf("frame_rate = %v, want 2", got.FrameRate)
	}
	if got.SeverityBucketSev2 != 2 || got.SeverityBucketSev3 != 4 || got.SeverityBucketSev4 != 6 || got.SeverityBucketSev5 != 8 {
		t.Errorf("severity buckets = %v/%v/%v/%v, want 2/4/6/8",
			got.SeverityBucketSev2, got.SeverityBucketSev3, got.SeverityBucketSev4, got.SeverityBucketSev5)
	}
	if got.Defaults == nil || got.Defaults[settings.KeyALPRHeuristicSeverityBucketSev2] != 2.0 {
		t.Errorf("defaults map missing severity bucket: %v", got.Defaults)
	}
}

func TestSetTuning_FullShapeHappyPath(t *testing.T) {
	h, q := newALPRTuningTestHandler(t, nil)
	body := `{
	  "frame_rate": 3.0,
	  "confidence_min": 0.85,
	  "encounter_gap_seconds": 90,
	  "alpr_heuristic_turns_min": 3,
	  "alpr_heuristic_persistence_minutes_min": 10,
	  "alpr_heuristic_distinct_routes_min": 4,
	  "alpr_heuristic_distinct_areas_min": 3,
	  "alpr_heuristic_area_cell_km": 5,
	  "severity_bucket_sev2": 2,
	  "severity_bucket_sev3": 4,
	  "severity_bucket_sev4": 6,
	  "severity_bucket_sev5": 8,
	  "notify_min_severity": 3
	}`
	rec := doPutTuning(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeTuningResponse(t, rec)
	if got.FrameRate != 3.0 {
		t.Errorf("frame_rate = %v, want 3", got.FrameRate)
	}
	if got.HeuristicTurnsMin != 3 {
		t.Errorf("turns_min = %v, want 3", got.HeuristicTurnsMin)
	}
	if q.rows[settings.KeyALPRFramesPerSecond] != "3" {
		t.Errorf("frame_rate row = %q, want 3", q.rows[settings.KeyALPRFramesPerSecond])
	}
	if q.rows[settings.KeyALPRHeuristicTurnsMin] != "3" {
		t.Errorf("turns_min row = %q, want 3", q.rows[settings.KeyALPRHeuristicTurnsMin])
	}
	// Audit row written with the correct action and a non-empty diff
	// payload.
	if len(q.audit) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(q.audit))
	}
	if q.audit[0].Action != "tuning_change" {
		t.Errorf("audit action = %q, want tuning_change", q.audit[0].Action)
	}
	var payload map[string]any
	if err := json.Unmarshal(q.audit[0].Payload, &payload); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	changed, ok := payload["changed_keys"].(map[string]any)
	if !ok || len(changed) == 0 {
		t.Errorf("changed_keys missing or empty: %v", payload)
	}
	if _, ok := changed[settings.KeyALPRHeuristicTurnsMin]; !ok {
		t.Errorf("expected turns_min in changed_keys: %v", changed)
	}
	if !q.audit[0].Actor.Valid || q.audit[0].Actor.String != "user:7" {
		t.Errorf("audit actor = %+v, want user:7", q.audit[0].Actor)
	}
}

func TestSetTuning_RejectsMonotonicViolation(t *testing.T) {
	h, q := newALPRTuningTestHandler(t, nil)
	// sev3 < sev2 is the monotonic violation.
	body := `{"severity_bucket_sev2": 5, "severity_bucket_sev3": 4}`
	rec := doPutTuning(t, h, body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var resp alprTuningValidationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, ok := resp.FieldErrors["severity_bucket_sev3"]; !ok {
		t.Errorf("expected field_errors.severity_bucket_sev3, got %v", resp.FieldErrors)
	}
	// Must NOT write any setting or audit row on a 422.
	if v, ok := q.rows[settings.KeyALPRHeuristicSeverityBucketSev2]; ok {
		t.Errorf("sev2 row written despite 422: %q", v)
	}
	if len(q.audit) != 0 {
		t.Errorf("audit rows = %d, want 0 on 422", len(q.audit))
	}
}

func TestSetTuning_RejectsOutOfRangeKnob(t *testing.T) {
	h, _ := newALPRTuningTestHandler(t, nil)
	body := `{"frame_rate": 10}`
	rec := doPutTuning(t, h, body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var resp alprTuningValidationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, ok := resp.FieldErrors["frame_rate"]; !ok {
		t.Errorf("expected field_errors.frame_rate, got %v", resp.FieldErrors)
	}
}

func TestResetTuning_HappyPathAndAudit(t *testing.T) {
	h, q := newALPRTuningTestHandler(t, map[string]string{
		settings.KeyALPRFramesPerSecond:     "3.5",
		settings.KeyALPRHeuristicTurnsMin:   "5",
		settings.KeyALPRHeuristicAreaCellKm: "12",
	})
	rec := doPostTuningReset(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := decodeTuningResponse(t, rec)
	if got.FrameRate != 2.0 || got.HeuristicTurnsMin != 2 || got.HeuristicAreaCellKm != 5.0 {
		t.Errorf("reset values = %v/%v/%v, want defaults 2/2/5",
			got.FrameRate, got.HeuristicTurnsMin, got.HeuristicAreaCellKm)
	}
	if q.rows[settings.KeyALPRFramesPerSecond] != "2" {
		t.Errorf("frame_rate row = %q, want 2", q.rows[settings.KeyALPRFramesPerSecond])
	}
	if len(q.audit) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(q.audit))
	}
	if q.audit[0].Action != "tuning_change" {
		t.Errorf("audit action = %q, want tuning_change", q.audit[0].Action)
	}
	var payload map[string]any
	if err := json.Unmarshal(q.audit[0].Payload, &payload); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	if reset, _ := payload["reset_all"].(bool); !reset {
		t.Errorf("reset_all flag missing in audit payload: %v", payload)
	}
}

func TestSetTuning_NoOpWhenNothingChanges(t *testing.T) {
	// PUT'ing the current values must not emit an audit row -- the
	// diff is empty, the load is the source of truth.
	h, q := newALPRTuningTestHandler(t, map[string]string{
		settings.KeyALPRFramesPerSecond: "2",
		settings.KeyALPRConfidenceMin:   "0.75",
	})
	rec := doPutTuning(t, h, `{"frame_rate":2.0,"confidence_min":0.75}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(q.audit) != 0 {
		t.Errorf("audit rows = %d, want 0 on no-op save", len(q.audit))
	}
}

func TestListTuningAudit_ReturnsRecentChanges(t *testing.T) {
	h, q := newALPRTuningTestHandler(t, nil)
	// Write two audit rows directly to the fake.
	for _, action := range []string{"tuning_change", "alpr_disclaimer_ack", "tuning_change"} {
		_, err := q.InsertAudit(context.Background(), db.InsertAuditParams{
			Action:  action,
			Actor:   pgtype.Text{String: "user:1", Valid: true},
			Payload: []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/settings/alpr/tuning/audit", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.ListTuningAudit(c); err != nil {
		t.Fatalf("ListTuningAudit error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rows []auditListItem
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (filtered to tuning_change)", len(rows))
	}
	for _, r := range rows {
		if r.Action != "tuning_change" {
			t.Errorf("row action = %q, want tuning_change", r.Action)
		}
	}
}
