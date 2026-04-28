package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// fakeKeyring implements plateKeyring for tests. It maps each ciphertext
// (compared by string equality of the bytes) to a fixed plaintext, with
// a programmable error injection for negative tests.
type fakeKeyring struct {
	plates map[string]string
	labels map[string]string
	err    error
}

func newFakeKeyring() *fakeKeyring {
	return &fakeKeyring{
		plates: make(map[string]string),
		labels: make(map[string]string),
	}
}

func (k *fakeKeyring) Decrypt(ciphertext []byte) (string, error) {
	if k.err != nil {
		return "", k.err
	}
	if v, ok := k.plates[string(ciphertext)]; ok {
		return v, nil
	}
	return "", errors.New("fake: unknown ciphertext")
}

func (k *fakeKeyring) DecryptLabel(ciphertext []byte) (string, error) {
	if k.err != nil {
		return "", k.err
	}
	if v, ok := k.labels[string(ciphertext)]; ok {
		return v, nil
	}
	return "", errors.New("fake: unknown label ciphertext")
}

// fakeEncountersQuerier implements alprEncountersQuerier with simple
// in-memory state. Each table is a slice or map keyed by the natural
// identifier so tests can populate exactly what they need.
type fakeEncountersQuerier struct {
	routes              map[string]db.Route // key: dongle|route_name
	routesByID          map[int32]db.Route  // key: id
	encountersByRoute   map[string][]db.PlateEncounter
	encountersByHash    map[string][]db.PlateEncounter
	detectionsByRoute   map[string][]db.ListDetectionsForRouteRow
	watchlistByHash     map[string]db.GetWatchlistByHashRow
	signaturesByID      map[int64]db.VehicleSignature
	tripsByRouteID      map[int32]db.Trip
	getRouteErr         error
	listEncountersErr   error
	listDetectionsErr   error
	listEncountersHashE error
}

func newFakeEncountersQuerier() *fakeEncountersQuerier {
	return &fakeEncountersQuerier{
		routes:            make(map[string]db.Route),
		routesByID:        make(map[int32]db.Route),
		encountersByRoute: make(map[string][]db.PlateEncounter),
		encountersByHash:  make(map[string][]db.PlateEncounter),
		detectionsByRoute: make(map[string][]db.ListDetectionsForRouteRow),
		watchlistByHash:   make(map[string]db.GetWatchlistByHashRow),
		signaturesByID:    make(map[int64]db.VehicleSignature),
		tripsByRouteID:    make(map[int32]db.Trip),
	}
}

func encountersRouteKey(dongle, route string) string { return dongle + "|" + route }

func (f *fakeEncountersQuerier) GetRoute(_ context.Context, arg db.GetRouteParams) (db.Route, error) {
	if f.getRouteErr != nil {
		return db.Route{}, f.getRouteErr
	}
	r, ok := f.routes[encountersRouteKey(arg.DongleID, arg.RouteName)]
	if !ok {
		return db.Route{}, pgx.ErrNoRows
	}
	return r, nil
}

func (f *fakeEncountersQuerier) GetRouteByID(_ context.Context, id int32) (db.Route, error) {
	r, ok := f.routesByID[id]
	if !ok {
		return db.Route{}, pgx.ErrNoRows
	}
	return r, nil
}

func (f *fakeEncountersQuerier) ListEncountersForRoute(_ context.Context, arg db.ListEncountersForRouteParams) ([]db.PlateEncounter, error) {
	if f.listEncountersErr != nil {
		return nil, f.listEncountersErr
	}
	return f.encountersByRoute[encountersRouteKey(arg.DongleID, arg.Route)], nil
}

func (f *fakeEncountersQuerier) ListEncountersForPlate(_ context.Context, plateHash []byte) ([]db.PlateEncounter, error) {
	if f.listEncountersHashE != nil {
		return nil, f.listEncountersHashE
	}
	return f.encountersByHash[string(plateHash)], nil
}

