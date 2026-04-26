package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// pairingTestRequest exercises the handler end-to-end via httptest.
func pairingTestRequest(t *testing.T, h *PairingHandler, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/pair?"+query, nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	if err := h.Pair(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return rec
}

func TestPairing_ValidToken_ShowsDongleID(t *testing.T) {
	priv, pubPEM := testDeviceKey(t)
	dongleID := "abc123def4567890"
	dev := newTestDevice(dongleID, "SERIAL001", pubPEM)
	mock := &mockDBTX{device: dev}
	handler := NewPairingHandler(&pairingLookupAdapter{mock: mock})

	claims := jwt.MapClaims{
		"identity": dongleID,
		"pair":     true,
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(priv)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	rec := pairingTestRequest(t, handler, "pair="+signed)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), dongleID) {
		t.Errorf("response missing dongle_id %q: %s", dongleID, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `class="v ok"`) {
		t.Errorf("response should mark status as ok: %s", rec.Body.String())
	}
}

func TestPairing_MissingToken(t *testing.T) {
	priv, pubPEM := testDeviceKey(t)
	dev := newTestDevice("abc123def4567890", "SERIAL001", pubPEM)
	mock := &mockDBTX{device: dev}
	handler := NewPairingHandler(&pairingLookupAdapter{mock: mock})
	_ = priv

	rec := pairingTestRequest(t, handler, "")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (we render an error page)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing pair token") {
		t.Errorf("response missing 'missing pair token': %s", rec.Body.String())
	}
}

func TestPairing_TokenSignedByOtherKey_Rejected(t *testing.T) {
	_, pubPEM := testDeviceKey(t)
	otherPriv, _ := testDeviceKey(t)

	dongleID := "abc123def4567890"
	dev := newTestDevice(dongleID, "SERIAL001", pubPEM)
	mock := &mockDBTX{device: dev}
	handler := NewPairingHandler(&pairingLookupAdapter{mock: mock})

	claims := jwt.MapClaims{
		"identity": dongleID,
		"pair":     true,
		"exp":      time.Now().Add(time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, _ := token.SignedString(otherPriv)

	rec := pairingTestRequest(t, handler, "pair="+signed)
	if !strings.Contains(rec.Body.String(), "verification failed") {
		t.Errorf("expected verification-failed status: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `class="v err"`) {
		t.Errorf("response should mark status as err: %s", rec.Body.String())
	}
}

func TestPairing_TokenWithoutPairClaim_Rejected(t *testing.T) {
	priv, pubPEM := testDeviceKey(t)
	dongleID := "abc123def4567890"
	dev := newTestDevice(dongleID, "SERIAL001", pubPEM)
	mock := &mockDBTX{device: dev}
	handler := NewPairingHandler(&pairingLookupAdapter{mock: mock})

	claims := jwt.MapClaims{
		"identity": dongleID,
		"exp":      time.Now().Add(time.Hour).Unix(),
		// no `pair: true` claim
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, _ := token.SignedString(priv)

	rec := pairingTestRequest(t, handler, "pair="+signed)
	if !strings.Contains(rec.Body.String(), "not a pair token") {
		t.Errorf("expected 'not a pair token' status: %s", rec.Body.String())
	}
}

func TestPairing_UnknownDevice_Rejected(t *testing.T) {
	mock := &mockDBTX{} // no device, every GetDevice returns error
	handler := NewPairingHandler(&pairingLookupAdapter{mock: mock, notFound: true})

	priv, _ := testDeviceKey(t)
	claims := jwt.MapClaims{
		"identity": "abc123def4567890",
		"pair":     true,
		"exp":      time.Now().Add(time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, _ := token.SignedString(priv)

	rec := pairingTestRequest(t, handler, "pair="+signed)
	if !strings.Contains(rec.Body.String(), "not registered") {
		t.Errorf("expected 'not registered' status: %s", rec.Body.String())
	}
}

func TestPairing_RegisterRoutes_RegistersPair(t *testing.T) {
	mock := &mockDBTX{}
	handler := NewPairingHandler(&pairingLookupAdapter{mock: mock})

	e := echo.New()
	handler.RegisterRoutes(e)

	found := false
	for _, r := range e.Routes() {
		if r.Method == http.MethodGet && r.Path == "/pair" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected GET /pair route to be registered")
	}
}

// pairingLookupAdapter wraps a mockDBTX as a PairingLookup so the existing
// pgx.Row stub in helpers_test.go can be reused without duplicating logic.
type pairingLookupAdapter struct {
	mock     *mockDBTX
	notFound bool
}

func (a *pairingLookupAdapter) GetDevice(_ context.Context, dongleID string) (db.Device, error) {
	_ = dongleID
	if a.notFound {
		return db.Device{}, pgx.ErrNoRows
	}
	if a.mock.device == nil {
		return db.Device{}, pgx.ErrNoRows
	}
	return *a.mock.device, nil
}
