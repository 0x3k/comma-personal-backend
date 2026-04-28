package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/worker"
)

// ----------------------------------------------------------------------
// Test fakes
// ----------------------------------------------------------------------

// fakeCorrectionsKeyring is the test stand-in for *alprcrypto.Keyring.
// Hash mirrors the real normalize() (uppercase + strip space/dash/dot/
// tab) so two plaintexts that differ only in punctuation match -- the
// same invariant the watchlist test fakes already enforce.
type fakeCorrectionsKeyring struct {
	mu     sync.Mutex
	plates map[string]string // ciphertext -> plaintext
	encErr error
}

func newFakeCorrectionsKeyring() *fakeCorrectionsKeyring {
	return &fakeCorrectionsKeyring{plates: make(map[string]string)}
}

func (k *fakeCorrectionsKeyring) Hash(plaintext string) []byte {
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
	// Pad to 32 bytes so the schema's octet_length(plate_hash)=32
	// invariant stays satisfied; tests that exercise the on-the-wire
	// b64 length expect 32-byte hashes.
	if len(out) < 32 {
		pad := make([]byte, 32-len(out))
		out = append(out, pad...)
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

func (k *fakeCorrectionsKeyring) Encrypt(plaintext string) ([]byte, error) {
	if k.encErr != nil {
		return nil, k.encErr
	}
	ct := []byte("PLATE:" + plaintext)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.plates[string(ct)] = plaintext
	return ct, nil
}

// fakeCorrectionsQuerier implements alprCorrectionsQuerier with a small
// in-memory store. Each test populates the maps it needs.
type fakeCorrectionsQuerier struct {
	mu sync.Mutex

	detectionsByID map[int64]*db.GetDetectionByIDRow
	encountersByID map[int64]*db.PlateEncounter
	watchlist      map[string]*db.GetWatchlistByHashRow // key: hash bytes

	auditLog       []db.AlprAuditLog
	auditNext      int64
	insertAuditErr error

	updateDetectionErr error
	bulkDetectionsErr  error
	bulkEncountersErr  error
}

func newFakeCorrectionsQuerier() *fakeCorrectionsQuerier {
	return &fakeCorrectionsQuerier{
		detectionsByID: make(map[int64]*db.GetDetectionByIDRow),
		encountersByID: make(map[int64]*db.PlateEncounter),
		watchlist:      make(map[string]*db.GetWatchlistByHashRow),
	}
}

// WithTxQuerier returns the same fake -- the in-memory state is the
// transaction. Mirrors the worker package's aggregator fake.
func (f *fakeCorrectionsQuerier) WithTxQuerier(_ pgx.Tx) alprCorrectionsQuerier { return f }

func (f *fakeCorrectionsQuerier) GetDetectionByID(_ context.Context, id int64) (db.GetDetectionByIDRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.detectionsByID[id]
	if !ok {
		return db.GetDetectionByIDRow{}, pgx.ErrNoRows
	}
	return *r, nil
}

func (f *fakeCorrectionsQuerier) UpdateDetectionPlate(_ context.Context, arg db.UpdateDetectionPlateParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateDetectionErr != nil {
		return f.updateDetectionErr
	}
	r, ok := f.detectionsByID[arg.ID]
	if !ok {
		return pgx.ErrNoRows
	}
	r.PlateCiphertext = arg.PlateCiphertext
	r.PlateHash = arg.PlateHash
	r.OcrCorrected = true
	return nil
}

func (f *fakeCorrectionsQuerier) BulkUpdateDetectionsHashOnly(_ context.Context, arg db.BulkUpdateDetectionsHashOnlyParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bulkDetectionsErr != nil {
		return 0, f.bulkDetectionsErr
	}
	var n int64
	for _, r := range f.detectionsByID {
		if string(r.PlateHash) == string(arg.OldPlateHash) {
			r.PlateHash = append([]byte(nil), arg.NewPlateHash...)
			n++
		}
	}
	return n, nil
}

func (f *fakeCorrectionsQuerier) BulkUpdateEncountersPlateHash(_ context.Context, arg db.BulkUpdateEncountersPlateHashParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bulkEncountersErr != nil {
		return 0, f.bulkEncountersErr
	}
	var n int64
	for _, e := range f.encountersByID {
		if string(e.PlateHash) == string(arg.OldPlateHash) {
			e.PlateHash = append([]byte(nil), arg.NewPlateHash...)
			n++
		}
	}
	return n, nil
}

func (f *fakeCorrectionsQuerier) DistinctRoutesForPlateHash(_ context.Context, plateHash []byte) ([]db.DistinctRoutesForPlateHashRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]struct{}{}
	out := []db.DistinctRoutesForPlateHashRow{}
	for _, r := range f.detectionsByID {
		if string(r.PlateHash) != string(plateHash) {
			continue
		}
		k := r.DongleID + "|" + r.Route
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, db.DistinctRoutesForPlateHashRow{
			DongleID: r.DongleID, Route: r.Route,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DongleID != out[j].DongleID {
			return out[i].DongleID < out[j].DongleID
		}
		return out[i].Route < out[j].Route
	})
	return out, nil
}

func (f *fakeCorrectionsQuerier) DistinctRoutesForEncountersPlateHash(_ context.Context, plateHash []byte) ([]db.DistinctRoutesForEncountersPlateHashRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]struct{}{}
	out := []db.DistinctRoutesForEncountersPlateHashRow{}
	for _, e := range f.encountersByID {
		if string(e.PlateHash) != string(plateHash) {
			continue
		}
		k := e.DongleID + "|" + e.Route
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, db.DistinctRoutesForEncountersPlateHashRow{
			DongleID: e.DongleID, Route: e.Route,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DongleID != out[j].DongleID {
			return out[i].DongleID < out[j].DongleID
		}
		return out[i].Route < out[j].Route
	})
	return out, nil
}