func (f *fakeEncountersQuerier) ListDetectionsForRoute(_ context.Context, arg db.ListDetectionsForRouteParams) ([]db.ListDetectionsForRouteRow, error) {
	if f.listDetectionsErr != nil {
		return nil, f.listDetectionsErr
	}
	return f.detectionsByRoute[encountersRouteKey(arg.DongleID, arg.Route)], nil
}

func (f *fakeEncountersQuerier) GetWatchlistByHash(_ context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error) {
	v, ok := f.watchlistByHash[string(plateHash)]
	if !ok {
		return db.GetWatchlistByHashRow{}, pgx.ErrNoRows
	}
	return v, nil
}

func (f *fakeEncountersQuerier) GetSignature(_ context.Context, id int64) (db.VehicleSignature, error) {
	v, ok := f.signaturesByID[id]
	if !ok {
		return db.VehicleSignature{}, pgx.ErrNoRows
	}
	return v, nil
}

func (f *fakeEncountersQuerier) GetTripByRouteID(_ context.Context, routeID int32) (db.Trip, error) {
	v, ok := f.tripsByRouteID[routeID]
	if !ok {
		return db.Trip{}, pgx.ErrNoRows
	}
	return v, nil
}

// stubEnvelope is a deterministic alprEnvelope for tests.
type stubEnvelope struct {
	enabled bool
	hasKey  bool
	err     error
}

func (s stubEnvelope) Enabled(_ context.Context) (bool, error) { return s.enabled, s.err }
func (s stubEnvelope) KeyringConfigured() bool                 { return s.hasKey }

// timestamptz wraps a time as a valid pgtype.Timestamptz so tests can
// build encounter rows with concise expressions.
func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// makeEncounter builds a PlateEncounter row with sensible defaults so
// tests only override fields that matter to them.
func makeEncounter(id int64, dongle, route string, plateHash []byte, firstSeen, lastSeen time.Time) db.PlateEncounter {
	return db.PlateEncounter{
		ID:             id,
		DongleID:       dongle,
		Route:          route,
		PlateHash:      plateHash,
		FirstSeenTs:    timestamptz(firstSeen),
		LastSeenTs:     timestamptz(lastSeen),
		DetectionCount: 1,
		TurnCount:      0,
		Status:         "ok",
		BboxFirst:      []byte(`{"x":1,"y":2,"w":3,"h":4}`),
		BboxLast:       []byte(`{"x":5,"y":6,"w":7,"h":8}`),
	}
}

func makeDetection(id int64, dongle, route string, plateHash, ciphertext []byte, frameTs time.Time) db.ListDetectionsForRouteRow {
	return db.ListDetectionsForRouteRow{
		ID:              id,
		DongleID:        dongle,
		Route:           route,
		PlateCiphertext: ciphertext,
		PlateHash:       plateHash,
		Bbox:            []byte(`{"x":10,"y":20,"w":30,"h":40}`),
		Confidence:      0.9,
		FrameTs:         timestamptz(frameTs),
	}
}

// newRouteEncountersRequest builds an Echo context for GET /v1/routes/:dongle/:route/plates
// pre-populated with the given dongle id under the JWT auth-mode key
// (so checkDongleAccess sees a JWT-authenticated caller).
func newRouteEncountersRequest(t *testing.T, dongle, route, authDongle string) (*httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/routes/%s/%s/plates", dongle, route), nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues(dongle, route)
	c.Set(middleware.ContextKeyDongleID, authDongle)
	return rec, c
}

func newPlateDetailRequest(t *testing.T, hashB64 string, query string) (*httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	e := echo.New()
	target := "/v1/plates/" + hashB64
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("hash_b64")
	c.SetParamValues(hashB64)
	c.Set(middleware.ContextKeyAuthMode, middleware.AuthModeSession)
	c.Set(middleware.ContextKeyUserID, int32(1))
	return rec, c
}

