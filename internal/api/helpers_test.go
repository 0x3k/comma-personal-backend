package api

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
)

// mockDBTX is a single-query mock used by tests that only exercise one
// QueryRow path (e.g. GetDevice). Pilotauth uses its own specialized mock
// because it chains a SELECT lookup with an INSERT.
type mockDBTX struct {
	device *db.Device
	err    error
}

func (m *mockDBTX) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *mockDBTX) Query(_ context.Context, _ string, _ ...interface{}) (pgx.Rows, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockDBTX) QueryRow(_ context.Context, _ string, _ ...interface{}) pgx.Row {
	return &mockRow{device: m.device, err: m.err}
}

// mockRow is a shared pgx.Row implementation used by both mocks.
type mockRow struct {
	device *db.Device
	err    error
}

func (r *mockRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	if r.device == nil {
		return fmt.Errorf("no device")
	}
	if len(dest) < 5 {
		return fmt.Errorf("expected 5 scan destinations, got %d", len(dest))
	}
	*dest[0].(*string) = r.device.DongleID
	*dest[1].(*pgtype.Text) = r.device.Serial
	*dest[2].(*pgtype.Text) = r.device.PublicKey
	*dest[3].(*pgtype.Timestamptz) = r.device.CreatedAt
	*dest[4].(*pgtype.Timestamptz) = r.device.UpdatedAt
	return nil
}

func newTestDevice(dongleID, serial, publicKey string) *db.Device {
	now := time.Now()
	return &db.Device{
		DongleID:  dongleID,
		Serial:    pgtype.Text{String: serial, Valid: serial != ""},
		PublicKey: pgtype.Text{String: publicKey, Valid: publicKey != ""},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
}