func (f *fakeCorrectionsQuerier) GetWatchlistByHash(_ context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.watchlist[string(plateHash)]
	if !ok {
		return db.GetWatchlistByHashRow{}, pgx.ErrNoRows
	}
	return *r, nil
}

func (f *fakeCorrectionsQuerier) RenameWatchlistHash(_ context.Context, arg db.RenameWatchlistHashParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.watchlist[string(arg.OldPlateHash)]
	if !ok {
		return 0, nil
	}
	delete(f.watchlist, string(arg.OldPlateHash))
	r.PlateHash = append([]byte(nil), arg.NewPlateHash...)
	r.UpdatedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	f.watchlist[string(arg.NewPlateHash)] = r
	return 1, nil
}

func (f *fakeCorrectionsQuerier) ApplyMergedWatchlistRow(_ context.Context, arg db.ApplyMergedWatchlistRowParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.watchlist[string(arg.PlateHash)]
	if !ok {
		return 0, nil
	}
	r.Severity = arg.Severity
	r.FirstAlertAt = arg.FirstAlertAt
	r.LastAlertAt = arg.LastAlertAt
	r.AckedAt = arg.AckedAt
	r.Notes = arg.Notes
	r.UpdatedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return 1, nil
}

func (f *fakeCorrectionsQuerier) RemoveWatchlist(_ context.Context, plateHash []byte) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.watchlist[string(plateHash)]; !ok {
		return 0, nil
	}
	delete(f.watchlist, string(plateHash))
	return 1, nil
}

func (f *fakeCorrectionsQuerier) InsertAudit(_ context.Context, arg db.InsertAuditParams) (db.AlprAuditLog, error) {
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

// auditCount reports how many audit rows match a given action.
func (f *fakeCorrectionsQuerier) auditCount(action string) int {
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

// lastAuditPayload returns the payload of the most-recent audit row
// matching action, decoded as a generic map.
func (f *fakeCorrectionsQuerier) lastAuditPayload(t *testing.T, action string) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.auditLog) - 1; i >= 0; i-- {
		if f.auditLog[i].Action != action {
			continue
		}
		var out map[string]any
		if err := json.Unmarshal(f.auditLog[i].Payload, &out); err != nil {
			t.Fatalf("unmarshal audit payload: %v", err)
		}
		return out
	}
	t.Fatalf("no audit row for action %q", action)
	return nil
}

// correctionsFakeTxBeginner implements alprCorrectionsTxBeginner. The returned tx
// is a no-op stub: every method on pgx.Tx is implemented as a return-
// nil because the in-memory fake querier does not require a real tx.
type correctionsFakeTxBeginner struct {
	beginErr error
}

func (b *correctionsFakeTxBeginner) Begin(_ context.Context) (pgx.Tx, error) {
	if b.beginErr != nil {
		return nil, b.beginErr
	}
	return &correctionsFakeTx{}, nil
}

type correctionsFakeTx struct{}

