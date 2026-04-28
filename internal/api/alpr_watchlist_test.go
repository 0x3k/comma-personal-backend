package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/alpr/heuristic"
	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// fakeWatchlistKeyring is a tiny stand-in for *alprcrypto.Keyring. It
// supports deterministic Hash + reversible Encrypt/Decrypt by using
// the plain plaintext bytes (uppercased + stripped) as the ciphertext.
// Tests are not exercising the AEAD itself -- they just need plate text
// to round-trip through the handler.
type fakeWatchlistKeyring struct {
	mu       sync.Mutex
	plates   map[string]string
	labels   map[string]string
	hashErr  error
	encErr   error
	encLabel error
	decErr   error
	decLabel error
}

func newFakeWatchlistKeyring() *fakeWatchlistKeyring {
	return &fakeWatchlistKeyring{
		plates: make(map[string]string),
		labels: make(map[string]string),
	}
}

func (k *fakeWatchlistKeyring) Hash(plaintext string) []byte {
	// Strip whitespace/dashes/dots and uppercase, mirroring
	// alprcrypto.Keyring's normalize. The hash is the normalised
	// bytes themselves so two plaintexts that differ only in
	// punctuation yield the same hash without any HMAC setup.
	out := []byte{}
	for i := 0; i < len(plaintext); i++ {
		c := plaintext[i]
		switch c {
		case ' ', '-', '.', '\t':
			continue
		}
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		out = append(out, c)
	}
	return out
}

func (k *fakeWatchlistKeyring) Encrypt(plaintext string) ([]byte, error) {
	if k.encErr != nil {
		return nil, k.encErr
	}
	ct := []byte("PLATE:" + plaintext)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.plates[string(ct)] = plaintext
	return ct, nil
}

