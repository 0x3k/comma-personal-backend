package middleware

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

// ContextKeyDongleID is the key used to store the dongle_id in the Echo context.
const ContextKeyDongleID = "dongle_id"

// errorResponse is the JSON envelope returned on auth failure.
type errorResponse struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// JWTAuth returns Echo middleware that validates JWT tokens signed with RS256
// or ES256. The publicKey parameter must be an *rsa.PublicKey, *ecdsa.PublicKey,
// or a slice containing both ([]crypto.PublicKey) to support multiple key types.
//
// The middleware extracts the dongle_id from token claims (checking the fields
// "dongle_id", "identity", and "sub" in that order) and stores it in the Echo
// context under the key ContextKeyDongleID.
//
// Tokens are read from the Authorization: Bearer header.
func JWTAuth(publicKey interface{}) echo.MiddlewareFunc {
	keyFunc := buildKeyFunc(publicKey)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			tokenString, err := extractToken(c)
			if err != nil {
				return c.JSON(http.StatusUnauthorized, errorResponse{
					Error: err.Error(),
					Code:  http.StatusUnauthorized,
				})
			}

			token, err := jwt.Parse(tokenString, keyFunc,
				jwt.WithValidMethods([]string{"RS256", "ES256"}),
			)
			if err != nil {
				return c.JSON(http.StatusUnauthorized, errorResponse{
					Error: fmt.Sprintf("failed to validate token: %s", err.Error()),
					Code:  http.StatusUnauthorized,
				})
			}

			if !token.Valid {
				return c.JSON(http.StatusUnauthorized, errorResponse{
					Error: "invalid token",
					Code:  http.StatusUnauthorized,
				})
			}

			claims, ok := token.Claims.(jwt.MapClaims)
			if !ok {
				return c.JSON(http.StatusUnauthorized, errorResponse{
					Error: "failed to parse token claims",
					Code:  http.StatusUnauthorized,
				})
			}

			dongleID, err := extractDongleID(claims)
			if err != nil {
				return c.JSON(http.StatusUnauthorized, errorResponse{
					Error: err.Error(),
					Code:  http.StatusUnauthorized,
				})
			}

			c.Set(ContextKeyDongleID, dongleID)
			return next(c)
		}
	}
}

// extractToken reads the JWT token string from the request. It checks the
// Authorization: Bearer header first.
func extractToken(c echo.Context) (string, error) {
	auth := c.Request().Header.Get("Authorization")
	if auth != "" {
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			token := strings.TrimSpace(parts[1])
			if token != "" {
				return token, nil
			}
		}
	}

	return "", fmt.Errorf("missing authorization token")
}

// extractDongleID pulls the dongle_id from JWT claims. It checks the fields
// "dongle_id", "identity", and "sub" in order, returning the first non-empty
// string value found.
func extractDongleID(claims jwt.MapClaims) (string, error) {
	for _, key := range []string{"dongle_id", "identity", "sub"} {
		if val, ok := claims[key]; ok {
			if s, ok := val.(string); ok && s != "" {
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("failed to extract dongle_id from token claims")
}

// buildKeyFunc returns a jwt.Keyfunc that selects the correct public key based
// on the token's signing algorithm.
func buildKeyFunc(publicKey interface{}) jwt.Keyfunc {
	return func(token *jwt.Token) (interface{}, error) {
		switch token.Method.Alg() {
		case "RS256":
			key, err := resolveKey[*rsa.PublicKey](publicKey)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve RSA public key: %w", err)
			}
			return key, nil
		case "ES256":
			key, err := resolveKey[*ecdsa.PublicKey](publicKey)
			if err != nil {
				return nil, fmt.Errorf("failed to resolve ECDSA public key: %w", err)
			}
			return key, nil
		default:
			return nil, fmt.Errorf("unexpected signing method: %s", token.Method.Alg())
		}
	}
}

// resolveKey attempts to extract a key of type T from the provided key material.
// The key material can be the key itself, or a slice of crypto.PublicKey values.
func resolveKey[T crypto.PublicKey](key interface{}) (T, error) {
	var zero T

	if k, ok := key.(T); ok {
		return k, nil
	}

	if keys, ok := key.([]crypto.PublicKey); ok {
		for _, k := range keys {
			if typed, ok := k.(T); ok {
				return typed, nil
			}
		}
	}

	return zero, fmt.Errorf("no matching key found for requested algorithm")
}
