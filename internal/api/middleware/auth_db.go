package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// DeviceLookup is the subset of db.Queries that JWTAuthFromDB depends on.
// Kept narrow so tests can stub a single method.
type DeviceLookup interface {
	GetDevice(ctx context.Context, dongleID string) (db.Device, error)
}

// JWTAuthFromDB returns Echo middleware that verifies RS256/ES256 JWTs
// against each device's public key stored in the devices table. Tokens are
// issued and signed by the device itself (see openpilot common/api.py), so
// there is no shared server secret.
//
// The flow:
//  1. Extract the token from the Authorization header.
//  2. Parse it unverified to read the "identity" (or "dongle_id"/"sub")
//     claim.
//  3. Look up the device's public_key PEM from the database.
//  4. Re-parse the token with signature verification using that key.
//  5. On success, store the dongle_id in the Echo context under
//     ContextKeyDongleID.
func JWTAuthFromDB(lookup DeviceLookup) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			tokenString, err := extractToken(c)
			if err != nil {
				return unauthorized(c, err.Error())
			}

			dongleID, err := ParseIdentity(tokenString)
			if err != nil {
				return unauthorized(c, err.Error())
			}

			device, err := lookup.GetDevice(c.Request().Context(), dongleID)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return unauthorized(c, "device not registered")
				}
				return c.JSON(http.StatusInternalServerError, errorResponse{
					Error: "failed to look up device",
					Code:  http.StatusInternalServerError,
				})
			}
			if !device.PublicKey.Valid || device.PublicKey.String == "" {
				return unauthorized(c, "device has no registered public key")
			}

			if err := VerifySignedToken(tokenString, device.PublicKey.String); err != nil {
				return unauthorized(c, fmt.Sprintf("failed to validate token: %s", err.Error()))
			}

			c.Set(ContextKeyDongleID, dongleID)
			return next(c)
		}
	}
}

// ParseIdentity reads the identity claim from an unverified token. We only
// trust this value after VerifySignedToken succeeds with the matching public
// key; here it is just a pointer into the devices table.
func ParseIdentity(tokenString string) (string, error) {
	parser := jwt.NewParser()
	token, _, err := parser.ParseUnverified(tokenString, jwt.MapClaims{})
	if err != nil {
		return "", fmt.Errorf("failed to parse token: %w", err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", fmt.Errorf("failed to parse token claims")
	}
	return extractDongleID(claims)
}

// VerifySignedToken parses tokenString with signature verification enabled.
// The device's stored PEM is tried as an RSA public key when the token alg is
// RS256 and as an ECDSA public key when the alg is ES256 -- matching the two
// algorithms openpilot's Api supports.
func VerifySignedToken(tokenString, publicKeyPEM string) error {
	parsed, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		switch t.Method.Alg() {
		case jwt.SigningMethodRS256.Alg():
			return jwt.ParseRSAPublicKeyFromPEM([]byte(publicKeyPEM))
		case jwt.SigningMethodES256.Alg():
			return jwt.ParseECPublicKeyFromPEM([]byte(publicKeyPEM))
		default:
			return nil, fmt.Errorf("unexpected signing method: %s", t.Method.Alg())
		}
	}, jwt.WithValidMethods([]string{"RS256", "ES256"}))
	if err != nil {
		return err
	}
	if !parsed.Valid {
		return fmt.Errorf("invalid token")
	}
	return nil
}

func unauthorized(c echo.Context, msg string) error {
	return c.JSON(http.StatusUnauthorized, errorResponse{
		Error: msg,
		Code:  http.StatusUnauthorized,
	})
}