func (k *fakeWatchlistKeyring) Decrypt(ciphertext []byte) (string, error) {
	if k.decErr != nil {
		return "", k.decErr
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if v, ok := k.plates[string(ciphertext)]; ok {
		return v, nil
	}
	return "", fmt.Errorf("fake: unknown plate ciphertext")
}

func (k *fakeWatchlistKeyring) EncryptLabel(plaintext string) ([]byte, error) {
	if k.encLabel != nil {
		return nil, k.encLabel
	}
	ct := []byte("LABEL:" + plaintext)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.labels[string(ct)] = plaintext
	return ct, nil
}

func (k *fakeWatchlistKeyring) DecryptLabel(ciphertext []byte) (string, error) {
	if k.decLabel != nil {
		return "", k.decLabel
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if v, ok := k.labels[string(ciphertext)]; ok {
		return v, nil
	}
	return "", fmt.Errorf("fake: unknown label ciphertext")
}

// fakeWatchlistQuerier implements alprWatchlistQuerier with a small
// in-memory store. Each test populates the maps it needs.
type fakeWatchlistQuerier struct {
	mu sync.Mutex

	rows               map[string]*db.GetWatchlistByHashRow // key: plate hash bytes
	encountersByHash   map[string][]db.GetMostRecentEncounterForPlateRow
	detectionsByRoute  map[string][]db.ListDetectionsForRouteRow
	routes             map[string]db.Route // key: dongle|route
	routesByID         map[int32]db.Route
	tripsByRouteID     map[int32]db.Trip
	eventsByHash       map[string][]db.PlateAlertEvent
	auditLog           []db.AlprAuditLog
	auditNext          int64
	insertAuditErr     error
	listAlertsErr      error
	upsertWhitelistErr error
}

func newFakeWatchlistQuerier() *fakeWatchlistQuerier {
	return &fakeWatchlistQuerier{
		rows:              make(map[string]*db.GetWatchlistByHashRow),
		encountersByHash:  make(map[string][]db.GetMostRecentEncounterForPlateRow),
		detectionsByRoute: make(map[string][]db.ListDetectionsForRouteRow),
		routes:            make(map[string]db.Route),
		routesByID:        make(map[int32]db.Route),
		tripsByRouteID:    make(map[int32]db.Trip),
		eventsByHash:      make(map[string][]db.PlateAlertEvent),
	}
}

func (f *fakeWatchlistQuerier) ListAlerts(_ context.Context, arg db.ListAlertsParams) ([]db.ListAlertsRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listAlertsErr != nil {
		return nil, f.listAlertsErr
	}
	out := []db.ListAlertsRow{}
	for _, r := range f.rows {
		if r.Kind != "alerted" {
			continue
		}
		if arg.AckedFilter.Valid {
			isAcked := r.AckedAt.Valid
			if arg.AckedFilter.Bool && !isAcked {
				continue
			}
			if !arg.AckedFilter.Bool && isAcked {
				continue
			}
		}
		out = append(out, db.ListAlertsRow{
			ID:              r.ID,
			PlateHash:       r.PlateHash,
			LabelCiphertext: r.LabelCiphertext,
			Kind:            r.Kind,
			Severity:        r.Severity,
			FirstAlertAt:    r.FirstAlertAt,
			LastAlertAt:     r.LastAlertAt,
			AckedAt:         r.AckedAt,
			Notes:           r.Notes,
			CreatedAt:       r.CreatedAt,
			UpdatedAt:       r.UpdatedAt,
		})
	}
	// Sort by last_alert_at desc for stable ordering.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if rowOlder(out[i], out[j]) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	start := int(arg.Offset)
	if start > len(out) {
		start = len(out)
	}
	end := start + int(arg.Limit)
	if end > len(out) {
		end = len(out)
	}
	return out[start:end], nil
}

func rowOlder(a, b db.ListAlertsRow) bool {
	if !a.LastAlertAt.Valid && b.LastAlertAt.Valid {
		return true
	}
	if a.LastAlertAt.Valid && b.LastAlertAt.Valid {
		return a.LastAlertAt.Time.Before(b.LastAlertAt.Time)
	}
	return false
}

func (f *fakeWatchlistQuerier) ListWatchlistByKind(_ context.Context, arg db.ListWatchlistByKindParams) ([]db.ListWatchlistByKindRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []db.ListWatchlistByKindRow{}
	for _, r := range f.rows {
		if r.Kind != arg.Kind {
			continue
		}
		out = append(out, db.ListWatchlistByKindRow{
			ID:              r.ID,
			PlateHash:       r.PlateHash,
			LabelCiphertext: r.LabelCiphertext,
			Kind:            r.Kind,
			Severity:        r.Severity,
			FirstAlertAt:    r.FirstAlertAt,
			LastAlertAt:     r.LastAlertAt,
			AckedAt:         r.AckedAt,
			Notes:           r.Notes,
			CreatedAt:       r.CreatedAt,
			UpdatedAt:       r.UpdatedAt,
		})
	}
	start := int(arg.Offset)
	if start > len(out) {
		start = len(out)
	}
	end := start + int(arg.Limit)
	if end > len(out) {
		end = len(out)
	}
	return out[start:end], nil
}

func (f *fakeWatchlistQuerier) GetWatchlistByHash(_ context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[string(plateHash)]
	if !ok {
		return db.GetWatchlistByHashRow{}, pgx.ErrNoRows
	}
	return *r, nil
}

func (f *fakeWatchlistQuerier) AckWatchlist(_ context.Context, arg db.AckWatchlistParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[string(arg.PlateHash)]
	if !ok {
		return 0, nil
	}
	r.AckedAt = arg.AckedAt
	r.UpdatedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return 1, nil
}

func (f *fakeWatchlistQuerier) UnackWatchlist(_ context.Context, plateHash []byte) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[string(plateHash)]
	if !ok {
		return 0, nil
	}
	r.AckedAt = pgtype.Timestamptz{}
	r.UpdatedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return 1, nil
}

func (f *fakeWatchlistQuerier) UpsertWatchlistWhitelist(_ context.Context, arg db.UpsertWatchlistWhitelistParams) (db.UpsertWatchlistWhitelistRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertWhitelistErr != nil {
		return db.UpsertWatchlistWhitelistRow{}, f.upsertWhitelistErr
	}
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	r, ok := f.rows[string(arg.PlateHash)]
	if !ok {
		newRow := &db.GetWatchlistByHashRow{
			ID:              int64(len(f.rows) + 1),
			PlateHash:       arg.PlateHash,
			LabelCiphertext: arg.LabelCiphertext,
			Kind:            "whitelist",
			Notes:           arg.Notes,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		f.rows[string(arg.PlateHash)] = newRow
		r = newRow
	} else {
		r.Kind = "whitelist"
		if len(arg.LabelCiphertext) > 0 {
			r.LabelCiphertext = arg.LabelCiphertext
		}
		// Whitelist transition clears alert state.
		r.Severity = pgtype.Int2{}
		r.FirstAlertAt = pgtype.Timestamptz{}
		r.LastAlertAt = pgtype.Timestamptz{}
		r.AckedAt = pgtype.Timestamptz{}
		r.UpdatedAt = now
	}
	return db.UpsertWatchlistWhitelistRow{
		ID:              r.ID,
		PlateHash:       r.PlateHash,
		LabelCiphertext: r.LabelCiphertext,
		Kind:            r.Kind,
		Severity:        r.Severity,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}, nil
}

func (f *fakeWatchlistQuerier) UpdateWatchlistNotes(_ context.Context, arg db.UpdateWatchlistNotesParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[string(arg.PlateHash)]
	if !ok {
		return 0, nil
	}
	r.Notes = arg.Notes
	return 1, nil
}

func (f *fakeWatchlistQuerier) RemoveWatchlist(_ context.Context, plateHash []byte) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[string(plateHash)]; !ok {
		return 0, nil
	}
	delete(f.rows, string(plateHash))
	return 1, nil
}

func (f *fakeWatchlistQuerier) CountUnackedAlerts(_ context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, r := range f.rows {
		if r.Kind == "alerted" && !r.AckedAt.Valid {
			n++
		}
	}
	return n, nil
}

func (f *fakeWatchlistQuerier) MaxOpenSeverity(_ context.Context) (int16, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var max int16
	for _, r := range f.rows {
		if r.Kind == "alerted" && !r.AckedAt.Valid && r.Severity.Valid && r.Severity.Int16 > max {
			max = r.Severity.Int16
		}
	}
	return max, nil
}

func (f *fakeWatchlistQuerier) GetMostRecentEncounterForPlate(_ context.Context, plateHash []byte) (db.GetMostRecentEncounterForPlateRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.encountersByHash[string(plateHash)]
	if len(rows) == 0 {
		return db.GetMostRecentEncounterForPlateRow{}, pgx.ErrNoRows
	}
	// Return the row with the latest LastSeenTs.
	best := rows[0]
	for _, r := range rows[1:] {
		if !best.LastSeenTs.Valid {
			best = r
			continue
		}
		if r.LastSeenTs.Valid && r.LastSeenTs.Time.After(best.LastSeenTs.Time) {
			best = r
		}
	}
	return best, nil
}

func (f *fakeWatchlistQuerier) CountEncountersForPlate(_ context.Context, plateHash []byte) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.encountersByHash[string(plateHash)])), nil
}

