package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// validKeyB64 returns a base64 string that decodes to exactly 32 bytes,
// the size of an AES-256 key. Used to satisfy the encryption_key
// precondition in tests that exercise enable=true.
func alprValidKeyB64() string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// alprFakeQuerier is a multi-key in-memory stub for the settings.Querier
// interface. The retention test fake only stored a single key/value; the
// ALPR handler reads and writes several rows per request. It also
// implements the alprAuditQuerier surface (InsertAudit) so the same fake
// can stand in for the audit-log dependency without an extra struct.
type alprFakeQuerier struct {
	rows      map[string]string
	getErr    error
	upsertErr error
	insertErr error
	audit     []db.AlprAuditLog
	auditErr  error
	auditNext int64
}

func newALPRFakeQuerier() *alprFakeQuerier {
	return &alprFakeQuerier{rows: make(map[string]string)}
}

func (f *alprFakeQuerier) GetSetting(_ context.Context, key string) (db.Setting, error) {
	if f.getErr != nil {
		return db.Setting{}, f.getErr
	}
	v, ok := f.rows[key]
	if !ok {
		return db.Setting{}, pgx.ErrNoRows
	}
	return db.Setting{
		Key:       key,
		Value:     v,
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}, nil
}

func (f *alprFakeQuerier) UpsertSetting(_ context.Context, arg db.UpsertSettingParams) (db.Setting, error) {
	if f.upsertErr != nil {
		return db.Setting{}, f.upsertErr
	}
	f.rows[arg.Key] = arg.Value
	return db.Setting{
		Key:       arg.Key,
		Value:     arg.Value,
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}, nil
}

func (f *alprFakeQuerier) InsertSettingIfMissing(_ context.Context, arg db.InsertSettingIfMissingParams) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	if _, ok := f.rows[arg.Key]; !ok {
		f.rows[arg.Key] = arg.Value
	}
	return nil
}

func (f *alprFakeQuerier) InsertAudit(_ context.Context, arg db.InsertAuditParams) (db.AlprAuditLog, error) {
	if f.auditErr != nil {
		return db.AlprAuditLog{}, f.auditErr
	}
	f.auditNext++
	row := db.AlprAuditLog{
		ID:        f.auditNext,
		Action:    arg.Action,
		Actor:     arg.Actor,
		Payload:   arg.Payload,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	f.audit = append(f.audit, row)
	return row, nil
}

// newALPRTestHandler builds an ALPRSettingsHandler against a fresh in-memory
// fake querier and the supplied env-derived config. The optional rows map
// pre-populates the settings table for the test (e.g. simulating a prior
// disclaimer ack). The fake querier is reused as the audit-log backend so
// tests can assert on f.audit after calling the handler.
func newALPRTestHandler(t *testing.T, cfg *config.ALPRConfig, rows map[string]string) (*ALPRSettingsHandler, *alprFakeQuerier) {
	t.Helper()
	q := newALPRFakeQuerier()
	for k, v := range rows {
		q.rows[k] = v
	}
	h := NewALPRSettingsHandler(settings.New(q), cfg, q)
	return h, q
}

func decodeALPRResponse(t *testing.T, rec *httptest.ResponseRecorder) alprSettingsResponse {
	t.Helper()
	var resp alprSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v\nbody=%s", err, rec.Body.String())
	}
	return resp
}

func TestGetALPR_DefaultsWhenNothingStored(t *testing.T) {
	cfg := &config.ALPRConfig{
		EngineURL:              "http://alpr:8081",
		Region:                 "us",
		FramesPerSecond:        2,
		ConfidenceMin:          0.75,
		RetentionDaysUnflagged: 30,
		RetentionDaysFlagged:   365,
		NotifyMinSeverity:      4,
	}
	h, _ := newALPRTestHandler(t, cfg, nil)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/settings/alpr", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.GetALPR(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	got := decodeALPRResponse(t, rec)
	if got.Enabled {
		t.Errorf("enabled = true, want false default")
	}
	if got.EngineURL != "http://alpr:8081" {
		t.Errorf("engine_url = %q, want default", got.EngineURL)
	}
	if got.Region != "us" {
		t.Errorf("region = %q, want us", got.Region)
	}
	if got.FramesPerSecond != 2 {
		t.Errorf("frames_per_second = %v, want 2", got.FramesPerSecond)
	}
	if got.ConfidenceMin != 0.75 {
		t.Errorf("confidence_min = %v, want 0.75", got.ConfidenceMin)
	}
	if got.RetentionDaysUnflagged != 30 {
		t.Errorf("retention_days_unflagged = %d, want 30", got.RetentionDaysUnflagged)
	}
	if got.RetentionDaysFlagged != 365 {
		t.Errorf("retention_days_flagged = %d, want 365", got.RetentionDaysFlagged)
	}
	if got.NotifyMinSeverity != 4 {
		t.Errorf("notify_min_severity = %d, want 4", got.NotifyMinSeverity)
	}
	if got.EncryptionKeyConfigured {
		t.Errorf("encryption_key_configured = true, want false (no key)")
	}
	if !got.DisclaimerRequired {
		t.Errorf("disclaimer_required = false, want true")
	}
	if got.DisclaimerVersion != ALPRDisclaimerCurrentVersion {
		t.Errorf("disclaimer_version = %q, want %q", got.DisclaimerVersion, ALPRDisclaimerCurrentVersion)
	}
	if got.DisclaimerAckedAt != nil {
		t.Errorf("disclaimer_acked_at = %v, want nil", *got.DisclaimerAckedAt)
	}
}

