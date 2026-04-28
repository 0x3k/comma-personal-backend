package heuristic

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
)

// fakeFusionDeps is the in-memory FusionDeps used by the fusion-layer
// tests. Each method returns the configured slice/row exactly so a
// test can author the precise scenario it cares about (a 1M-row
// production DB is the wrong place to assert "share_pct >= 0.80").
type fakeFusionDeps struct {
	// detectionsByPlate -> rows returned by
	// CountDetectionsBySignatureForPlate.
	detectionsByPlate map[string][]db.CountDetectionsBySignatureForPlateRow

	// signaturesByID -> row returned by GetSignature. Missing keys
	// return pgx.ErrNoRows.
	signaturesByID map[int64]db.VehicleSignature

	// platesForSignatureInWindow -> rows returned by
	// ListPlateHashesForSignatureInWindow keyed on the signature_id.
	platesForSignatureInWindow map[int64][]db.ListPlateHashesForSignatureInWindowRow

	// watchlistByPlate -> row returned by GetWatchlistByHash. Missing
	// keys return pgx.ErrNoRows.
	watchlistByPlate map[string]db.GetWatchlistByHashRow

	// failure injection
	countErr error
	listErr  error
}

func newFakeFusionDeps() *fakeFusionDeps {
	return &fakeFusionDeps{
		detectionsByPlate:          make(map[string][]db.CountDetectionsBySignatureForPlateRow),
		signaturesByID:             make(map[int64]db.VehicleSignature),
		platesForSignatureInWindow: make(map[int64][]db.ListPlateHashesForSignatureInWindowRow),
		watchlistByPlate:           make(map[string]db.GetWatchlistByHashRow),
	}
}

func (f *fakeFusionDeps) CountDetectionsBySignatureForPlate(_ context.Context, plateHash []byte) ([]db.CountDetectionsBySignatureForPlateRow, error) {
	if f.countErr != nil {
		return nil, f.countErr
	}
	return f.detectionsByPlate[string(plateHash)], nil
}

func (f *fakeFusionDeps) GetSignature(_ context.Context, id int64) (db.VehicleSignature, error) {
	row, ok := f.signaturesByID[id]
	if !ok {
		return db.VehicleSignature{}, pgx.ErrNoRows
	}
	return row, nil
}

func (f *fakeFusionDeps) ListPlatesForSignature(_ context.Context, signatureID pgtype.Int8) ([][]byte, error) {
	rows := f.platesForSignatureInWindow[signatureID.Int64]
	seen := make(map[string]struct{}, len(rows))
	out := make([][]byte, 0, len(rows))
	for _, r := range rows {
		k := string(r.PlateHash)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, append([]byte(nil), r.PlateHash...))
	}
	return out, nil
}