func (f *fakeWatchlistQuerier) GetRoute(_ context.Context, arg db.GetRouteParams) (db.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.routes[arg.DongleID+"|"+arg.RouteName]
	if !ok {
		return db.Route{}, pgx.ErrNoRows
	}
	return r, nil
}

func (f *fakeWatchlistQuerier) GetTripByRouteID(_ context.Context, routeID int32) (db.Trip, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tripsByRouteID[routeID]
	if !ok {
		return db.Trip{}, pgx.ErrNoRows
	}
	return t, nil
}

func (f *fakeWatchlistQuerier) ListEventsForPlate(_ context.Context, arg db.ListEventsForPlateParams) ([]db.PlateAlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.eventsByHash[string(arg.PlateHash)]
	if int(arg.Offset) >= len(rows) {
		return nil, nil
	}
	end := int(arg.Offset) + int(arg.Limit)
	if end > len(rows) {
		end = len(rows)
	}
	return rows[int(arg.Offset):end], nil
}

func (f *fakeWatchlistQuerier) ListDetectionsForRoute(_ context.Context, arg db.ListDetectionsForRouteParams) ([]db.ListDetectionsForRouteRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.detectionsByRoute[arg.DongleID+"|"+arg.Route], nil
}

func (f *fakeWatchlistQuerier) InsertAudit(_ context.Context, arg db.InsertAuditParams) (db.AlprAuditLog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertAuditErr != nil {
		return db.AlprAuditLog{}, f.insertAuditErr
	}
	f.auditNext++
	row := db.AlprAuditLog{
		ID:        f.auditNext,
		Action:    arg.Action,
		Actor:     arg.Actor,
		Payload:   arg.Payload,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	f.auditLog = append(f.auditLog, row)
	return row, nil
}

// auditCount returns the number of audit rows with the given action.
func (f *fakeWatchlistQuerier) auditCount(action string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.auditLog {
		if r.Action == action {
			n++
		}
	}
	return n
}

// makeWatchlistHandler wires a fresh handler against the supplied
// querier + keyring + suppression channel. Skips the requireAlprEnabled
// wrapper so unit tests focus on the handler body.
func makeWatchlistHandler(q alprWatchlistQuerier, k watchlistKeyring, suppressed chan<- heuristic.AlertSuppressed) *ALPRWatchlistHandler {
	store := settings.New(newALPRFakeQuerier())
	return NewALPRWatchlistHandler(q, store, k, suppressed)
}

// newSessionRequest builds an Echo context whose auth-mode is "session"
// so actorFromContext stamps a real actor on the audit row.
func newSessionRequest(t *testing.T, method, target string, body any) (*httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	e := echo.New()
	var buf *bytes.Buffer
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		buf = bytes.NewBuffer(raw)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, target, buf)
	if body != nil {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(middleware.ContextKeyAuthMode, middleware.AuthModeSession)
	c.Set(middleware.ContextKeyUserID, int32(42))
	return rec, c
}

// ----------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------

func TestAckAlert_HappyPath_Idempotent(t *testing.T) {
	hash := []byte{0x01, 0x02, 0x03}
	q := newFakeWatchlistQuerier()
	q.rows[string(hash)] = &db.GetWatchlistByHashRow{
		ID:           1,
		PlateHash:    hash,
		Kind:         "alerted",
		Severity:     pgtype.Int2{Int16: 4, Valid: true},
		FirstAlertAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
		LastAlertAt:  pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)

	// First ack: 200 + acked_at populated + audit row inserted.
	rec1, c1 := newSessionRequest(t, http.MethodPost,
		"/v1/alpr/alerts/"+base64.RawURLEncoding.EncodeToString(hash)+"/ack", nil)
	c1.SetParamNames("hash_b64")
	c1.SetParamValues(base64.RawURLEncoding.EncodeToString(hash))
	if err := h.AckAlert(c1); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if rec1.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec1.Code, rec1.Body.String())
	}
	if !q.rows[string(hash)].AckedAt.Valid {
		t.Errorf("acked_at not set after ack")
	}
	if got := q.auditCount("alert_ack"); got != 1 {
		t.Errorf("audit alert_ack = %d, want 1", got)
	}

	// Re-ack: still 200 + audit count increments (each request is a
	// distinct accountability event).
	rec2, c2 := newSessionRequest(t, http.MethodPost,
		"/v1/alpr/alerts/"+base64.RawURLEncoding.EncodeToString(hash)+"/ack", nil)
	c2.SetParamNames("hash_b64")
	c2.SetParamValues(base64.RawURLEncoding.EncodeToString(hash))
	if err := h.AckAlert(c2); err != nil {
		t.Fatalf("ack #2: %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("re-ack status = %d, want 200", rec2.Code)
	}
	if got := q.auditCount("alert_ack"); got != 2 {
		t.Errorf("audit alert_ack after re-ack = %d, want 2", got)
	}
}