func (t *correctionsFakeTx) Begin(_ context.Context) (pgx.Tx, error) { return t, nil }
func (t *correctionsFakeTx) BeginFunc(_ context.Context, _ func(pgx.Tx) error) error {
	return errors.New("not implemented")
}
func (t *correctionsFakeTx) Commit(_ context.Context) error   { return nil }
func (t *correctionsFakeTx) Rollback(_ context.Context) error { return nil }
func (t *correctionsFakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("not implemented")
}
func (t *correctionsFakeTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults { return nil }
func (t *correctionsFakeTx) LargeObjects() pgx.LargeObjects                             { return pgx.LargeObjects{} }
func (t *correctionsFakeTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	return nil, errors.New("not implemented")
}
func (t *correctionsFakeTx) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (t *correctionsFakeTx) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("not implemented")
}
func (t *correctionsFakeTx) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row { return nil }
func (t *correctionsFakeTx) Conn() *pgx.Conn                                        { return nil }

// makeCorrectionsHandler wires a fresh handler against the supplied
// dependencies. The test bypasses the requireAlprEnabled middleware so
// the handler body is the focus.
func makeCorrectionsHandler(
	q *fakeCorrectionsQuerier,
	k correctionsKeyring,
	completions chan worker.RouteAlprDetectionsComplete,
) *ALPRCorrectionsHandler {
	store := settings.New(newALPRFakeQuerier())
	beginner := &correctionsFakeTxBeginner{}
	var ch chan<- worker.RouteAlprDetectionsComplete
	if completions != nil {
		ch = completions
	}
	return NewALPRCorrectionsHandler(q, beginner, store, k, ch)
}

// addDetection seeds a detection in the fake querier with a given id,
// route, and plate hash. The ciphertext is set to the literal "PLATE:"
// prefix that fakeCorrectionsKeyring.Encrypt produces, so a decrypt
// round-trip in tests is symmetric with the keyring's behaviour.
func (f *fakeCorrectionsQuerier) addDetection(id int64, dongleID, route string, plate string, hash []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detectionsByID[id] = &db.GetDetectionByIDRow{
		ID:              id,
		DongleID:        dongleID,
		Route:           route,
		PlateCiphertext: []byte("PLATE:" + plate),
		PlateHash:       append([]byte(nil), hash...),
		FrameTs:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
}

// addEncounter seeds an encounter row in the fake querier.
func (f *fakeCorrectionsQuerier) addEncounter(id int64, dongleID, route string, hash []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.encountersByID[id] = &db.PlateEncounter{
		ID:          id,
		DongleID:    dongleID,
		Route:       route,
		PlateHash:   append([]byte(nil), hash...),
		FirstSeenTs: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		LastSeenTs:  pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
}

// addWatchlist seeds a watchlist row in the fake querier.
func (f *fakeCorrectionsQuerier) addWatchlist(hash []byte, kind string, severity int16, notes string, acked bool) *db.GetWatchlistByHashRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	row := &db.GetWatchlistByHashRow{
		ID:        int64(len(f.watchlist) + 1),
		PlateHash: append([]byte(nil), hash...),
		Kind:      kind,
	}
	if severity > 0 {
		row.Severity = pgtype.Int2{Int16: severity, Valid: true}
	}
	if notes != "" {
		row.Notes = pgtype.Text{String: notes, Valid: true}
	}
	row.FirstAlertAt = pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}
	row.LastAlertAt = pgtype.Timestamptz{Time: now, Valid: true}
	if acked {
		row.AckedAt = pgtype.Timestamptz{Time: now, Valid: true}
	}
	row.CreatedAt = pgtype.Timestamptz{Time: now, Valid: true}
	row.UpdatedAt = pgtype.Timestamptz{Time: now, Valid: true}
	f.watchlist[string(hash)] = row
	return row
}

// drainRoutes pulls any pending re-trigger events off the channel and
// returns them sorted by (dongle_id, route) for stable assertions.
func drainRoutes(ch chan worker.RouteAlprDetectionsComplete) []worker.RouteAlprDetectionsComplete {
	out := []worker.RouteAlprDetectionsComplete{}
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			sort.Slice(out, func(i, j int) bool {
				if out[i].DongleID != out[j].DongleID {
					return out[i].DongleID < out[j].DongleID
				}
				return out[i].Route < out[j].Route
			})
			return out
		}
	}
}

// newSessionRequestForCorrections builds an Echo context with session-
// auth context keys set so actorFromContext stamps a real actor on the
// audit row. Mirrors newSessionRequest in alpr_watchlist_test.go.
func newSessionRequestForCorrections(t *testing.T, method, target string, body any) (*httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	return newSessionRequest(t, method, target, body)
}

