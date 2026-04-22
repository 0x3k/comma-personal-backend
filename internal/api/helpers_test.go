package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
)

// testDeviceKey generates an RSA key pair for a test device and returns the
// private key alongside the PEM-encoded public key that would be stored in
// the devices.public_key column at pilotauth time.
func testDeviceKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate rsa key: %v", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	return priv, string(pemBytes)
}

// signDeviceJWT issues an RS256 token with openpilot's claim shape (identity,
// iat, exp) signed with the device's private key.
func signDeviceJWT(t *testing.T, priv *rsa.PrivateKey, dongleID string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"identity": dongleID,
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(priv)
	if err != nil {
		t.Fatalf("failed to sign test token: %v", err)
	}
	return signed
}

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