func TestAckAlert_UnknownHashReturns404(t *testing.T) {
	hash := []byte{0xff, 0xee, 0xdd}
	q := newFakeWatchlistQuerier()
	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)

	rec, c := newSessionRequest(t, http.MethodPost,
		"/v1/alpr/alerts/"+base64.RawURLEncoding.EncodeToString(hash)+"/ack", nil)
	c.SetParamNames("hash_b64")
	c.SetParamValues(base64.RawURLEncoding.EncodeToString(hash))
	if err := h.AckAlert(c); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if got := q.auditCount("alert_ack"); got != 0 {
		t.Errorf("audit alert_ack on missing row = %d, want 0", got)
	}
}

func TestUnackAlert_ClearsAck(t *testing.T) {
	hash := []byte{0xa0, 0xb1}
	q := newFakeWatchlistQuerier()
	q.rows[string(hash)] = &db.GetWatchlistByHashRow{
		ID:        1,
		PlateHash: hash,
		Kind:      "alerted",
		AckedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)
	rec, c := newSessionRequest(t, http.MethodPost,
		"/v1/alpr/alerts/"+base64.RawURLEncoding.EncodeToString(hash)+"/unack", nil)
	c.SetParamNames("hash_b64")
	c.SetParamValues(base64.RawURLEncoding.EncodeToString(hash))
	if err := h.UnackAlert(c); err != nil {
		t.Fatalf("unack: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if q.rows[string(hash)].AckedAt.Valid {
		t.Errorf("acked_at still set after unack")
	}
	if q.auditCount("alert_unack") != 1 {
		t.Errorf("audit alert_unack count = %d, want 1", q.auditCount("alert_unack"))
	}
}

func TestAlertNote_SetsNotesAndAudits(t *testing.T) {
	hash := []byte{0x55, 0x66}
	q := newFakeWatchlistQuerier()
	q.rows[string(hash)] = &db.GetWatchlistByHashRow{
		ID:        1,
		PlateHash: hash,
		Kind:      "alerted",
	}
	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)
	rec, c := newSessionRequest(t, http.MethodPost,
		"/v1/alpr/alerts/"+base64.RawURLEncoding.EncodeToString(hash)+"/note",
		alertNoteRequest{Notes: "this is the white truck from Tuesday"})
	c.SetParamNames("hash_b64")
	c.SetParamValues(base64.RawURLEncoding.EncodeToString(hash))
	if err := h.AlertNote(c); err != nil {
		t.Fatalf("note: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !q.rows[string(hash)].Notes.Valid ||
		q.rows[string(hash)].Notes.String != "this is the white truck from Tuesday" {
		t.Errorf("notes not persisted: %+v", q.rows[string(hash)].Notes)
	}
	if q.auditCount("alert_note") != 1 {
		t.Errorf("audit alert_note count = %d, want 1", q.auditCount("alert_note"))
	}
}

func TestAddWhitelist_NewPlate_NoSuppressionEvent(t *testing.T) {
	q := newFakeWatchlistQuerier()
	k := newFakeWatchlistKeyring()
	suppressed := make(chan heuristic.AlertSuppressed, 4)
	h := makeWatchlistHandler(q, k, suppressed)

	rec, c := newSessionRequest(t, http.MethodPost, "/v1/alpr/whitelist",
		whitelistAddRequest{Plate: "abc-123", Label: "neighbour"})
	if err := h.AddWhitelist(c); err != nil {
		t.Fatalf("add: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	hash := k.Hash("abc-123")
	r := q.rows[string(hash)]
	if r == nil {
		t.Fatalf("watchlist row missing after add")
	}
	if r.Kind != "whitelist" {
		t.Errorf("kind = %q, want whitelist", r.Kind)
	}
	if len(r.LabelCiphertext) == 0 {
		t.Errorf("label not encrypted")
	}
	// No prior alerted row -> no suppression event.
	select {
	case ev := <-suppressed:
		t.Errorf("unexpected suppression event for new plate: %+v", ev)
	default:
	}
	if q.auditCount("whitelist_add") != 1 {
		t.Errorf("audit whitelist_add = %d, want 1", q.auditCount("whitelist_add"))
	}
}

func TestAddWhitelist_TransitionFromAlerted_EmitsSuppression(t *testing.T) {
	plate := "XYZ 789"
	k := newFakeWatchlistKeyring()
	hash := k.Hash(plate)

	q := newFakeWatchlistQuerier()
	q.rows[string(hash)] = &db.GetWatchlistByHashRow{
		ID:           7,
		PlateHash:    hash,
		Kind:         "alerted",
		Severity:     pgtype.Int2{Int16: 4, Valid: true},
		FirstAlertAt: pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true},
		LastAlertAt:  pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	suppressed := make(chan heuristic.AlertSuppressed, 4)

	h := makeWatchlistHandler(q, k, suppressed)
	rec, c := newSessionRequest(t, http.MethodPost, "/v1/alpr/whitelist",
		whitelistAddRequest{Plate: plate, Label: "Spouse"})
	if err := h.AddWhitelist(c); err != nil {
		t.Fatalf("add: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	r := q.rows[string(hash)]
	if r.Kind != "whitelist" {
		t.Errorf("kind = %q, want whitelist after transition", r.Kind)
	}
	if r.Severity.Valid {
		t.Errorf("severity = %v, want NULL after whitelist transition", r.Severity)
	}

	select {
	case ev := <-suppressed:
		if string(ev.PlateHash) != string(hash) {
			t.Errorf("suppression hash mismatch: got=%x want=%x", ev.PlateHash, hash)
		}
		if ev.PriorSeverity != 4 {
			t.Errorf("suppression prior_severity = %d, want 4", ev.PriorSeverity)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("expected AlertSuppressed event, none arrived")
	}

	if q.auditCount("whitelist_add") != 1 {
		t.Errorf("audit whitelist_add = %d, want 1", q.auditCount("whitelist_add"))
	}
}

func TestAddWhitelist_EmptyPlateRejected(t *testing.T) {
	q := newFakeWatchlistQuerier()
	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)
	for _, plate := range []string{"", "   ", "...", "-- --", "\t-.\t"} {
		rec, c := newSessionRequest(t, http.MethodPost, "/v1/alpr/whitelist",
			whitelistAddRequest{Plate: plate})
		if err := h.AddWhitelist(c); err != nil {
			t.Fatalf("add(%q): %v", plate, err)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status for plate=%q = %d, want 400", plate, rec.Code)
		}
	}
	if q.auditCount("whitelist_add") != 0 {
		t.Errorf("expected no audit rows for rejected plates, got %d", q.auditCount("whitelist_add"))
	}
}

func TestRemoveWhitelist_RestoresFutureScoring(t *testing.T) {
	// Whitelisting -> heuristic returns severity 0 due to the
	// suppression component. After removal, the same encounter set
	// should score normally again.
	plate := "ABC123"
	k := newFakeWatchlistKeyring()
	hash := k.Hash(plate)

	q := newFakeWatchlistQuerier()
	// Seed as whitelist.
	q.rows[string(hash)] = &db.GetWatchlistByHashRow{
		ID:        99,
		PlateHash: hash,
		Kind:      "whitelist",
	}

	suppressed := make(chan heuristic.AlertSuppressed, 4)
	h := makeWatchlistHandler(q, k, suppressed)

	// Build a synthetic encounter set strong enough to trigger the
	// heuristic post-removal. We exercise the pure scorer rather
	// than the worker -- the worker pulls the same Whitelisted bool
	// from the watchlist row, so the scoring layer is the load-
	// bearing piece.
	encs := []heuristic.Encounter{}
	now := time.Now()
	for i := 0; i < 4; i++ {
		encs = append(encs, heuristic.Encounter{
			EncounterID: int64(i + 1),
			Route:       fmt.Sprintf("route-%d", i),
			TurnCount:   2,
			FirstSeen:   now.Add(-time.Duration(i) * 24 * time.Hour),
			LastSeen:    now.Add(-time.Duration(i)*24*time.Hour + 5*time.Minute),
			HasGPS:      true,
			StartLat:    37.0 + float64(i)*0.1,
			StartLng:    -122.0 + float64(i)*0.1,
		})
	}

	// While whitelisted: severity must be 0.
	pre := heuristic.Score(heuristic.ScoringInput{
		PlateHash:        hash,
		RecentEncounters: encs,
		Whitelisted:      true,
		Thresholds:       heuristic.DefaultThresholds(),
	})
	if pre.Severity != 0 {
		t.Fatalf("pre-removal severity = %d, want 0 (whitelist suppression)", pre.Severity)
	}

	// Remove via handler.
	rec, c := newSessionRequest(t, http.MethodDelete,
		"/v1/alpr/whitelist/"+base64.RawURLEncoding.EncodeToString(hash), nil)
	c.SetParamNames("hash_b64")
	c.SetParamValues(base64.RawURLEncoding.EncodeToString(hash))
	if err := h.RemoveWhitelistEntry(c); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := q.rows[string(hash)]; ok {
		t.Errorf("watchlist row still present after delete")
	}

	// Post-removal: re-score with Whitelisted=false (the "real"
	// state once the row is gone) and confirm severity > 0.
	post := heuristic.Score(heuristic.ScoringInput{
		PlateHash:        hash,
		RecentEncounters: encs,
		Whitelisted:      false,
		Thresholds:       heuristic.DefaultThresholds(),
	})
	if post.Severity == 0 {
		t.Fatalf("post-removal severity = 0; expected non-zero with %d encounters", len(encs))
	}

	if q.auditCount("whitelist_remove") != 1 {
		t.Errorf("audit whitelist_remove = %d, want 1", q.auditCount("whitelist_remove"))
	}
}

func TestRemoveWhitelist_UnknownReturns404(t *testing.T) {
	q := newFakeWatchlistQuerier()
	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)
	hash := []byte{0xde, 0xad, 0xbe, 0xef}
	rec, c := newSessionRequest(t, http.MethodDelete,
		"/v1/alpr/whitelist/"+base64.RawURLEncoding.EncodeToString(hash), nil)
	c.SetParamNames("hash_b64")
	c.SetParamValues(base64.RawURLEncoding.EncodeToString(hash))
	if err := h.RemoveWhitelistEntry(c); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRemoveWhitelist_RefusesAlertedRow(t *testing.T) {
	hash := []byte{0x12}
	q := newFakeWatchlistQuerier()
	q.rows[string(hash)] = &db.GetWatchlistByHashRow{
		ID:        1,
		PlateHash: hash,
		Kind:      "alerted",
	}
	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)
	rec, c := newSessionRequest(t, http.MethodDelete,
		"/v1/alpr/whitelist/"+base64.RawURLEncoding.EncodeToString(hash), nil)
	c.SetParamNames("hash_b64")
	c.SetParamValues(base64.RawURLEncoding.EncodeToString(hash))
	if err := h.RemoveWhitelistEntry(c); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 for alerted row", rec.Code)
	}
	// Row must still exist.
	if _, ok := q.rows[string(hash)]; !ok {
		t.Errorf("alerted row was deleted by whitelist DELETE; should be untouched")
	}
}

func TestAlertsSummary_MatchesListing(t *testing.T) {
	q := newFakeWatchlistQuerier()
	now := time.Now().UTC()
	open := []byte{0x01}
	acked := []byte{0x02}
	q.rows[string(open)] = &db.GetWatchlistByHashRow{
		ID:           1,
		PlateHash:    open,
		Kind:         "alerted",
		Severity:     pgtype.Int2{Int16: 4, Valid: true},
		LastAlertAt:  pgtype.Timestamptz{Time: now.Add(-30 * time.Minute), Valid: true},
		FirstAlertAt: pgtype.Timestamptz{Time: now.Add(-60 * time.Minute), Valid: true},
	}
	q.rows[string(acked)] = &db.GetWatchlistByHashRow{
		ID:           2,
		PlateHash:    acked,
		Kind:         "alerted",
		Severity:     pgtype.Int2{Int16: 5, Valid: true},
		LastAlertAt:  pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true},
		FirstAlertAt: pgtype.Timestamptz{Time: now.Add(-4 * time.Hour), Valid: true},
		AckedAt:      pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
	}

	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)

	// Summary: 1 open, max severity 4, last_alert_at == open's LastAlertAt.
	rec, c := newSessionRequest(t, http.MethodGet, "/v1/alpr/alerts/summary", nil)
	if err := h.AlertsSummary(c); err != nil {
		t.Fatalf("summary: %v", err)
	}
	var summary alertsSummaryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.OpenCount != 1 {
		t.Errorf("open_count = %d, want 1", summary.OpenCount)
	}
	if summary.MaxOpenSeverity == nil || *summary.MaxOpenSeverity != 4 {
		t.Errorf("max_open_severity = %v, want 4 (only open's counts)", summary.MaxOpenSeverity)
	}
	if summary.LastAlertAt == nil {
		t.Errorf("last_alert_at = nil, want populated")
	}

	// Listing default (status=open) returns the open row only.
	recList, cList := newSessionRequest(t, http.MethodGet, "/v1/alpr/alerts", nil)
	if err := h.ListAlerts(cList); err != nil {
		t.Fatalf("list: %v", err)
	}
	var list alertsListResponse
	if err := json.Unmarshal(recList.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Alerts) != 1 {
		t.Fatalf("len(alerts) = %d, want 1", len(list.Alerts))
	}
	if list.Alerts[0].PlateHashB64 != base64.RawURLEncoding.EncodeToString(open) {
		t.Errorf("first alert hash mismatch")
	}
	if int64(len(list.Alerts)) != summary.OpenCount {
		t.Errorf("listing length %d != summary open_count %d", len(list.Alerts), summary.OpenCount)
	}
}

func TestListAlerts_PaginationAndStatusFilter(t *testing.T) {
	q := newFakeWatchlistQuerier()
	now := time.Now().UTC()
	for i := 0; i < 30; i++ {
		hash := []byte{byte(i)}
		q.rows[string(hash)] = &db.GetWatchlistByHashRow{
			ID:           int64(i + 1),
			PlateHash:    hash,
			Kind:         "alerted",
			Severity:     pgtype.Int2{Int16: 3, Valid: true},
			FirstAlertAt: pgtype.Timestamptz{Time: now.Add(-time.Duration(i+1) * time.Hour), Valid: true},
			LastAlertAt:  pgtype.Timestamptz{Time: now.Add(-time.Duration(i) * time.Minute), Valid: true},
		}
	}
	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)

	// Default page size is 25.
	rec, c := newSessionRequest(t, http.MethodGet, "/v1/alpr/alerts", nil)
	if err := h.ListAlerts(c); err != nil {
		t.Fatalf("list: %v", err)
	}
	var page1 alertsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page1.Alerts) != 25 {
		t.Errorf("page1 len = %d, want 25 (default limit)", len(page1.Alerts))
	}

	// limit=10 offset=20 -> 10 results (rows 21..30).
	rec2, c2 := newSessionRequest(t, http.MethodGet, "/v1/alpr/alerts?limit=10&offset=20", nil)
	c2.QueryParams() // ensure URL is parsed
	if err := h.ListAlerts(c2); err != nil {
		t.Fatalf("list 2: %v", err)
	}
	var page2 alertsListResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decode 2: %v", err)
	}
	if len(page2.Alerts) != 10 {
		t.Errorf("page2 len = %d, want 10", len(page2.Alerts))
	}

	// limit out of range -> 400.
	rec3, c3 := newSessionRequest(t, http.MethodGet, "/v1/alpr/alerts?limit=101", nil)
	if err := h.ListAlerts(c3); err != nil {
		t.Fatalf("list 3: %v", err)
	}
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("oversized limit = %d, want 400", rec3.Code)
	}

	// status=acked -> 0 (none acked in seed).
	rec4, c4 := newSessionRequest(t, http.MethodGet, "/v1/alpr/alerts?status=acked", nil)
	if err := h.ListAlerts(c4); err != nil {
		t.Fatalf("list 4: %v", err)
	}
	var ackedPage alertsListResponse
	if err := json.Unmarshal(rec4.Body.Bytes(), &ackedPage); err != nil {
		t.Fatalf("decode 4: %v", err)
	}
	if len(ackedPage.Alerts) != 0 {
		t.Errorf("acked filter len = %d, want 0", len(ackedPage.Alerts))
	}

	// status=garbage -> 400.
	rec5, c5 := newSessionRequest(t, http.MethodGet, "/v1/alpr/alerts?status=junk", nil)
	if err := h.ListAlerts(c5); err != nil {
		t.Fatalf("list 5: %v", err)
	}
	if rec5.Code != http.StatusBadRequest {
		t.Errorf("garbage status = %d, want 400", rec5.Code)
	}
}