// newJWTRequestForCorrections is the JWT-mode counterpart. Used by the
// cross-device 403 test.
func newJWTRequestForCorrections(t *testing.T, method, target, dongleID string, body any) (*httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	rec, c := newSessionRequest(t, method, target, body)
	// Override auth-mode to JWT and stamp the dongle context. checkDongleAccess
	// reads ContextKeyAuthMode != AuthModeSession to engage cross-dongle
	// guard.
	c.Set(middleware.ContextKeyAuthMode, "jwt")
	c.Set(middleware.ContextKeyUserID, int32(0))
	c.Set(middleware.ContextKeyDongleID, dongleID)
	return rec, c
}

// ----------------------------------------------------------------------
// PATCH /v1/alpr/detections/:id tests
// ----------------------------------------------------------------------

func TestEditDetection_HappyPathRehashes(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()

	oldHash := k.Hash("0CR123")
	q.addDetection(7, "dongle-A", "2024-01-01--12-00-00", "0CR123", oldHash)
	q.addEncounter(11, "dongle-A", "2024-01-01--12-00-00", oldHash)

	completions := make(chan worker.RouteAlprDetectionsComplete, 8)
	h := makeCorrectionsHandler(q, k, completions)

	rec, c := newSessionRequestForCorrections(t, http.MethodPatch,
		"/v1/alpr/detections/7", editDetectionRequest{Plate: "OCR123"})
	c.SetParamNames("id")
	c.SetParamValues("7")
	if err := h.EditDetection(c); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp editDetectionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Accepted {
		t.Errorf("accepted = false, want true")
	}

	// New hash + ciphertext stamped on the detection row, ocr_corrected
	// flipped.
	got := q.detectionsByID[7]
	newHash := k.Hash("OCR123")
	if string(got.PlateHash) != string(newHash) {
		t.Errorf("plate_hash = %x, want %x", got.PlateHash, newHash)
	}
	if !got.OcrCorrected {
		t.Errorf("ocr_corrected not flipped")
	}
	// Re-hashing produces the SAME hash a fresh ingest of the same plate
	// would (criterion 3): a brand-new detection of "OCR-123" hashes to
	// the same value because normalize() folds out the dash.
	freshHash := k.Hash("OCR-123")
	if string(freshHash) != string(newHash) {
		t.Errorf("fresh hash %x != edit hash %x (re-hashing must use the same Hash function)",
			freshHash, newHash)
	}

	// One audit row of action=plate_edit with the OLD hash as
	// before_value, NEW hash as after_value, no plaintext.
	if q.auditCount("plate_edit") != 1 {
		t.Errorf("plate_edit audit count = %d, want 1", q.auditCount("plate_edit"))
	}
	pl := q.lastAuditPayload(t, "plate_edit")
	if got, want := pl["before_value"], base64.RawURLEncoding.EncodeToString(oldHash); got != want {
		t.Errorf("before_value = %v, want %s", got, want)
	}
	if got, want := pl["after_value"], base64.RawURLEncoding.EncodeToString(newHash); got != want {
		t.Errorf("after_value = %v, want %s", got, want)
	}
	for k := range pl {
		if strings.Contains(strings.ToLower(k), "plain") || strings.Contains(strings.ToLower(k), "ciphertext") {
			t.Errorf("audit payload key %q hints at plaintext leakage", k)
		}
	}
	for _, v := range pl {
		s, _ := v.(string)
		if s == "OCR123" || s == "0CR123" {
			t.Errorf("audit payload value %q contains plate text", s)
		}
	}

	// Re-trigger: the detection's route at minimum must have been
	// enqueued. Routes for the OLD hash on other drives were also
	// enqueued (none in this test).
	events := drainRoutes(completions)
	if len(events) < 1 {
		t.Fatalf("no aggregator re-trigger events emitted")
	}
	foundDetectionRoute := false
	for _, ev := range events {
		if ev.DongleID == "dongle-A" && ev.Route == "2024-01-01--12-00-00" {
			foundDetectionRoute = true
		}
	}
	if !foundDetectionRoute {
		t.Errorf("detection's own route not enqueued; got %+v", events)
	}
	if got := resp.AffectedRoutes; got < 1 {
		t.Errorf("affected_routes = %d, want >= 1", got)
	}
}