func (f *fakeFusionDeps) ListPlateHashesForSignatureInWindow(_ context.Context, arg db.ListPlateHashesForSignatureInWindowParams) ([]db.ListPlateHashesForSignatureInWindowRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	rows := f.platesForSignatureInWindow[arg.SignatureID.Int64]
	out := make([]db.ListPlateHashesForSignatureInWindowRow, 0, len(rows))
	for _, r := range rows {
		// Window filter mirrors the SQL: last_seen_ts >= window_start
		// AND first_seen_ts <= window_end. The fake stores last_seen
		// only because the heuristic uses it for "most recent"; for
		// the window filter we treat last_seen as both bounds.
		if r.LastSeenTs.Valid {
			if r.LastSeenTs.Time.Before(arg.WindowStart.Time) {
				continue
			}
			if r.LastSeenTs.Time.After(arg.WindowEnd.Time) {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeFusionDeps) GetWatchlistByHash(_ context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error) {
	row, ok := f.watchlistByPlate[string(plateHash)]
	if !ok {
		return db.GetWatchlistByHashRow{}, pgx.ErrNoRows
	}
	return row, nil
}

// seedDetections records (signature_id, count) pairs for the plate. A
// nil signature_id (sigID == 0) records the missing-signature row.
func (f *fakeFusionDeps) seedDetections(plateHash []byte, sigID int64, count int64) {
	row := db.CountDetectionsBySignatureForPlateRow{
		DetectionCount: count,
	}
	if sigID > 0 {
		row.SignatureID = pgtype.Int8{Int64: sigID, Valid: true}
	}
	f.detectionsByPlate[string(plateHash)] = append(f.detectionsByPlate[string(plateHash)], row)
}

// seedPlatesForSignatureInArea records one synthetic encounter row at
// the supplied lat/lng cell. The fusion test cell is hardcoded at 5km
// matching DefaultPlateSwapAreaCellKm; tests that need a different
// cell must seed the row directly.
func (f *fakeFusionDeps) seedPlatesForSignatureInArea(sigID int64, plateHash []byte, cellLat, cellLng int64, gpsLat, gpsLng float64, last time.Time) {
	row := db.ListPlateHashesForSignatureInWindowRow{
		PlateHash:   append([]byte(nil), plateHash...),
		CellLat:     cellLat,
		CellLng:     cellLng,
		EncounterID: int64(len(f.platesForSignatureInWindow[sigID]) + 1),
		LastSeenTs:  pgtype.Timestamptz{Time: last, Valid: true},
		GpsLat:      pgtype.Float8{Float64: gpsLat, Valid: true},
		GpsLng:      pgtype.Float8{Float64: gpsLng, Valid: true},
	}
	f.platesForSignatureInWindow[sigID] = append(f.platesForSignatureInWindow[sigID], row)
}

func (f *fakeFusionDeps) seedSignature(id int64, key string) {
	f.signaturesByID[id] = db.VehicleSignature{
		ID:           id,
		SignatureKey: key,
		SampleCount:  10,
	}
}

func (f *fakeFusionDeps) seedWhitelist(plateHash []byte) {
	f.watchlistByPlate[string(plateHash)] = db.GetWatchlistByHashRow{
		PlateHash: append([]byte(nil), plateHash...),
		Kind:      "whitelist",
	}
}

// fusionBaseTime fixes the wall-clock for tests so window filtering is
// deterministic.
var fusionBaseTime = time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)

// findFusionComponent looks up a component by name in the fusion
// result.
func findFusionComponent(r FusionResult, name string) (Component, bool) {
	for _, c := range r.Components {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}

// --- (1) plate-confirmation: signature_consistent ---

func TestFuseSignatures_SignatureConsistent_RaisesSeverityBy0_5(t *testing.T) {
	deps := newFakeFusionDeps()
	plate := []byte{0x01}
	// 9 detections under sig 42, 1 under nothing. 9/9 (signature-bearing)
	// = 1.0 share, well above 80%.
	deps.seedDetections(plate, 42, 9)
	deps.seedDetections(plate, 0, 1) // NULL signature_id, ignored in denom
	deps.seedSignature(42, "toyota|camry|silver|sedan")

	got, err := FuseSignatures(context.Background(), deps, FusionInput{
		PlateHash:  plate,
		Now:        fusionBaseTime,
		Thresholds: DefaultFusionThresholds(),
	})
	if err != nil {
		t.Fatalf("FuseSignatures: %v", err)
	}
	if got.ExtraSeverity != 0.5 {
		t.Fatalf("extra severity: got=%v want=0.5", got.ExtraSeverity)
	}
	c, ok := findFusionComponent(got, ComponentSignatureConsistent)
	if !ok {
		t.Fatalf("missing signature_consistent component (got=%+v)", got.Components)
	}
	if c.Points != 0.5 {
		t.Fatalf("points: got=%v want=0.5", c.Points)
	}
	if got, _ := c.Evidence["share_pct"].(float64); got < 0.80 {
		t.Fatalf("share_pct evidence: got=%v want >=0.80", got)
	}
	if got, _ := c.Evidence["signature_key"].(string); got != "toyota|camry|silver|sedan" {
		t.Fatalf("signature_key evidence: got=%q", got)
	}
}

func TestFuseSignatures_BelowConsistencyThreshold_NoComponent(t *testing.T) {
	deps := newFakeFusionDeps()
	plate := []byte{0x02}
	// 6/10 = 60% -- below the 80% spec.
	deps.seedDetections(plate, 1, 6)
	deps.seedDetections(plate, 2, 4)

	got, err := FuseSignatures(context.Background(), deps, FusionInput{
		PlateHash:  plate,
		Now:        fusionBaseTime,
		Thresholds: DefaultFusionThresholds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ExtraSeverity != 0 {
		t.Fatalf("expected no severity bump, got %v", got.ExtraSeverity)
	}
	if _, ok := findFusionComponent(got, ComponentSignatureConsistent); ok {
		t.Fatal("did not expect signature_consistent below 80%")
	}
}

// --- (2) signature-conflict: signature_inconsistent ---

func TestFuseSignatures_SignatureInconsistent_FlagWithoutSeverity(t *testing.T) {
	deps := newFakeFusionDeps()
	plate := []byte{0x03}
	// 5 sig-1, 5 sig-2 -> two signatures each at 50% (>=20% spec).
	deps.seedDetections(plate, 1, 5)
	deps.seedDetections(plate, 2, 5)

	got, err := FuseSignatures(context.Background(), deps, FusionInput{
		PlateHash:  plate,
		Now:        fusionBaseTime,
		Thresholds: DefaultFusionThresholds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := findFusionComponent(got, ComponentSignatureInconsistent)
	if !ok {
		t.Fatalf("missing signature_inconsistent component (got=%+v)", got.Components)
	}
	if c.Points != 0 {
		t.Fatalf("points should be 0 for inconsistent flag, got %v", c.Points)
	}
	if got.ExtraSeverity != 0 {
		t.Fatalf("ExtraSeverity should not increase from inconsistent flag, got %v", got.ExtraSeverity)
	}
	sigs, _ := c.Evidence["signatures"].([]int64)
	if len(sigs) != 2 {
		t.Fatalf("evidence signatures: got=%v want=2 entries", sigs)
	}
}

func TestFuseSignatures_SignatureInconsistent_OneTinyShareDoesNotTrigger(t *testing.T) {
	deps := newFakeFusionDeps()
	plate := []byte{0x04}
	// 18 sig-1, 1 sig-2 (5%) -> only one signature crosses 20%.
	deps.seedDetections(plate, 1, 18)
	deps.seedDetections(plate, 2, 1)

	got, err := FuseSignatures(context.Background(), deps, FusionInput{
		PlateHash:  plate,
		Now:        fusionBaseTime,
		Thresholds: DefaultFusionThresholds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findFusionComponent(got, ComponentSignatureInconsistent); ok {
		t.Fatal("did not expect signature_inconsistent when only one signature crosses 20%")
	}
}

// --- (3) plate-swap detection ---

func TestFuseSignatures_PlateSwap_3PlatesIn1Area_RaisesAlert(t *testing.T) {
	deps := newFakeFusionDeps()
	plate := []byte{0x10}
	// Plate is dominantly under signature 7.
	deps.seedDetections(plate, 7, 10)
	deps.seedSignature(7, "toyota|camry|silver|sedan")

	// Three distinct plate hashes in cell (100, 200), one in a
	// different cell (101, 200). The swap alert must include the
	// three from the dense cell only.
	now := fusionBaseTime
	deps.seedPlatesForSignatureInArea(7, []byte{0xa1}, 100, 200, 40.0, -73.0, now.Add(-2*time.Hour))
	deps.seedPlatesForSignatureInArea(7, []byte{0xa2}, 100, 200, 40.0, -73.0, now.Add(-1*time.Hour))
	deps.seedPlatesForSignatureInArea(7, []byte{0xa3}, 100, 200, 40.0, -73.0, now.Add(-30*time.Minute))
	deps.seedPlatesForSignatureInArea(7, []byte{0xb9}, 101, 200, 41.0, -73.0, now.Add(-3*time.Hour))

	got, err := FuseSignatures(context.Background(), deps, FusionInput{
		PlateHash:  plate,
		Now:        now,
		Thresholds: DefaultFusionThresholds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PlateSwapAlerts) != 1 {
		t.Fatalf("expected 1 plate-swap alert, got %d", len(got.PlateSwapAlerts))
	}
	swap := got.PlateSwapAlerts[0]
	if swap.SignatureID != 7 {
		t.Fatalf("signature id: got=%d want=7", swap.SignatureID)
	}
	if swap.Severity != DefaultPlateSwapSeverity {
		t.Fatalf("severity: got=%d want=%d", swap.Severity, DefaultPlateSwapSeverity)
	}
	if len(swap.PlateHashes) != 3 {
		t.Fatalf("plate hashes: got=%d want=3", len(swap.PlateHashes))
	}
	// Plate hashes must be lexicographically sorted for deterministic
	// evidence; assert the first is 0xa1 (the lowest input).
	if !bytes.Equal(swap.PlateHashes[0], []byte{0xa1}) {
		t.Fatalf("first plate hash: got=%x want=a1", swap.PlateHashes[0])
	}
	if !swap.MostRecentSeen.Equal(now.Add(-30 * time.Minute)) {
		t.Fatalf("most recent seen: got=%v want=%v", swap.MostRecentSeen, now.Add(-30*time.Minute))
	}
	// The component must also be present so the audit row records
	// that fusion contributed.
	if _, ok := findFusionComponent(got, ComponentPlateSwap); !ok {
		t.Fatal("missing plate_swap component on alert path")
	}
}

func TestFuseSignatures_PlateSwap_BelowThreshold_NoAlert(t *testing.T) {
	deps := newFakeFusionDeps()
	plate := []byte{0x11}
	deps.seedDetections(plate, 7, 10)
	now := fusionBaseTime
	// Only 2 plates in any cell -- below the spec's 3-plate floor.
	deps.seedPlatesForSignatureInArea(7, []byte{0xc1}, 100, 200, 40.0, -73.0, now.Add(-1*time.Hour))
	deps.seedPlatesForSignatureInArea(7, []byte{0xc2}, 100, 200, 40.0, -73.0, now.Add(-2*time.Hour))

	got, err := FuseSignatures(context.Background(), deps, FusionInput{
		PlateHash:  plate,
		Now:        now,
		Thresholds: DefaultFusionThresholds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PlateSwapAlerts) != 0 {
		t.Fatalf("did not expect alert; got %d", len(got.PlateSwapAlerts))
	}
}

func TestFuseSignatures_PlateSwap_AllPlatesWhitelisted_Suppressed(t *testing.T) {
	deps := newFakeFusionDeps()
	plate := []byte{0x12}
	deps.seedDetections(plate, 7, 10)
	now := fusionBaseTime
	plates := [][]byte{{0xd1}, {0xd2}, {0xd3}}
	for i, p := range plates {
		deps.seedPlatesForSignatureInArea(7, p, 100, 200, 40.0, -73.0, now.Add(-time.Duration(i+1)*time.Hour))
	}
	for _, p := range plates {
		deps.seedWhitelist(p)
	}

	got, err := FuseSignatures(context.Background(), deps, FusionInput{
		PlateHash:  plate,
		Now:        now,
		Thresholds: DefaultFusionThresholds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PlateSwapAlerts) != 0 {
		t.Fatalf("whitelisted plates should suppress alert; got %d", len(got.PlateSwapAlerts))
	}
}

func TestFuseSignatures_PlateSwap_PartialWhitelistDropsBelowThreshold(t *testing.T) {
	deps := newFakeFusionDeps()
	plate := []byte{0x13}
	deps.seedDetections(plate, 7, 10)
	now := fusionBaseTime
	// Three plates, two whitelisted. Survivor count = 1 < 3.
	plates := [][]byte{{0xe1}, {0xe2}, {0xe3}}
	for i, p := range plates {
		deps.seedPlatesForSignatureInArea(7, p, 100, 200, 40.0, -73.0, now.Add(-time.Duration(i+1)*time.Hour))
	}
	deps.seedWhitelist(plates[0])
	deps.seedWhitelist(plates[1])

	got, err := FuseSignatures(context.Background(), deps, FusionInput{
		PlateHash:  plate,
		Now:        now,
		Thresholds: DefaultFusionThresholds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PlateSwapAlerts) != 0 {
		t.Fatalf("expected partial whitelist to suppress alert; got %d", len(got.PlateSwapAlerts))
	}
}

func TestFuseSignatures_PlateSwap_RespectsLookbackWindow(t *testing.T) {
	deps := newFakeFusionDeps()
	plate := []byte{0x14}
	deps.seedDetections(plate, 7, 10)
	now := fusionBaseTime
	// One inside window, two well outside (60d ago).
	deps.seedPlatesForSignatureInArea(7, []byte{0xf1}, 100, 200, 40.0, -73.0, now.Add(-1*time.Hour))
	deps.seedPlatesForSignatureInArea(7, []byte{0xf2}, 100, 200, 40.0, -73.0, now.Add(-60*24*time.Hour))
	deps.seedPlatesForSignatureInArea(7, []byte{0xf3}, 100, 200, 40.0, -73.0, now.Add(-30*24*time.Hour))

	got, err := FuseSignatures(context.Background(), deps, FusionInput{
		PlateHash:  plate,
		Now:        now,
		Thresholds: DefaultFusionThresholds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PlateSwapAlerts) != 0 {
		t.Fatalf("only one in-window plate; alert should not fire (got %d)", len(got.PlateSwapAlerts))
	}
}

// --- (4) missing-signature path ---

func TestFuseSignatures_AllNullSignatures_ReturnsEmpty(t *testing.T) {
	deps := newFakeFusionDeps()
	plate := []byte{0x20}
	// All 50 detections with no signature.
	deps.seedDetections(plate, 0, 50)

	got, err := FuseSignatures(context.Background(), deps, FusionInput{
		PlateHash:  plate,
		Now:        fusionBaseTime,
		Thresholds: DefaultFusionThresholds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ExtraSeverity != 0 {
		t.Fatalf("severity should be 0 when no signatures, got %v", got.ExtraSeverity)
	}
	if len(got.Components) != 0 {
		t.Fatalf("components should be empty, got %+v", got.Components)
	}
	if len(got.PlateSwapAlerts) != 0 {
		t.Fatalf("alerts should be empty, got %d", len(got.PlateSwapAlerts))
	}
}

func TestFuseSignatures_NoDetectionsReturnsEmpty(t *testing.T) {
	deps := newFakeFusionDeps()
	plate := []byte{0x21}

	got, err := FuseSignatures(context.Background(), deps, FusionInput{
		PlateHash:  plate,
		Now:        fusionBaseTime,
		Thresholds: DefaultFusionThresholds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ExtraSeverity != 0 || len(got.Components) != 0 || len(got.PlateSwapAlerts) != 0 {
		t.Fatalf("expected empty result, got %+v", got)
	}
}

// --- (5) error propagation + nil deps ---

func TestFuseSignatures_NilDepsReturnsError(t *testing.T) {
	_, err := FuseSignatures(context.Background(), nil, FusionInput{
		PlateHash:  []byte{0x99},
		Now:        fusionBaseTime,
		Thresholds: DefaultFusionThresholds(),
	})
	if !errors.Is(err, ErrNoFusionDeps) {
		t.Fatalf("expected ErrNoFusionDeps, got %v", err)
	}
}

func TestFuseSignatures_CountErrorPropagates(t *testing.T) {
	deps := newFakeFusionDeps()
	deps.countErr = errors.New("boom")
	_, err := FuseSignatures(context.Background(), deps, FusionInput{
		PlateHash:  []byte{0x99},
		Now:        fusionBaseTime,
		Thresholds: DefaultFusionThresholds(),
	})
	if err == nil {
		t.Fatal("expected error from CountDetectionsBySignatureForPlate")
	}
}

// --- (6) version constant exists and is non-empty ---

func TestSignatureFusionVersion_NotEmpty(t *testing.T) {
	if SignatureFusionVersion == "" {
		t.Fatal("SignatureFusionVersion must not be empty")
	}
	if SignatureFusionVersion == HeuristicVersion {
		t.Fatal("SignatureFusionVersion must be tracked independently of HeuristicVersion")
	}
}

// --- (7) deterministic ordering: pickDominant tie-break ---

func TestPickDominant_TieBreaksByLowestID(t *testing.T) {
	bySigID := map[int64]int64{
		5: 10,
		1: 10,
		3: 10,
	}
	id, count := pickDominant(bySigID)
	if id != 1 || count != 10 {
		t.Fatalf("pickDominant: got=(%d,%d) want=(1,10)", id, count)
	}
}