// makeRouteEncountersHandler wires a fresh handler against the supplied
// fakes. Skipping the requireAlprEnabled wrapper so unit tests focus on
// the handler body.
func makeRouteEncountersHandler(q alprEncountersQuerier, k plateKeyring) *ALPREncountersHandler {
	h := NewALPREncountersHandler(q, settings.New(newALPRFakeQuerier()), k)
	return h
}

func TestRouteEncounters_HappyPath_MixedAlertedUnalerted(t *testing.T) {
	const dongle = "d-1"
	const route = "2026-04-01--10-00-00"
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)

	hashAlerted := []byte{0x01, 0x02, 0x03, 0x04}
	hashClean := []byte{0xaa, 0xbb, 0xcc, 0xdd}
	cipherAlerted := []byte("CIPHER-ALERTED")
	cipherClean := []byte("CIPHER-CLEAN")

	q := newFakeEncountersQuerier()
	q.routes[encountersRouteKey(dongle, route)] = db.Route{
		ID:        7,
		DongleID:  dongle,
		RouteName: route,
		StartTime: timestamptz(now),
	}
	q.encountersByRoute[encountersRouteKey(dongle, route)] = []db.PlateEncounter{
		func() db.PlateEncounter {
			e := makeEncounter(101, dongle, route, hashAlerted, now, now.Add(10*time.Second))
			e.DetectionCount = 12
			e.TurnCount = 3
			e.SignatureID = pgtype.Int8{Int64: 555, Valid: true}
			return e
		}(),
		func() db.PlateEncounter {
			e := makeEncounter(102, dongle, route, hashClean, now.Add(20*time.Second), now.Add(40*time.Second))
			e.DetectionCount = 4
			e.TurnCount = 1
			return e
		}(),
	}
	q.detectionsByRoute[encountersRouteKey(dongle, route)] = []db.ListDetectionsForRouteRow{
		func() db.ListDetectionsForRouteRow {
			d := makeDetection(1, dongle, route, hashAlerted, cipherAlerted, now)
			d.ThumbPath = pgtype.Text{String: "thumbs/d-1/route/0/1.jpg", Valid: true}
			return d
		}(),
		makeDetection(2, dongle, route, hashClean, cipherClean, now.Add(20*time.Second)),
	}
	q.watchlistByHash[string(hashAlerted)] = db.GetWatchlistByHashRow{
		ID:        9,
		PlateHash: hashAlerted,
		Kind:      "alerted",
		Severity:  pgtype.Int2{Int16: 4, Valid: true},
	}
	q.signaturesByID[555] = db.VehicleSignature{
		ID:           555,
		SignatureKey: "toyota|camry|silver|sedan",
		Make:         pgtype.Text{String: "Toyota", Valid: true},
		Model:        pgtype.Text{String: "Camry", Valid: true},
		Color:        pgtype.Text{String: "silver", Valid: true},
		BodyType:     pgtype.Text{String: "sedan", Valid: true},
		Confidence:   pgtype.Float4{Float32: 0.85, Valid: true},
	}

	k := newFakeKeyring()
	k.plates[string(cipherAlerted)] = "ABC123"
	k.plates[string(cipherClean)] = "ZYX987"

	h := makeRouteEncountersHandler(q, k)
	rec, c := newRouteEncountersRequest(t, dongle, route, dongle)
	if err := h.GetRouteEncounters(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body routeEncountersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rec.Body.String())
	}
	if len(body.Encounters) != 2 {
		t.Fatalf("len(encounters) = %d, want 2", len(body.Encounters))
	}

	alerted := body.Encounters[0]
	if alerted.Plate != "ABC123" {
		t.Errorf("first encounter plate = %q, want ABC123", alerted.Plate)
	}
	if alerted.PlateHashB64 != base64.RawURLEncoding.EncodeToString(hashAlerted) {
		t.Errorf("first encounter plate_hash_b64 = %q, want %q",
			alerted.PlateHashB64, base64.RawURLEncoding.EncodeToString(hashAlerted))
	}
	if alerted.DetectionCount != 12 {
		t.Errorf("first encounter detection_count = %d, want 12", alerted.DetectionCount)
	}
	if alerted.TurnCount != 3 {
		t.Errorf("first encounter turn_count = %d, want 3", alerted.TurnCount)
	}
	if alerted.SeverityIfAlerted == nil || *alerted.SeverityIfAlerted != 4 {
		t.Errorf("first encounter severity_if_alerted = %v, want 4", alerted.SeverityIfAlerted)
	}
	if alerted.AckStatus == nil || *alerted.AckStatus != "open" {
		t.Errorf("first encounter ack_status = %v, want open", alerted.AckStatus)
	}
	if alerted.Signature == nil || alerted.Signature.Make != "Toyota" || alerted.Signature.Model != "Camry" {
		t.Errorf("first encounter signature = %+v, want Toyota Camry", alerted.Signature)
	}
	if alerted.BboxFirst == nil || alerted.BboxFirst.X != 1 || alerted.BboxFirst.W != 3 {
		t.Errorf("first encounter bbox_first = %+v, want {1,2,3,4}", alerted.BboxFirst)
	}
	if alerted.SampleThumbURL == nil || *alerted.SampleThumbURL != "/v1/alpr/detections/1/thumbnail" {
		t.Errorf("first encounter sample_thumb_url = %v, want /v1/alpr/detections/1/thumbnail",
			alerted.SampleThumbURL)
	}

	clean := body.Encounters[1]
	if clean.Plate != "ZYX987" {
		t.Errorf("second encounter plate = %q, want ZYX987", clean.Plate)
	}
	if clean.SeverityIfAlerted != nil {
		t.Errorf("second encounter severity_if_alerted = %v, want nil (no watchlist row)", *clean.SeverityIfAlerted)
	}
	if clean.AckStatus != nil {
		t.Errorf("second encounter ack_status = %v, want nil", *clean.AckStatus)
	}
	if clean.Signature != nil {
		t.Errorf("second encounter signature = %+v, want nil (no signature_id)", clean.Signature)
	}
	if clean.SampleThumbURL != nil {
		t.Errorf("second encounter sample_thumb_url = %v, want nil (no thumb)", *clean.SampleThumbURL)
	}
}