func TestEditDetection_IdempotentOnAlreadyCorrected(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()
	hash := k.Hash("OCR123")
	q.addDetection(7, "dongle-A", "r1", "OCR123", hash)
	q.detectionsByID[7].OcrCorrected = true

	h := makeCorrectionsHandler(q, k, nil)
	rec, c := newSessionRequestForCorrections(t, http.MethodPatch,
		"/v1/alpr/detections/7", editDetectionRequest{Plate: "OCR123"})
	c.SetParamNames("id")
	c.SetParamValues("7")
	if err := h.EditDetection(c); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !q.detectionsByID[7].OcrCorrected {
		t.Errorf("ocr_corrected flipped off by idempotent edit")
	}
	if string(q.detectionsByID[7].PlateHash) != string(hash) {
		t.Errorf("plate_hash drifted on idempotent edit")
	}
}

func TestEditDetection_EmptyPlateReturns400(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()
	q.addDetection(7, "dongle-A", "r1", "OCR", k.Hash("OCR"))
	h := makeCorrectionsHandler(q, k, nil)

	for _, p := range []string{"", "   ", " - . - "} {
		rec, c := newSessionRequestForCorrections(t, http.MethodPatch,
			"/v1/alpr/detections/7", editDetectionRequest{Plate: p})
		c.SetParamNames("id")
		c.SetParamValues("7")
		if err := h.EditDetection(c); err != nil {
			t.Fatalf("edit (plate=%q): %v", p, err)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("plate=%q: status = %d, want 400", p, rec.Code)
		}
	}
}

