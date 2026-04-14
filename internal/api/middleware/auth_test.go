package middleware

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

// testKeys holds RSA and ECDSA key pairs generated once for the test suite.
type testKeys struct {
	rsaPrivate   *rsa.PrivateKey
	rsaPublic    *rsa.PublicKey
	ecdsaPrivate *ecdsa.PrivateKey
	ecdsaPublic  *ecdsa.PublicKey
}

func generateTestKeys(t *testing.T) testKeys {
	t.Helper()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate ECDSA key: %v", err)
	}

	return testKeys{
		rsaPrivate:   rsaKey,
		rsaPublic:    &rsaKey.PublicKey,
		ecdsaPrivate: ecKey,
		ecdsaPublic:  &ecKey.PublicKey,
	}
}

func signToken(t *testing.T, method jwt.SigningMethod, key interface{}, claims jwt.MapClaims) string {
	t.Helper()

	token := jwt.NewWithClaims(method, claims)
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	return signed
}

func setupEcho(t *testing.T, publicKey interface{}) (*echo.Echo, echo.HandlerFunc) {
	t.Helper()

	e := echo.New()
	handler := func(c echo.Context) error {
		dongleID := c.Get(ContextKeyDongleID)
		return c.JSON(http.StatusOK, map[string]interface{}{
			"dongle_id": dongleID,
		})
	}
	return e, handler
}

func doRequest(t *testing.T, e *echo.Echo, handler echo.HandlerFunc, publicKey interface{}, authHeader string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	mw := JWTAuth(publicKey)
	h := mw(handler)
	_ = h(c)

	return rec
}

type errBody struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

func parseErrorBody(t *testing.T, rec *httptest.ResponseRecorder) errBody {
	t.Helper()

	var body errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse error body: %v", err)
	}
	return body
}