func TestListAlerts_SeverityFilter(t *testing.T) {
	q := newFakeWatchlistQuerier()
	now := time.Now().UTC()
	for i, sev := range []int16{2, 3, 4, 5} {
		hash := []byte{byte(i + 1)}
		q.rows[string(hash)] = &db.GetWatchlistByHashRow{
			ID:          int64(i + 1),
			PlateHash:   hash,
			Kind:        "alerted",
			Severity:    pgtype.Int2{Int16: sev, Valid: true},
			LastAlertAt: pgtype.Timestamptz{Time: now.Add(-time.Duration(i) * time.Hour), Valid: true},
		}
	}
	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)

	rec, c := newSessionRequest(t, http.MethodGet, "/v1/alpr/alerts?severity=4,5", nil)
	if err := h.ListAlerts(c); err != nil {
		t.Fatalf("list: %v", err)
	}
	var page alertsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Alerts) != 2 {
		t.Errorf("severity filter len = %d, want 2", len(page.Alerts))
	}
	for _, a := range page.Alerts {
		if a.Severity == nil || (*a.Severity != 4 && *a.Severity != 5) {
			t.Errorf("severity filter leaked: %+v", a.Severity)
		}
	}

	// out-of-range -> 400.
	rec2, c2 := newSessionRequest(t, http.MethodGet, "/v1/alpr/alerts?severity=99", nil)
	if err := h.ListAlerts(c2); err != nil {
		t.Fatalf("list 2: %v", err)
	}
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("out-of-range severity = %d, want 400", rec2.Code)
	}
}