func TestEditDetection_UnknownIDReturns404(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()
	h := makeCorrectionsHandler(q, k, nil)

	rec, c := newSessionRequestForCorrections(t, http.MethodPatch,
		"/v1/alpr/detections/999", editDetectionRequest{Plate: "OCR"})
	c.SetParamNames("id")
	c.SetParamValues("999")
	if err := h.EditDetection(c); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestEditDetection_CrossDongleJWTReturns403(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()
	q.addDetection(7, "dongle-A", "r1", "OCR", k.Hash("OCR"))
	h := makeCorrectionsHandler(q, k, nil)

	// JWT-authed caller, dongle "dongle-B", targets a detection on
	// dongle-A. checkDongleAccess must 403.
	rec, c := newJWTRequestForCorrections(t, http.MethodPatch,
		"/v1/alpr/detections/7", "dongle-B", editDetectionRequest{Plate: "OCR"})
	c.SetParamNames("id")
	c.SetParamValues("7")
	if err := h.EditDetection(c); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestEditDetection_PropagatesNewHashThroughEncountersAndHints(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()

	oldHash := k.Hash("0CR123")
	newHash := k.Hash("OCR123")
	q.addDetection(7, "dongle-A", "r1", "0CR123", oldHash)
	q.addEncounter(11, "dongle-A", "r1", oldHash)
	// Pre-existing detection of "OCR123" on a different route -- this
	// should drive the merge hint.
	q.addDetection(8, "dongle-A", "r2", "OCR123", newHash)
	q.addWatchlist(newHash, "alerted", 3, "saw earlier", false)

	completions := make(chan worker.RouteAlprDetectionsComplete, 8)
	h := makeCorrectionsHandler(q, k, completions)

	rec, c := newSessionRequestForCorrections(t, http.MethodPatch,
		"/v1/alpr/detections/7", editDetectionRequest{Plate: "OCR123"})
	c.SetParamNames("id")
	c.SetParamValues("7")
	if err := h.EditDetection(c); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp editDetectionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The hint must mention the existing hash so the UI can offer a merge.
	if resp.Hint == "" {
		t.Errorf("hint empty when an existing watchlist row matches the new hash")
	}
	if resp.MatchHashB64 != base64.RawURLEncoding.EncodeToString(newHash) {
		t.Errorf("match_hash_b64 = %q, want %s",
			resp.MatchHashB64, base64.RawURLEncoding.EncodeToString(newHash))
	}

	// The detection's route AND the route the new hash already lives on
	// must both be enqueued so the heuristic re-evaluates across the
	// boundary.
	events := drainRoutes(completions)
	gotRoutes := map[string]bool{}
	for _, ev := range events {
		gotRoutes[ev.DongleID+"|"+ev.Route] = true
	}
	if !gotRoutes["dongle-A|r1"] {
		t.Errorf("re-trigger missing edited route r1; got %+v", events)
	}
	if !gotRoutes["dongle-A|r2"] {
		t.Errorf("re-trigger missing new-hash route r2; got %+v", events)
	}
}

// ----------------------------------------------------------------------
// POST /v1/alpr/plates/merge tests
// ----------------------------------------------------------------------

// thirtyTwoByteHash returns a deterministic 32-byte hash whose first
// byte is the supplied seed; the rest are zeros. Tests that only need
// distinct-but-stable hashes use this without dragging in the keyring.
func thirtyTwoByteHash(seed byte) []byte {
	out := make([]byte, 32)
	out[0] = seed
	return out
}

func TestMergePlates_ConsolidatesBothHistories(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()

	from := thirtyTwoByteHash(0xAA)
	to := thirtyTwoByteHash(0xBB)

	q.addDetection(1, "dongle-A", "r1", "FROMA", from)
	q.addDetection(2, "dongle-A", "r2", "FROMB", from)
	q.addDetection(3, "dongle-A", "r3", "TOA", to)
	q.addEncounter(10, "dongle-A", "r1", from)
	q.addEncounter(11, "dongle-A", "r3", to)

	// Both rows present, with severity 4 (higher) on `from` and a
	// notes string on each. Merge must preserve max severity, both
	// notes, etc.
	q.addWatchlist(from, "alerted", 4, "from-notes", false)
	q.addWatchlist(to, "alerted", 2, "to-notes", true)

	completions := make(chan worker.RouteAlprDetectionsComplete, 16)
	h := makeCorrectionsHandler(q, k, completions)

	rec, c := newSessionRequestForCorrections(t, http.MethodPost, "/v1/alpr/plates/merge",
		mergePlatesRequest{
			FromHashB64: base64.RawURLEncoding.EncodeToString(from),
			ToHashB64:   base64.RawURLEncoding.EncodeToString(to),
		})
	if err := h.MergePlates(c); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Detections rewritten.
	for _, id := range []int64{1, 2} {
		if string(q.detectionsByID[id].PlateHash) != string(to) {
			t.Errorf("detection %d hash = %x, want %x (to)", id, q.detectionsByID[id].PlateHash, to)
		}
	}
	if string(q.detectionsByID[3].PlateHash) != string(to) {
		t.Errorf("detection 3 (already on to) drifted")
	}
	// Encounters rewritten.
	if string(q.encountersByID[10].PlateHash) != string(to) {
		t.Errorf("encounter 10 not migrated to %x; got %x", to, q.encountersByID[10].PlateHash)
	}

	// Watchlist: source row removed, destination row keeps to.PlateHash
	// and absorbs from's metadata.
	if _, ok := q.watchlist[string(from)]; ok {
		t.Errorf("source watchlist row not deleted")
	}
	dst, ok := q.watchlist[string(to)]
	if !ok {
		t.Fatalf("destination watchlist row missing")
	}
	if dst.Severity.Int16 != 4 {
		t.Errorf("severity = %d, want 4 (max)", dst.Severity.Int16)
	}
	// One side was unacked (from): the merged ack must clear so the
	// alert resurfaces.
	if dst.AckedAt.Valid {
		t.Errorf("acked_at retained even though one side was unacked")
	}
	// Both notes present, joined.
	if !strings.Contains(dst.Notes.String, "from-notes") || !strings.Contains(dst.Notes.String, "to-notes") {
		t.Errorf("notes = %q, want both 'from-notes' and 'to-notes'", dst.Notes.String)
	}

	// Audit row has from + to hashes, no plate text.
	if q.auditCount("plate_merge") != 1 {
		t.Errorf("plate_merge audit count = %d, want 1", q.auditCount("plate_merge"))
	}
	pl := q.lastAuditPayload(t, "plate_merge")
	if pl["from_hash"] != base64.RawURLEncoding.EncodeToString(from) {
		t.Errorf("audit from_hash = %v, want %s", pl["from_hash"], base64.RawURLEncoding.EncodeToString(from))
	}
	if pl["to_hash"] != base64.RawURLEncoding.EncodeToString(to) {
		t.Errorf("audit to_hash = %v, want %s", pl["to_hash"], base64.RawURLEncoding.EncodeToString(to))
	}
	if pl["watchlist_path"] != "merged" {
		t.Errorf("audit watchlist_path = %v, want 'merged'", pl["watchlist_path"])
	}

	// Re-trigger: every (dongle_id, route) touched by either hash must
	// be enqueued.
	events := drainRoutes(completions)
	gotRoutes := map[string]bool{}
	for _, ev := range events {
		gotRoutes[ev.DongleID+"|"+ev.Route] = true
	}
	for _, want := range []string{"dongle-A|r1", "dongle-A|r2", "dongle-A|r3"} {
		if !gotRoutes[want] {
			t.Errorf("re-trigger missing route %s; got %+v", want, events)
		}
	}
}

func TestMergePlates_SourceOnlyWatchlistRowRenames(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()
	from := thirtyTwoByteHash(0x10)
	to := thirtyTwoByteHash(0x20)

	q.addDetection(1, "dongle-A", "r1", "FROM", from)
	q.addWatchlist(from, "alerted", 5, "important", false)

	h := makeCorrectionsHandler(q, k, make(chan worker.RouteAlprDetectionsComplete, 4))
	rec, c := newSessionRequestForCorrections(t, http.MethodPost, "/v1/alpr/plates/merge",
		mergePlatesRequest{
			FromHashB64: base64.RawURLEncoding.EncodeToString(from),
			ToHashB64:   base64.RawURLEncoding.EncodeToString(to),
		})
	if err := h.MergePlates(c); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Source row gone, destination row inherits the metadata.
	if _, ok := q.watchlist[string(from)]; ok {
		t.Errorf("source watchlist row not removed")
	}
	dst, ok := q.watchlist[string(to)]
	if !ok {
		t.Fatalf("renamed watchlist row missing at to-hash")
	}
	if dst.Severity.Int16 != 5 {
		t.Errorf("severity = %d, want 5 (preserved through rename)", dst.Severity.Int16)
	}
	if dst.Notes.String != "important" {
		t.Errorf("notes = %q, want 'important'", dst.Notes.String)
	}
	pl := q.lastAuditPayload(t, "plate_merge")
	if pl["watchlist_path"] != "renamed" {
		t.Errorf("audit watchlist_path = %v, want 'renamed'", pl["watchlist_path"])
	}
}

func TestMergePlates_NeitherHashExists404(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()
	h := makeCorrectionsHandler(q, k, nil)

	from := thirtyTwoByteHash(0x40)
	to := thirtyTwoByteHash(0x50)
	rec, c := newSessionRequestForCorrections(t, http.MethodPost, "/v1/alpr/plates/merge",
		mergePlatesRequest{
			FromHashB64: base64.RawURLEncoding.EncodeToString(from),
			ToHashB64:   base64.RawURLEncoding.EncodeToString(to),
		})
	if err := h.MergePlates(c); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestMergePlates_RejectsEqualHashes400(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()
	h := makeCorrectionsHandler(q, k, nil)

	hash := thirtyTwoByteHash(0x77)
	rec, c := newSessionRequestForCorrections(t, http.MethodPost, "/v1/alpr/plates/merge",
		mergePlatesRequest{
			FromHashB64: base64.RawURLEncoding.EncodeToString(hash),
			ToHashB64:   base64.RawURLEncoding.EncodeToString(hash),
		})
	if err := h.MergePlates(c); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestMergePlates_MalformedHashReturns400(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()
	h := makeCorrectionsHandler(q, k, nil)

	for _, tc := range []struct {
		name string
		from string
		to   string
	}{
		{"non-base64-from", "not!base64", base64.RawURLEncoding.EncodeToString(thirtyTwoByteHash(0xAA))},
		{"non-base64-to", base64.RawURLEncoding.EncodeToString(thirtyTwoByteHash(0xAA)), "@@@"},
		{"short-from", base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x02}), base64.RawURLEncoding.EncodeToString(thirtyTwoByteHash(0xBB))},
		{"short-to", base64.RawURLEncoding.EncodeToString(thirtyTwoByteHash(0xAA)), base64.RawURLEncoding.EncodeToString([]byte{0x01})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, c := newSessionRequestForCorrections(t, http.MethodPost, "/v1/alpr/plates/merge",
				mergePlatesRequest{FromHashB64: tc.from, ToHashB64: tc.to})
			if err := h.MergePlates(c); err != nil {
				t.Fatalf("merge: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestMergePlates_DestinationOnlyRowIsNoOp(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()
	from := thirtyTwoByteHash(0x60)
	to := thirtyTwoByteHash(0x70)

	q.addDetection(1, "dongle-A", "r1", "FROM", from)
	q.addWatchlist(to, "alerted", 3, "to-notes", false)

	h := makeCorrectionsHandler(q, k, make(chan worker.RouteAlprDetectionsComplete, 4))
	rec, c := newSessionRequestForCorrections(t, http.MethodPost, "/v1/alpr/plates/merge",
		mergePlatesRequest{
			FromHashB64: base64.RawURLEncoding.EncodeToString(from),
			ToHashB64:   base64.RawURLEncoding.EncodeToString(to),
		})
	if err := h.MergePlates(c); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Destination row unchanged.
	dst := q.watchlist[string(to)]
	if dst.Severity.Int16 != 3 || dst.Notes.String != "to-notes" {
		t.Errorf("destination row drifted: severity=%d notes=%q", dst.Severity.Int16, dst.Notes.String)
	}
	pl := q.lastAuditPayload(t, "plate_merge")
	if pl["watchlist_path"] != "to_only" {
		t.Errorf("audit watchlist_path = %v, want 'to_only'", pl["watchlist_path"])
	}
}

// ----------------------------------------------------------------------
// mergeWatchlistFields unit tests (pure function -- no DB dependency)
// ----------------------------------------------------------------------

func TestMergeWatchlistFields_MaxSeverityEarliestFirstLatestLast(t *testing.T) {
	from := db.GetWatchlistByHashRow{
		Severity:     pgtype.Int2{Int16: 4, Valid: true},
		FirstAlertAt: tsAt("2024-01-10T00:00:00Z"),
		LastAlertAt:  tsAt("2024-02-01T00:00:00Z"),
		Notes:        pgtype.Text{String: "old", Valid: true},
		AckedAt:      pgtype.Timestamptz{},
	}
	to := db.GetWatchlistByHashRow{
		Severity:     pgtype.Int2{Int16: 2, Valid: true},
		FirstAlertAt: tsAt("2024-01-15T00:00:00Z"),
		LastAlertAt:  tsAt("2024-01-30T00:00:00Z"),
		Notes:        pgtype.Text{String: "new", Valid: true},
		AckedAt:      tsAt("2024-02-02T00:00:00Z"),
	}
	got := mergeWatchlistFields(from, to)

	if got.severity.Int16 != 4 {
		t.Errorf("severity = %d, want 4", got.severity.Int16)
	}
	if !got.firstAlertAt.Valid || got.firstAlertAt.Time.Format("2006-01-02") != "2024-01-10" {
		t.Errorf("firstAlertAt = %v, want 2024-01-10", got.firstAlertAt.Time)
	}
	if !got.lastAlertAt.Valid || got.lastAlertAt.Time.Format("2006-01-02") != "2024-02-01" {
		t.Errorf("lastAlertAt = %v, want 2024-02-01", got.lastAlertAt.Time)
	}
	// One side unacked -> ack cleared.
	if got.ackedAt.Valid {
		t.Errorf("ackedAt retained when one side was unacked")
	}
	if !strings.Contains(got.notes.String, "old") || !strings.Contains(got.notes.String, "new") {
		t.Errorf("notes = %q, want both 'old' and 'new'", got.notes.String)
	}
}

func TestMergeWatchlistFields_BothAckedKeepsLatest(t *testing.T) {
	from := db.GetWatchlistByHashRow{AckedAt: tsAt("2024-01-01T00:00:00Z")}
	to := db.GetWatchlistByHashRow{AckedAt: tsAt("2024-02-01T00:00:00Z")}
	got := mergeWatchlistFields(from, to)
	if !got.ackedAt.Valid {
		t.Fatalf("ackedAt invalid when both acked")
	}
	if got.ackedAt.Time.Format("2006-01-02") != "2024-02-01" {
		t.Errorf("ackedAt = %v, want 2024-02-01 (latest)", got.ackedAt.Time)
	}
}

// tsAt parses an RFC3339 timestamp into a pgtype.Timestamptz. Test
// helper to keep the merge-rule tests readable.
func tsAt(s string) pgtype.Timestamptz {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// ----------------------------------------------------------------------
// Sanity assertion: id parameter must be a positive integer
// ----------------------------------------------------------------------

func TestEditDetection_IDMustBePositiveInt(t *testing.T) {
	q := newFakeCorrectionsQuerier()
	k := newFakeCorrectionsKeyring()
	h := makeCorrectionsHandler(q, k, nil)

	for _, id := range []string{"abc", "0", "-1"} {
		rec, c := newSessionRequestForCorrections(t, http.MethodPatch,
			"/v1/alpr/detections/"+id, editDetectionRequest{Plate: "OCR"})
		c.SetParamNames("id")
		c.SetParamValues(id)
		if err := h.EditDetection(c); err != nil {
			t.Fatalf("edit (id=%q): %v", id, err)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("id=%q status = %d, want 400", id, rec.Code)
		}
	}
}