func TestJWTAuth(t *testing.T) {
	keys := generateTestKeys(t)

	combinedKeys := []crypto.PublicKey{keys.rsaPublic, keys.ecdsaPublic}

	tests := []struct {
		name         string
		authHeader   string
		publicKey    interface{}
		wantStatus   int
		wantDongleID string
		wantError    bool
	}{
		{
			name: "valid RS256 token with dongle_id claim",
			authHeader: "Bearer " + signToken(t, jwt.SigningMethodRS256, keys.rsaPrivate, jwt.MapClaims{
				"dongle_id": "abc123",
				"exp":       jwt.NewNumericDate(time.Now().Add(time.Hour)),
				"iat":       jwt.NewNumericDate(time.Now()),
			}),
			publicKey:    keys.rsaPublic,
			wantStatus:   http.StatusOK,
			wantDongleID: "abc123",
		},
		{
			name: "valid ES256 token with dongle_id claim",
			authHeader: "Bearer " + signToken(t, jwt.SigningMethodES256, keys.ecdsaPrivate, jwt.MapClaims{
				"dongle_id": "def456",
				"exp":       jwt.NewNumericDate(time.Now().Add(time.Hour)),
				"iat":       jwt.NewNumericDate(time.Now()),
			}),
			publicKey:    keys.ecdsaPublic,
			wantStatus:   http.StatusOK,
			wantDongleID: "def456",
		},
		{
			name: "valid RS256 token with identity claim",
			authHeader: "Bearer " + signToken(t, jwt.SigningMethodRS256, keys.rsaPrivate, jwt.MapClaims{
				"identity": "ghi789",
				"exp":      jwt.NewNumericDate(time.Now().Add(time.Hour)),
				"iat":      jwt.NewNumericDate(time.Now()),
			}),
			publicKey:    keys.rsaPublic,
			wantStatus:   http.StatusOK,
			wantDongleID: "ghi789",
		},
		{
			name: "valid RS256 token with sub claim",
			authHeader: "Bearer " + signToken(t, jwt.SigningMethodRS256, keys.rsaPrivate, jwt.MapClaims{
				"sub": "jkl012",
				"exp": jwt.NewNumericDate(time.Now().Add(time.Hour)),
				"iat": jwt.NewNumericDate(time.Now()),
			}),
			publicKey:    keys.rsaPublic,
			wantStatus:   http.StatusOK,
			wantDongleID: "jkl012",
		},
		{
			name: "valid RS256 token with combined keys",
			authHeader: "Bearer " + signToken(t, jwt.SigningMethodRS256, keys.rsaPrivate, jwt.MapClaims{
				"dongle_id": "combo1",
				"exp":       jwt.NewNumericDate(time.Now().Add(time.Hour)),
				"iat":       jwt.NewNumericDate(time.Now()),
			}),
			publicKey:    combinedKeys,
			wantStatus:   http.StatusOK,
			wantDongleID: "combo1",
		},
		{
			name: "valid ES256 token with combined keys",
			authHeader: "Bearer " + signToken(t, jwt.SigningMethodES256, keys.ecdsaPrivate, jwt.MapClaims{
				"dongle_id": "combo2",
				"exp":       jwt.NewNumericDate(time.Now().Add(time.Hour)),
				"iat":       jwt.NewNumericDate(time.Now()),
			}),
			publicKey:    combinedKeys,
			wantStatus:   http.StatusOK,
			wantDongleID: "combo2",
		},
		{
			name: "expired token returns 401",
			authHeader: "Bearer " + signToken(t, jwt.SigningMethodRS256, keys.rsaPrivate, jwt.MapClaims{
				"dongle_id": "expired1",
				"exp":       jwt.NewNumericDate(time.Now().Add(-time.Hour)),
				"iat":       jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			}),
			publicKey:  keys.rsaPublic,
			wantStatus: http.StatusUnauthorized,
			wantError:  true,
		},
		{
			name: "wrong algorithm returns 401 (ES256 token with RSA key)",
			authHeader: "Bearer " + signToken(t, jwt.SigningMethodES256, keys.ecdsaPrivate, jwt.MapClaims{
				"dongle_id": "wrongalg",
				"exp":       jwt.NewNumericDate(time.Now().Add(time.Hour)),
				"iat":       jwt.NewNumericDate(time.Now()),
			}),
			publicKey:  keys.rsaPublic, // only RSA key, but token is ES256
			wantStatus: http.StatusUnauthorized,
			wantError:  true,
		},
		{
			name: "wrong algorithm returns 401 (RS256 token with ECDSA key)",
			authHeader: "Bearer " + signToken(t, jwt.SigningMethodRS256, keys.rsaPrivate, jwt.MapClaims{
				"dongle_id": "wrongalg2",
				"exp":       jwt.NewNumericDate(time.Now().Add(time.Hour)),
				"iat":       jwt.NewNumericDate(time.Now()),
			}),
			publicKey:  keys.ecdsaPublic, // only ECDSA key, but token is RS256
			wantStatus: http.StatusUnauthorized,
			wantError:  true,
		},
		{
			name:       "missing token returns 401",
			authHeader: "",
			publicKey:  keys.rsaPublic,
			wantStatus: http.StatusUnauthorized,
			wantError:  true,
		},
		{
			name:       "malformed authorization header returns 401",
			authHeader: "NotBearer sometoken",
			publicKey:  keys.rsaPublic,
			wantStatus: http.StatusUnauthorized,
			wantError:  true,
		},
		{
			name:       "malformed token returns 401",
			authHeader: "Bearer not.a.valid.jwt.token",
			publicKey:  keys.rsaPublic,
			wantStatus: http.StatusUnauthorized,
			wantError:  true,
		},
		{
			name:       "bearer with empty token returns 401",
			authHeader: "Bearer ",
			publicKey:  keys.rsaPublic,
			wantStatus: http.StatusUnauthorized,
			wantError:  true,
		},
		{
			name: "token without dongle_id claim returns 401",
			authHeader: "Bearer " + signToken(t, jwt.SigningMethodRS256, keys.rsaPrivate, jwt.MapClaims{
				"exp": jwt.NewNumericDate(time.Now().Add(time.Hour)),
				"iat": jwt.NewNumericDate(time.Now()),
			}),
			publicKey:  keys.rsaPublic,
			wantStatus: http.StatusUnauthorized,
			wantError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, handler := setupEcho(t, tt.publicKey)
			rec := doRequest(t, e, handler, tt.publicKey, tt.authHeader)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantError {
				body := parseErrorBody(t, rec)
				if body.Code != http.StatusUnauthorized {
					t.Errorf("error code = %d, want %d", body.Code, http.StatusUnauthorized)
				}
				if body.Error == "" {
					t.Error("expected non-empty error message")
				}
			}

			if tt.wantDongleID != "" {
				var body map[string]interface{}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("failed to parse success body: %v", err)
				}
				got, ok := body["dongle_id"].(string)
				if !ok {
					t.Fatalf("dongle_id not found in response body: %s", rec.Body.String())
				}
				if got != tt.wantDongleID {
					t.Errorf("dongle_id = %q, want %q", got, tt.wantDongleID)
				}
			}
		})
	}
}