func TestGetALPR_ReturnsStoredOverrides(t *testing.T) {
	cfg := &config.ALPRConfig{
		EngineURL:              "http://alpr:8081",
		Region:                 "us",
		FramesPerSecond:        2,
		ConfidenceMin:          0.75,
		RetentionDaysUnflagged: 30,
		RetentionDaysFlagged:   365,
		NotifyMinSeverity:      4,
		EncryptionKeyB64:       alprValidKeyB64(),
	}
	now := time.Now().UTC().Truncate(time.Second)
	h, _ := newALPRTestHandler(t, cfg, map[string]string{
		settings.KeyALPREnabled:                "true",
		settings.KeyALPRRegion:                 "eu",
		settings.KeyALPRFramesPerSecond:        "3",
		settings.KeyALPRConfidenceMin:          "0.85",
		settings.KeyALPRRetentionDaysUnflagged: "7",
		settings.KeyALPRRetentionDaysFlagged:   "180",
		settings.KeyALPRNotifyMinSeverity:      "2",
		settings.KeyALPRDisclaimerVersion:      ALPRDisclaimerCurrentVersion,
		settings.KeyALPRDisclaimerAckedAt:      now.Format(time.RFC3339),
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/settings/alpr", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.GetALPR(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	got := decodeALPRResponse(t, rec)

	if !got.Enabled {
		t.Errorf("enabled = false, want true")
	}
	if got.Region != "eu" {
		t.Errorf("region = %q, want eu", got.Region)
	}
	if got.FramesPerSecond != 3 {
		t.Errorf("frames_per_second = %v, want 3", got.FramesPerSecond)
	}
	if got.ConfidenceMin != 0.85 {
		t.Errorf("confidence_min = %v, want 0.85", got.ConfidenceMin)
	}
	if got.RetentionDaysUnflagged != 7 {
		t.Errorf("retention_days_unflagged = %d, want 7", got.RetentionDaysUnflagged)
	}
	if got.RetentionDaysFlagged != 180 {
		t.Errorf("retention_days_flagged = %d, want 180", got.RetentionDaysFlagged)
	}
	if got.NotifyMinSeverity != 2 {
		t.Errorf("notify_min_severity = %d, want 2", got.NotifyMinSeverity)
	}
	if !got.EncryptionKeyConfigured {
		t.Errorf("encryption_key_configured = false, want true")
	}
	if got.DisclaimerAckedAt == nil {
		t.Fatalf("disclaimer_acked_at = nil, want %v", now)
	}
	if *got.DisclaimerAckedAt != now.Format(time.RFC3339) {
		t.Errorf("disclaimer_acked_at = %q, want %q", *got.DisclaimerAckedAt, now.Format(time.RFC3339))
	}
}

func TestGetALPR_NeverReturnsEncryptionKey(t *testing.T) {
	cfg := &config.ALPRConfig{
		EngineURL:        "http://alpr:8081",
		EncryptionKeyB64: alprValidKeyB64(),
	}
	h, _ := newALPRTestHandler(t, cfg, nil)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/settings/alpr", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.GetALPR(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	body := rec.Body.String()
	if strings.Contains(body, cfg.EncryptionKeyB64) {
		t.Fatalf("response leaked encryption key: %s", body)
	}
	if strings.Contains(body, "encryption_key_b64") || strings.Contains(body, "\"encryption_key\":") {
		t.Fatalf("response includes a key field: %s", body)
	}
}

func TestSetALPR_EnableMissingEncryptionKeyAndDisclaimer(t *testing.T) {
	// No encryption key, no disclaimer ack -> 412 with both prerequisites.
	cfg := &config.ALPRConfig{EngineURL: "http://alpr:8081", Region: "us"}
	h, q := newALPRTestHandler(t, cfg, nil)

	rec := doPutALPR(t, h, `{"enabled":true}`)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412; body=%s", rec.Code, rec.Body.String())
	}

	var resp preconditionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if !containsString(resp.MissingPrerequisites, "encryption_key") {
		t.Errorf("missing_prerequisites = %v, want includes encryption_key", resp.MissingPrerequisites)
	}
	if !containsString(resp.MissingPrerequisites, "disclaimer") {
		t.Errorf("missing_prerequisites = %v, want includes disclaimer", resp.MissingPrerequisites)
	}
	if _, ok := q.rows[settings.KeyALPREnabled]; ok {
		t.Errorf("settings.KeyALPREnabled was written despite 412")
	}
}

func TestSetALPR_EnableMissingDisclaimerOnly(t *testing.T) {
	cfg := &config.ALPRConfig{
		EngineURL:        "http://alpr:8081",
		Region:           "us",
		EncryptionKeyB64: alprValidKeyB64(),
	}
	h, _ := newALPRTestHandler(t, cfg, nil)

	rec := doPutALPR(t, h, `{"enabled":true}`)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", rec.Code)
	}
	var resp preconditionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if containsString(resp.MissingPrerequisites, "encryption_key") {
		t.Errorf("missing_prerequisites unexpectedly contains encryption_key: %v", resp.MissingPrerequisites)
	}
	if !containsString(resp.MissingPrerequisites, "disclaimer") {
		t.Errorf("missing_prerequisites = %v, want includes disclaimer", resp.MissingPrerequisites)
	}
}