func TestRequireAlprEnabled_503Path(t *testing.T) {
	q := newFakeWatchlistQuerier()
	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)

	// Override the envelope to "off" without a real settings store.
	h.envelope = stubEnvelope{enabled: false, hasKey: true}

	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/v1/alpr/alerts", nil), rec)

	mw := h.RequireEnabled()
	gated := mw(func(echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"unreachable": "yes"})
	})
	if err := gated(c); err != nil {
		t.Fatalf("middleware err: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "alpr_disabled") {
		t.Errorf("body missing alpr_disabled marker: %s", rec.Body.String())
	}
}

func TestListAlerts_DecryptsPlateAndPopulatesLatestRoute(t *testing.T) {
	plate := "GUSTAV1"
	k := newFakeWatchlistKeyring()
	hash := k.Hash(plate)
	cipher, _ := k.Encrypt(plate)

	q := newFakeWatchlistQuerier()
	now := time.Now().UTC()
	q.rows[string(hash)] = &db.GetWatchlistByHashRow{
		ID:           1,
		PlateHash:    hash,
		Kind:         "alerted",
		Severity:     pgtype.Int2{Int16: 4, Valid: true},
		FirstAlertAt: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		LastAlertAt:  pgtype.Timestamptz{Time: now, Valid: true},
	}
	q.encountersByHash[string(hash)] = []db.GetMostRecentEncounterForPlateRow{
		{
			ID:         99,
			DongleID:   "d-1",
			Route:      "2026-04-27--09-00-00",
			PlateHash:  hash,
			LastSeenTs: pgtype.Timestamptz{Time: now, Valid: true},
		},
	}
	q.routes["d-1|2026-04-27--09-00-00"] = db.Route{
		ID:        7,
		DongleID:  "d-1",
		RouteName: "2026-04-27--09-00-00",
		StartTime: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
	}
	q.tripsByRouteID[7] = db.Trip{
		ID:           1,
		RouteID:      7,
		StartAddress: pgtype.Text{String: "Capitol Hill", Valid: true},
	}
	q.detectionsByRoute["d-1|2026-04-27--09-00-00"] = []db.ListDetectionsForRouteRow{
		{
			ID:              1,
			DongleID:        "d-1",
			Route:           "2026-04-27--09-00-00",
			PlateHash:       hash,
			PlateCiphertext: cipher,
		},
	}
	// Plate alert event so the evidence summary is non-empty.
	components, _ := json.Marshal([]evidenceComponent{
		{
			Name:   "cross_route_count",
			Points: 1,
			Evidence: map[string]any{
				"distinct_routes": 5,
			},
		},
		{
			Name:   "cross_route_geo_spread",
			Points: 1,
			Evidence: map[string]any{
				"distinct_areas": 2,
			},
		},
	})
	q.eventsByHash[string(hash)] = []db.PlateAlertEvent{
		{
			ID:         1,
			PlateHash:  hash,
			Severity:   4,
			Components: components,
			ComputedAt: pgtype.Timestamptz{Time: now, Valid: true},
		},
	}

	h := makeWatchlistHandler(q, k, nil)
	rec, c := newSessionRequest(t, http.MethodGet, "/v1/alpr/alerts", nil)
	if err := h.ListAlerts(c); err != nil {
		t.Fatalf("list: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var page alertsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Alerts) != 1 {
		t.Fatalf("len = %d, want 1", len(page.Alerts))
	}
	a := page.Alerts[0]
	if a.Plate != plate {
		t.Errorf("plate = %q, want %q", a.Plate, plate)
	}
	if a.LatestRoute == nil ||
		a.LatestRoute.DongleID != "d-1" ||
		a.LatestRoute.Route != "2026-04-27--09-00-00" {
		t.Errorf("latest_route = %+v, want d-1/2026-04-27--09-00-00", a.LatestRoute)
	}
	if a.LatestRoute.AddressLabel != "Capitol Hill" {
		t.Errorf("latest_route.address_label = %q, want Capitol Hill", a.LatestRoute.AddressLabel)
	}
	if a.EvidenceSummary == "" {
		t.Errorf("evidence_summary empty, expected non-empty from synthetic event")
	}
	if !strings.Contains(a.EvidenceSummary, "Seen on 5 trips") {
		t.Errorf("evidence_summary = %q, want it to mention 5 trips", a.EvidenceSummary)
	}
}

func TestFormatEvidenceSummary_VariantsAndEmpty(t *testing.T) {
	if got := formatEvidenceSummary(nil); got != "" {
		t.Errorf("nil components = %q, want empty", got)
	}
	if got := formatEvidenceSummary([]byte("not json")); got != "" {
		t.Errorf("garbage components = %q, want empty", got)
	}
	body, _ := json.Marshal([]evidenceComponent{
		{Name: "within_route_turns", Evidence: map[string]any{"max_turns": float64(4)}},
	})
	got := formatEvidenceSummary(body)
	if got != "followed through 4 turns once." {
		t.Errorf("single-component summary = %q", got)
	}
}

func TestListWhitelist_DecryptsLabel(t *testing.T) {
	k := newFakeWatchlistKeyring()
	plate := "DAD1"
	hash := k.Hash(plate)
	labelCipher, _ := k.EncryptLabel("Dad's car")

	q := newFakeWatchlistQuerier()
	q.rows[string(hash)] = &db.GetWatchlistByHashRow{
		ID:              1,
		PlateHash:       hash,
		LabelCiphertext: labelCipher,
		Kind:            "whitelist",
		CreatedAt:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	h := makeWatchlistHandler(q, k, nil)
	rec, c := newSessionRequest(t, http.MethodGet, "/v1/alpr/whitelist", nil)
	if err := h.ListWhitelist(c); err != nil {
		t.Fatalf("list: %v", err)
	}
	var resp whitelistListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Whitelist) != 1 {
		t.Fatalf("len = %d, want 1", len(resp.Whitelist))
	}
	if resp.Whitelist[0].Label != "Dad's car" {
		t.Errorf("label = %q, want Dad's car", resp.Whitelist[0].Label)
	}
}

func TestAckAlert_MalformedHashReturns400(t *testing.T) {
	q := newFakeWatchlistQuerier()
	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)
	rec, c := newSessionRequest(t, http.MethodPost, "/v1/alpr/alerts/!!!/ack", nil)
	c.SetParamNames("hash_b64")
	c.SetParamValues("!!!")
	if err := h.AckAlert(c); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// Compile-time guarantee that the fake satisfies the handler's
// querier interface. Cheap insurance against drift.
var _ alprWatchlistQuerier = (*fakeWatchlistQuerier)(nil)

// suppressErrors is a convenience: ensure the handler tolerates a
// nil suppression channel (no notification subsystem wired in).
func TestAddWhitelist_NilSuppressionChannelOK(t *testing.T) {
	plate := "noisy_chan"
	k := newFakeWatchlistKeyring()
	hash := k.Hash(plate)
	q := newFakeWatchlistQuerier()
	q.rows[string(hash)] = &db.GetWatchlistByHashRow{
		ID:        1,
		PlateHash: hash,
		Kind:      "alerted",
		Severity:  pgtype.Int2{Int16: 3, Valid: true},
	}
	h := makeWatchlistHandler(q, k, nil)
	rec, c := newSessionRequest(t, http.MethodPost, "/v1/alpr/whitelist",
		whitelistAddRequest{Plate: plate})
	if err := h.AddWhitelist(c); err != nil {
		t.Fatalf("add: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// Sanity check that an internal error path still produces an error
// envelope rather than panicking.
func TestAckAlert_DatabaseError(t *testing.T) {
	q := newFakeWatchlistQuerier()
	q.rows["fail"] = &db.GetWatchlistByHashRow{}
	dbErr := errors.New("simulated db failure")
	q.insertAuditErr = dbErr // ack itself succeeds; audit insert fails (logged, not surfaced)

	hash := []byte{0xab, 0xcd}
	q.rows[string(hash)] = &db.GetWatchlistByHashRow{
		ID:        1,
		PlateHash: hash,
		Kind:      "alerted",
	}
	h := makeWatchlistHandler(q, newFakeWatchlistKeyring(), nil)
	rec, c := newSessionRequest(t, http.MethodPost,
		"/v1/alpr/alerts/"+base64.RawURLEncoding.EncodeToString(hash)+"/ack", nil)
	c.SetParamNames("hash_b64")
	c.SetParamValues(base64.RawURLEncoding.EncodeToString(hash))
	if err := h.AckAlert(c); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// Per design the audit failure is NOT surfaced -- the ack still
	// succeeds. Verify the row is acked.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (audit failure shouldn't fail the request)", rec.Code)
	}
	if !q.rows[string(hash)].AckedAt.Valid {
		t.Errorf("acked_at not set despite handler returning 200")
	}
}
