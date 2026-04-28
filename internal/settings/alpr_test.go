package settings

import (
	"context"
	"errors"
	"testing"
	"time"

	"comma-personal-backend/internal/db"
)

func TestGetBool_RoundTrip(t *testing.T) {
	q := newFakeQuerier()
	s := New(q)
	ctx := context.Background()

	if err := s.SetBool(ctx, KeyALPREnabled, true); err != nil {
		t.Fatalf("SetBool returned error: %v", err)
	}
	got, err := s.GetBool(ctx, KeyALPREnabled)
	if err != nil {
		t.Fatalf("GetBool returned error: %v", err)
	}
	if !got {
		t.Errorf("GetBool = false, want true")
	}

	if err := s.SetBool(ctx, KeyALPREnabled, false); err != nil {
		t.Fatalf("SetBool returned error: %v", err)
	}
	got, err = s.GetBool(ctx, KeyALPREnabled)
	if err != nil {
		t.Fatalf("GetBool returned error: %v", err)
	}
	if got {
		t.Errorf("GetBool = true, want false")
	}
}

func TestGetBool_MissingKeyReturnsErrNotFound(t *testing.T) {
	s := New(newFakeQuerier())
	_, err := s.GetBool(context.Background(), KeyALPREnabled)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetBool err = %v, want ErrNotFound", err)
	}
}

func TestBoolOr_FallsBackOnMissing(t *testing.T) {
	s := New(newFakeQuerier())
	got, err := s.BoolOr(context.Background(), KeyALPREnabled, true)
	if err != nil {
		t.Fatalf("BoolOr returned error: %v", err)
	}
	if !got {
		t.Errorf("BoolOr = false, want fallback true")
	}
}

func TestBoolOr_ReturnsStoredValue(t *testing.T) {
	q := newFakeQuerier()
	q.rows[KeyALPREnabled] = db.Setting{Key: KeyALPREnabled, Value: "false"}
	s := New(q)
	got, err := s.BoolOr(context.Background(), KeyALPREnabled, true)
	if err != nil {
		t.Fatalf("BoolOr returned error: %v", err)
	}
	if got {
		t.Errorf("BoolOr = true, want stored false (override env default)")
	}
}

func TestGetFloat_RoundTrip(t *testing.T) {
	q := newFakeQuerier()
	s := New(q)
	ctx := context.Background()

	if err := s.SetFloat(ctx, KeyALPRConfidenceMin, 0.85); err != nil {
		t.Fatalf("SetFloat returned error: %v", err)
	}
	got, err := s.GetFloat(ctx, KeyALPRConfidenceMin)
	if err != nil {
		t.Fatalf("GetFloat returned error: %v", err)
	}
	if got != 0.85 {
		t.Errorf("GetFloat = %v, want 0.85", got)
	}
}

func TestFloatOr_FallsBackOnMissing(t *testing.T) {
	s := New(newFakeQuerier())
	got, err := s.FloatOr(context.Background(), KeyALPRConfidenceMin, 0.75)
	if err != nil {
		t.Fatalf("FloatOr returned error: %v", err)
	}
	if got != 0.75 {
		t.Errorf("FloatOr = %v, want fallback 0.75", got)
	}
}

func TestStringOr_FallsBackOnMissing(t *testing.T) {
	s := New(newFakeQuerier())
	got, err := s.StringOr(context.Background(), KeyALPRRegion, "us")
	if err != nil {
		t.Fatalf("StringOr returned error: %v", err)
	}
	if got != "us" {
		t.Errorf("StringOr = %q, want fallback us", got)
	}
}

func TestStringOr_ReturnsStoredValue(t *testing.T) {
	q := newFakeQuerier()
	q.rows[KeyALPRRegion] = db.Setting{Key: KeyALPRRegion, Value: "eu"}
	s := New(q)
	got, err := s.StringOr(context.Background(), KeyALPRRegion, "us")
	if err != nil {
		t.Fatalf("StringOr returned error: %v", err)
	}
	if got != "eu" {
		t.Errorf("StringOr = %q, want eu", got)
	}
}

func TestTimeOrZero_MissingReportsFalse(t *testing.T) {
	s := New(newFakeQuerier())
	_, ok, err := s.TimeOrZero(context.Background(), KeyALPRDisclaimerAckedAt)
	if err != nil {
		t.Fatalf("TimeOrZero returned error: %v", err)
	}
	if ok {
		t.Errorf("TimeOrZero ok = true for missing key, want false")
	}
}

func TestTimeOrZero_ReturnsStoredTime(t *testing.T) {
	want := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	q := newFakeQuerier()
	q.rows[KeyALPRDisclaimerAckedAt] = db.Setting{Key: KeyALPRDisclaimerAckedAt, Value: want.Format(time.RFC3339)}
	s := New(q)

	got, ok, err := s.TimeOrZero(context.Background(), KeyALPRDisclaimerAckedAt)
	if err != nil {
		t.Fatalf("TimeOrZero returned error: %v", err)
	}
	if !ok {
		t.Fatal("TimeOrZero ok = false, want true")
	}
	if !got.Equal(want) {
		t.Errorf("TimeOrZero = %v, want %v", got, want)
	}
}

func TestIntOr_FallsBackOnMissing(t *testing.T) {
	s := New(newFakeQuerier())
	got, err := s.IntOr(context.Background(), KeyALPRRetentionDaysUnflagged, 30)
	if err != nil {
		t.Fatalf("IntOr returned error: %v", err)
	}
	if got != 30 {
		t.Errorf("IntOr = %d, want fallback 30", got)
	}
}