func TestRouteEncounters_MissingSignaturePath(t *testing.T) {
	// Encounter has signature_id NOT NULL but the signature row is
	// missing (e.g. deleted by a retention sweep on the signatures
	// table while the encounter still references it). The handler
	// should NOT 500 -- it should drop the signature field but keep
	// the rest of the encounter.
	const dongle = "d-1"
	const route = "2026-04-01--11-00-00"
	now := time.Date(2026, 4, 1, 11, 0, 0, 0, time.UTC)

	hash := []byte{0x10, 0x20}
	cipher := []byte("CIPHER-X")

	q := newFakeEncountersQuerier()
	q.routes[encountersRouteKey(dongle, route)] = db.Route{
		ID: 1, DongleID: dongle, RouteName: route, StartTime: timestamptz(now),
	}
	q.encountersByRoute[encountersRouteKey(dongle, route)] = []db.PlateEncounter{
		func() db.PlateEncounter {
			e := makeEncounter(1, dongle, route, hash, now, now.Add(time.Second))
			e.SignatureID = pgtype.Int8{Int64: 999, Valid: true} // dangling FK
			return e
		}(),
	}
	q.detectionsByRoute[encountersRouteKey(dongle, route)] = []db.ListDetectionsForRouteRow{
		makeDetection(1, dongle, route, hash, cipher, now),
	}
	// signaturesByID intentionally empty -- 999 not found

	k := newFakeKeyring()
	k.plates[string(cipher)] = "QED111"

	h := makeRouteEncountersHandler(q, k)
	rec, c := newRouteEncountersRequest(t, dongle, route, dongle)
	if err := h.GetRouteEncounters(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body routeEncountersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Encounters) != 1 {
		t.Fatalf("len(encounters) = %d, want 1", len(body.Encounters))
	}
	if body.Encounters[0].Plate != "QED111" {
		t.Errorf("plate = %q, want QED111", body.Encounters[0].Plate)
	}
	if body.Encounters[0].Signature != nil {
		t.Errorf("signature = %+v, want nil (FK dangling)", body.Encounters[0].Signature)
	}
}

