package settings

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
)

// fakeQuerier is an in-memory stub that implements the Querier subset used
// by Store. It is intentionally tiny: Store is a thin wrapper, and the real
// SQL behaviour is already covered by sqlc-generated code.
type fakeQuerier struct {
	rows   map[string]db.Setting
	getErr error
	setErr error
	insErr error
	// insertedIfMissing records which keys went through the seed path and
	// what value was supplied, so tests can assert the no-op behaviour when
	// the row already exists.
	insertedIfMissing map[string]string
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{
		rows:              make(map[string]db.Setting),
		insertedIfMissing: make(map[string]string),
	}
}

func (f *fakeQuerier) GetSetting(_ context.Context, key string) (db.Setting, error) {
	if f.getErr != nil {
		return db.Setting{}, f.getErr
	}
	row, ok := f.rows[key]
	if !ok {
		return db.Setting{}, pgx.ErrNoRows
	}
	return row, nil
}

func (f *fakeQuerier) UpsertSetting(_ context.Context, arg db.UpsertSettingParams) (db.Setting, error) {
	if f.setErr != nil {
		return db.Setting{}, f.setErr
	}
	row := db.Setting{
		Key:       arg.Key,
		Value:     arg.Value,
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	f.rows[arg.Key] = row
	return row, nil
}

func (f *fakeQuerier) InsertSettingIfMissing(_ context.Context, arg db.InsertSettingIfMissingParams) error {
	if f.insErr != nil {
		return f.insErr
	}
	if _, ok := f.rows[arg.Key]; ok {
		return nil
	}
	f.rows[arg.Key] = db.Setting{
		Key:       arg.Key,
		Value:     arg.Value,
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	f.insertedIfMissing[arg.Key] = arg.Value
	return nil
}

func TestGetInt_ReturnsStoredValue(t *testing.T) {
	q := newFakeQuerier()
	q.rows[KeyRetentionDays] = db.Setting{Key: KeyRetentionDays, Value: "42"}
	s := New(q)

	got, err := s.GetInt(context.Background(), KeyRetentionDays)
	if err != nil {
		t.Fatalf("GetInt returned error: %v", err)
	}
	if got != 42 {
		t.Errorf("GetInt = %d, want 42", got)
	}
}

func TestGetInt_MissingKeyReturnsErrNotFound(t *testing.T) {
	s := New(newFakeQuerier())

	_, err := s.GetInt(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetInt err = %v, want ErrNotFound", err)
	}
}

func TestGetInt_NonIntegerValueFails(t *testing.T) {
	q := newFakeQuerier()
	q.rows[KeyRetentionDays] = db.Setting{Key: KeyRetentionDays, Value: "not-a-number"}
	s := New(q)

	_, err := s.GetInt(context.Background(), KeyRetentionDays)
	if err == nil {
		t.Fatal("GetInt returned nil error for non-integer value, want error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("GetInt err = %v, want non-ErrNotFound", err)
	}
}

func TestGetInt_WrapsUnderlyingDatabaseError(t *testing.T) {
	q := newFakeQuerier()
	q.getErr = fmt.Errorf("boom")
	s := New(q)

	_, err := s.GetInt(context.Background(), KeyRetentionDays)
	if err == nil {
		t.Fatal("GetInt returned nil error, want error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("GetInt err = %v, should not be ErrNotFound for a transport failure", err)
	}
}

func TestSetInt_WritesThroughAndRoundTrips(t *testing.T) {
	q := newFakeQuerier()
	s := New(q)
	ctx := context.Background()

	if err := s.SetInt(ctx, KeyRetentionDays, 7); err != nil {
		t.Fatalf("SetInt returned error: %v", err)
	}

	row, ok := q.rows[KeyRetentionDays]
	if !ok {
		t.Fatal("expected row to be written to fake store")
	}
	if row.Value != "7" {
		t.Errorf("stored value = %q, want %q", row.Value, "7")
	}

	got, err := s.GetInt(ctx, KeyRetentionDays)
	if err != nil {
		t.Fatalf("GetInt returned error: %v", err)
	}
	if got != 7 {
		t.Errorf("GetInt = %d, want 7", got)
	}
}

func TestSetInt_Overwrites(t *testing.T) {
	q := newFakeQuerier()
	s := New(q)
	ctx := context.Background()

	if err := s.SetInt(ctx, KeyRetentionDays, 1); err != nil {
		t.Fatalf("SetInt returned error: %v", err)
	}
	if err := s.SetInt(ctx, KeyRetentionDays, 2); err != nil {
		t.Fatalf("SetInt returned error: %v", err)
	}

	got, err := s.GetInt(ctx, KeyRetentionDays)
	if err != nil {
		t.Fatalf("GetInt returned error: %v", err)
	}
	if got != 2 {
		t.Errorf("GetInt = %d, want 2 (expected overwrite)", got)
	}
}

func TestSetInt_WrapsUnderlyingError(t *testing.T) {
	q := newFakeQuerier()
	q.setErr = fmt.Errorf("boom")
	s := New(q)

	if err := s.SetInt(context.Background(), KeyRetentionDays, 5); err == nil {
		t.Fatal("SetInt returned nil error, want error")
	}
}

func TestSeedIntIfMissing_InsertsWhenAbsent(t *testing.T) {
	q := newFakeQuerier()
	s := New(q)

	if err := s.SeedIntIfMissing(context.Background(), KeyRetentionDays, 30); err != nil {
		t.Fatalf("SeedIntIfMissing returned error: %v", err)
	}
	if v, ok := q.insertedIfMissing[KeyRetentionDays]; !ok || v != "30" {
		t.Errorf("insertedIfMissing[%q] = %q, want 30", KeyRetentionDays, v)
	}
	if q.rows[KeyRetentionDays].Value != "30" {
		t.Errorf("stored value = %q, want 30", q.rows[KeyRetentionDays].Value)
	}
}

func TestSeedIntIfMissing_NoOpWhenPresent(t *testing.T) {
	q := newFakeQuerier()
	q.rows[KeyRetentionDays] = db.Setting{Key: KeyRetentionDays, Value: "99"}
	s := New(q)

	if err := s.SeedIntIfMissing(context.Background(), KeyRetentionDays, 30); err != nil {
		t.Fatalf("SeedIntIfMissing returned error: %v", err)
	}
	if q.rows[KeyRetentionDays].Value != "99" {
		t.Errorf("stored value was overwritten: got %q, want 99", q.rows[KeyRetentionDays].Value)
	}
}

func TestRetentionDays_UsesEnvFallbackWhenMissing(t *testing.T) {
	s := New(newFakeQuerier())

	got, err := s.RetentionDays(context.Background(), 30)
	if err != nil {
		t.Fatalf("RetentionDays returned error: %v", err)
	}
	if got != 30 {
		t.Errorf("RetentionDays = %d, want 30 (env fallback)", got)
	}
}

func TestRetentionDays_UsesStoredValueWhenPresent(t *testing.T) {
	q := newFakeQuerier()
	q.rows[KeyRetentionDays] = db.Setting{Key: KeyRetentionDays, Value: "7"}
	s := New(q)

	got, err := s.RetentionDays(context.Background(), 30)
	if err != nil {
		t.Fatalf("RetentionDays returned error: %v", err)
	}
	if got != 7 {
		t.Errorf("RetentionDays = %d, want 7 (stored value)", got)
	}
}

func TestRetentionDays_StoredZeroMeansNeverDelete(t *testing.T) {
	q := newFakeQuerier()
	q.rows[KeyRetentionDays] = db.Setting{Key: KeyRetentionDays, Value: "0"}
	s := New(q)

	got, err := s.RetentionDays(context.Background(), 30)
	if err != nil {
		t.Fatalf("RetentionDays returned error: %v", err)
	}
	if got != 0 {
		t.Errorf("RetentionDays = %d, want 0 -- stored override must not be replaced by env default", got)
	}
}