func TestSetALPR_EnableMissingEncryptionKeyOnly(t *testing.T) {
	cfg := &config.ALPRConfig{EngineURL: "http://alpr:8081", Region: "us"}
	now := time.Now().UTC().Format(time.RFC3339)
	h, _ := newALPRTestHandler(t, cfg, map[string]string{
		settings.KeyALPRDisclaimerVersion: ALPRDisclaimerCurrentVersion,
		settings.KeyALPRDisclaimerAckedAt: now,
	})

	rec := doPutALPR(t, h, `{"enabled":true}`)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", rec.Code)
	}
	var resp preconditionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if !containsString(resp.MissingPrerequisites, "encryption_key") {
		t.Errorf("missing_prerequisites = %v, want includes encryption_key", resp.MissingPrerequisites)
	}
	if containsString(resp.MissingPrerequisites, "disclaimer") {
		t.Errorf("missing_prerequisites unexpectedly contains disclaimer: %v", resp.MissingPrerequisites)
	}
}

func TestSetALPR_EnableSucceedsWhenAllSatisfied(t *testing.T) {
	cfg := &config.ALPRConfig{
		EngineURL:        "http://alpr:8081",
		Region:           "us",
		EncryptionKeyB64: alprValidKeyB64(),
		FramesPerSecond:  2,
		ConfidenceMin:    0.75,
	}
	now := time.Now().UTC().Format(time.RFC3339)
	h, q := newALPRTestHandler(t, cfg, map[string]string{
		settings.KeyALPRDisclaimerVersion: ALPRDisclaimerCurrentVersion,
		settings.KeyALPRDisclaimerAckedAt: now,
	})

	rec := doPutALPR(t, h, `{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if q.rows[settings.KeyALPREnabled] != "true" {
		t.Errorf("alpr_enabled stored = %q, want true", q.rows[settings.KeyALPREnabled])
	}
	got := decodeALPRResponse(t, rec)
	if !got.Enabled {
		t.Errorf("response.enabled = false, want true")
	}
}

func TestSetALPR_EnableFailsWhenDisclaimerVersionStale(t *testing.T) {
	cfg := &config.ALPRConfig{
		EngineURL:        "http://alpr:8081",
		EncryptionKeyB64: alprValidKeyB64(),
	}
	h, _ := newALPRTestHandler(t, cfg, map[string]string{
		settings.KeyALPRDisclaimerVersion: "0.9-old",
		settings.KeyALPRDisclaimerAckedAt: time.Now().UTC().Format(time.RFC3339),
	})

	rec := doPutALPR(t, h, `{"enabled":true}`)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412 for stale disclaimer version", rec.Code)
	}
}

func TestSetALPR_BoundsValidation(t *testing.T) {
	cfg := &config.ALPRConfig{EngineURL: "http://alpr:8081"}
	h, q := newALPRTestHandler(t, cfg, nil)

	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"fps too low", `{"frames_per_second":0.4}`, http.StatusBadRequest},
		{"fps too high", `{"frames_per_second":4.1}`, http.StatusBadRequest},
		{"fps low boundary ok", `{"frames_per_second":0.5}`, http.StatusOK},
		{"fps high boundary ok", `{"frames_per_second":4}`, http.StatusOK},
		{"confidence too low", `{"confidence_min":0.49}`, http.StatusBadRequest},
		{"confidence too high", `{"confidence_min":0.96}`, http.StatusBadRequest},
		{"confidence low boundary ok", `{"confidence_min":0.5}`, http.StatusOK},
		{"confidence high boundary ok", `{"confidence_min":0.95}`, http.StatusOK},
		{"retention unflagged negative", `{"retention_days_unflagged":-1}`, http.StatusBadRequest},
		{"retention flagged negative", `{"retention_days_flagged":-1}`, http.StatusBadRequest},
		{"retention zero ok", `{"retention_days_unflagged":0}`, http.StatusOK},
		{"region invalid", `{"region":"mars"}`, http.StatusBadRequest},
		{"region eu ok", `{"region":"eu"}`, http.StatusOK},
		{"severity below 1", `{"notify_min_severity":0}`, http.StatusBadRequest},
		{"severity above 5", `{"notify_min_severity":6}`, http.StatusBadRequest},
		{"severity in range", `{"notify_min_severity":3}`, http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset stored rows between subtests so an earlier success
			// doesn't bleed into a later failure assertion.
			q.rows = make(map[string]string)
			rec := doPutALPR(t, h, tt.body)
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d; body=%s", rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}

func TestSetALPR_RejectsInvalidJSON(t *testing.T) {
	cfg := &config.ALPRConfig{EngineURL: "http://alpr:8081"}
	h, _ := newALPRTestHandler(t, cfg, nil)
	rec := doPutALPR(t, h, `{"enabled":`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSetALPR_AppliesNonEnableUpdatesWithoutPrereqs(t *testing.T) {
	// A PUT that does NOT flip enable=true must apply field updates even
	// when the encryption key and disclaimer are missing -- otherwise the
	// operator could not configure tunables ahead of enabling.
	cfg := &config.ALPRConfig{EngineURL: "http://alpr:8081", Region: "us"}
	h, q := newALPRTestHandler(t, cfg, nil)

	rec := doPutALPR(t, h, `{"region":"eu","frames_per_second":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if q.rows[settings.KeyALPRRegion] != "eu" {
		t.Errorf("region stored = %q, want eu", q.rows[settings.KeyALPRRegion])
	}
	if q.rows[settings.KeyALPRFramesPerSecond] != "3" {
		t.Errorf("frames_per_second stored = %q, want 3", q.rows[settings.KeyALPRFramesPerSecond])
	}
	got := decodeALPRResponse(t, rec)
	if got.Region != "eu" {
		t.Errorf("response.region = %q, want eu", got.Region)
	}
}

func TestSetALPR_DisableAlwaysAllowed(t *testing.T) {
	// Even with no encryption key and no disclaimer, the operator must
	// always be able to flip enable=false (e.g. to recover from a bad
	// state). The precondition guard only fires on enable=true.
	cfg := &config.ALPRConfig{EngineURL: "http://alpr:8081"}
	h, q := newALPRTestHandler(t, cfg, map[string]string{
		settings.KeyALPREnabled: "true",
	})
	rec := doPutALPR(t, h, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if q.rows[settings.KeyALPREnabled] != "false" {
		t.Errorf("alpr_enabled stored = %q, want false", q.rows[settings.KeyALPREnabled])
	}
}

func TestALPRRegisterRoutes_RegistersExpectedPaths(t *testing.T) {
	cfg := &config.ALPRConfig{EngineURL: "http://alpr:8081"}
	h, _ := newALPRTestHandler(t, cfg, nil)

	e := echo.New()
	g := e.Group("/v1")
	h.RegisterReadRoutes(g)
	h.RegisterMutationRoutes(g)

	expected := map[string]bool{
		"GET /v1/settings/alpr": true,
		"PUT /v1/settings/alpr": true,
	}
	for _, r := range e.Routes() {
		delete(expected, r.Method+" "+r.Path)
	}
	for route := range expected {
		t.Errorf("expected route %s to be registered", route)
	}
}

func TestALPREngineReachability_NoEndpoint(t *testing.T) {
	probe := newEngineReachability("")
	if probe.Check(context.Background()) {
		t.Errorf("Check() = true with empty endpoint, want false")
	}
}

func TestALPREngineReachability_HitAndCache(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	probe := newEngineReachability(srv.URL)

	if !probe.Check(context.Background()) {
		t.Fatalf("Check() = false, want true on healthy endpoint")
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
	// Second call within TTL must not re-hit the endpoint.
	if !probe.Check(context.Background()) {
		t.Fatalf("cached Check() = false, want true")
	}
	if hits != 1 {
		t.Errorf("hits = %d after second call, want 1 (cached)", hits)
	}
}

// doPutALPR is a small helper that issues a PUT against the ALPR settings
// endpoint and returns the recorder so each test asserts on the same shape.
func doPutALPR(t *testing.T, h *ALPRSettingsHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/v1/settings/alpr", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.SetALPR(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return rec
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