func TestRouteEncounters_RouteNotFound(t *testing.T) {
	q := newFakeEncountersQuerier()
	h := makeRouteEncountersHandler(q, newFakeKeyring())
	rec, c := newRouteEncountersRequest(t, "d-1", "no-such-route", "d-1")
	if err := h.GetRouteEncounters(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRouteEncounters_DongleAccessForbidden(t *testing.T) {
	const owner = "owner"
	const route = "2026-04-01--12-00-00"
	q := newFakeEncountersQuerier()
	q.routes[encountersRouteKey(owner, route)] = db.Route{ID: 1, DongleID: owner, RouteName: route}

	h := makeRouteEncountersHandler(q, newFakeKeyring())
	// JWT-auth caller targets a different dongle
	rec, c := newRouteEncountersRequest(t, owner, route, "attacker")
	if err := h.GetRouteEncounters(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestPlateDetail_HappyPath_DecryptsAndComputesStats(t *testing.T) {
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)

	hash := []byte{0xa1, 0xb2, 0xc3, 0xd4}
	cipher := []byte("CIPHER-MAIN")
	hashB64 := base64.RawURLEncoding.EncodeToString(hash)

	q := newFakeEncountersQuerier()
	q.encountersByHash[string(hash)] = []db.PlateEncounter{
		func() db.PlateEncounter {
			e := makeEncounter(1, "d-1", "route-A", hash, now, now.Add(time.Minute))
			e.DetectionCount = 5
			e.TurnCount = 1
			e.SignatureID = pgtype.Int8{Int64: 7, Valid: true}
			return e
		}(),
		func() db.PlateEncounter {
			e := makeEncounter(2, "d-1", "route-B", hash, now.Add(-2*24*time.Hour), now.Add(-2*24*time.Hour+time.Minute))
			e.DetectionCount = 3
			e.SignatureID = pgtype.Int8{Int64: 7, Valid: true}
			return e
		}(),
		func() db.PlateEncounter {
			e := makeEncounter(3, "d-1", "route-C", hash, now.Add(-60*24*time.Hour), now.Add(-60*24*time.Hour+time.Minute))
			e.DetectionCount = 2
			return e
		}(),
	}
	// Detections for route-A so the plate decryption succeeds.
	q.routes[encountersRouteKey("d-1", "route-A")] = db.Route{
		ID: 1, DongleID: "d-1", RouteName: "route-A", StartTime: timestamptz(now),
	}
	q.detectionsByRoute[encountersRouteKey("d-1", "route-A")] = []db.ListDetectionsForRouteRow{
		makeDetection(1, "d-1", "route-A", hash, cipher, now),
	}
	q.tripsByRouteID[1] = db.Trip{
		ID:           1,
		RouteID:      1,
		StartAddress: pgtype.Text{String: "Downtown", Valid: true},
	}
	q.watchlistByHash[string(hash)] = db.GetWatchlistByHashRow{
		ID:        99,
		PlateHash: hash,
		Kind:      "alerted",
		Severity:  pgtype.Int2{Int16: 4, Valid: true},
		AckedAt:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
	}
	q.signaturesByID[7] = db.VehicleSignature{
		ID:    7,
		Make:  pgtype.Text{String: "Toyota", Valid: true},
		Model: pgtype.Text{String: "Camry", Valid: true},
	}

	k := newFakeKeyring()
	k.plates[string(cipher)] = "ABC123"

	h := makeRouteEncountersHandler(q, k)
	rec, c := newPlateDetailRequest(t, hashB64, "")
	if err := h.GetPlateDetail(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body plateDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Plate != "ABC123" {
		t.Errorf("plate = %q, want ABC123", body.Plate)
	}
	if body.PlateHashB64 != hashB64 {
		t.Errorf("plate_hash_b64 = %q, want %q", body.PlateHashB64, hashB64)
	}
	if body.WatchlistStatus == nil || body.WatchlistStatus.Kind != "alerted" {
		t.Errorf("watchlist_status = %+v, want kind=alerted", body.WatchlistStatus)
	}
	if body.WatchlistStatus == nil || body.WatchlistStatus.Severity == nil || *body.WatchlistStatus.Severity != 4 {
		t.Errorf("watchlist_status.severity = %+v, want 4", body.WatchlistStatus)
	}
	if body.WatchlistStatus == nil || body.WatchlistStatus.AckedAt == nil {
		t.Errorf("watchlist_status.acked_at = nil, want set")
	}
	if body.Signature == nil || body.Signature.Make != "Toyota" {
		t.Errorf("signature = %+v, want Toyota", body.Signature)
	}
	if len(body.Encounters) != 3 {
		t.Errorf("len(encounters) = %d, want 3", len(body.Encounters))
	}
	if body.Stats.TotalDetections != 10 {
		t.Errorf("total_detections = %d, want 10", body.Stats.TotalDetections)
	}
	// Two encounters within the 30d window -> distinct_routes_30d = 2.
	if body.Stats.DistinctRoutes30d != 2 {
		t.Errorf("distinct_routes_30d = %d, want 2", body.Stats.DistinctRoutes30d)
	}
	// First-ever-seen should be the oldest encounter (route-C, 60d ago).
	wantFirst := now.Add(-60 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if body.Stats.FirstEverSeen != wantFirst {
		t.Errorf("first_ever_seen = %q, want %q", body.Stats.FirstEverSeen, wantFirst)
	}

	// area_cluster_label uses trip.start_address when present.
	if len(body.Encounters) > 0 && body.Encounters[0].AreaClusterLabel != "Downtown" {
		t.Errorf("encounters[0].area_cluster_label = %q, want Downtown",
			body.Encounters[0].AreaClusterLabel)
	}
}

func TestPlateDetail_PaginationCorrectness(t *testing.T) {
	hash := []byte{0xff, 0xee, 0xdd, 0xcc}
	hashB64 := base64.RawURLEncoding.EncodeToString(hash)
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	q := newFakeEncountersQuerier()
	// 100 encounters, newest first by last_seen_ts.
	encounters := make([]db.PlateEncounter, 100)
	for i := range encounters {
		ts := now.Add(time.Duration(-i) * time.Hour)
		encounters[i] = makeEncounter(int64(i+1), "d-1", fmt.Sprintf("route-%03d", i), hash, ts, ts.Add(time.Minute))
		encounters[i].DetectionCount = 1
	}
	q.encountersByHash[string(hash)] = encounters

	// No detections / signatures / trips -- the handler should still
	// page the encounters[] slice correctly without those.
	k := newFakeKeyring()

	h := makeRouteEncountersHandler(q, k)
	rec, c := newPlateDetailRequest(t, hashB64, "limit=25&offset=50")
	if err := h.GetPlateDetail(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body plateDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Encounters) != 25 {
		t.Fatalf("len(encounters) = %d, want 25", len(body.Encounters))
	}
	// First encounter on this page should be the 51st row of the full
	// list (offset=50, 0-indexed), i.e. encounter id 51.
	first := body.Encounters[0]
	if first.Route != "route-050" {
		t.Errorf("encounters[0].route = %q, want route-050", first.Route)
	}
	last := body.Encounters[24]
	if last.Route != "route-074" {
		t.Errorf("encounters[24].route = %q, want route-074", last.Route)
	}
	// stats.total_detections is computed across ALL 100 rows, not just
	// the page -- 100 rows with detection_count=1 each.
	if body.Stats.TotalDetections != 100 {
		t.Errorf("total_detections = %d, want 100 (full set)", body.Stats.TotalDetections)
	}
}

func TestPlateDetail_404OnUnknownHash(t *testing.T) {
	hash := []byte{0xde, 0xad}
	hashB64 := base64.RawURLEncoding.EncodeToString(hash)
	q := newFakeEncountersQuerier() // empty
	h := makeRouteEncountersHandler(q, newFakeKeyring())
	rec, c := newPlateDetailRequest(t, hashB64, "")
	if err := h.GetPlateDetail(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPlateDetail_400OnMalformedHash(t *testing.T) {
	q := newFakeEncountersQuerier()
	h := makeRouteEncountersHandler(q, newFakeKeyring())
	// "!!!" is not valid base64 in any encoding the handler accepts.
	rec, c := newPlateDetailRequest(t, "!!!", "")
	if err := h.GetPlateDetail(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPlateDetail_400OnBadPagination(t *testing.T) {
	hash := []byte{0x01, 0x02}
	hashB64 := base64.RawURLEncoding.EncodeToString(hash)
	q := newFakeEncountersQuerier()
	q.encountersByHash[string(hash)] = []db.PlateEncounter{
		makeEncounter(1, "d-1", "r-1", hash, time.Now(), time.Now()),
	}
	h := makeRouteEncountersHandler(q, newFakeKeyring())

	cases := []string{
		"limit=0",
		"limit=999",
		"limit=abc",
		"offset=-1",
		"offset=xyz",
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			rec, c := newPlateDetailRequest(t, hashB64, q)
			if err := h.GetPlateDetail(c); err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRequireAlprEnabled_503WhenDisabled(t *testing.T) {
	env := stubEnvelope{enabled: false, hasKey: true}
	mw := requireAlprEnabled(env)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	called := false
	handler := mw(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})
	if err := handler(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if called {
		t.Errorf("inner handler ran when alpr_enabled=false; should have short-circuited")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	var body alprDisabledResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rec.Body.String())
	}
	if body.Error != "alpr_disabled" {
		t.Errorf("body.error = %q, want alpr_disabled", body.Error)
	}
	if body.Detail == "" {
		t.Errorf("body.detail is empty; expected human-readable hint")
	}
}

func TestRequireAlprEnabled_503WhenKeyringMissing(t *testing.T) {
	env := stubEnvelope{enabled: true, hasKey: false}
	mw := requireAlprEnabled(env)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	called := false
	handler := mw(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})
	if err := handler(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if called {
		t.Errorf("inner handler ran when keyring missing; should have short-circuited")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestRequireAlprEnabled_PassesWhenEnabledAndKeyed(t *testing.T) {
	env := stubEnvelope{enabled: true, hasKey: true}
	mw := requireAlprEnabled(env)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	called := false
	handler := mw(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusNoContent)
	})
	if err := handler(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !called {
		t.Errorf("inner handler did not run; gate should have allowed the request")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestDecodePlateHashB64_AcceptsMultipleEncodings(t *testing.T) {
	raw := []byte{0xab, 0xcd, 0xef, 0x01, 0x02, 0x03}
	cases := map[string]string{
		"raw-url": base64.RawURLEncoding.EncodeToString(raw),
		"url":     base64.URLEncoding.EncodeToString(raw),
		"raw-std": base64.RawStdEncoding.EncodeToString(raw),
		"std":     base64.StdEncoding.EncodeToString(raw),
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := decodePlateHashB64(encoded)
			if err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if string(got) != string(raw) {
				t.Errorf("decoded = %x, want %x", got, raw)
			}
		})
	}
}
