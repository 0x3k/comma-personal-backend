package middleware

import (
	"fmt"
	"net/http"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

// JWTAuthHMAC returns Echo middleware that validates JWT tokens signed with
// HS256 using the provided secret. It extracts the dongle_id from the token
// claims (checking "dongle_id", "identity", and "sub" in order) and stores
// it in the Echo context under ContextKeyDongleID.
func JWTAuthHMAC(secret string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			tokenString, err := extractToken(c)
			if err != nil {
				return c.JSON(http.StatusUnauthorized, errorResponse{
					Error: err.Error(),
					Code:  http.StatusUnauthorized,
				})
			}

			token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
				if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, fmt.Errorf("unexpected signing method: %s", token.Method.Alg())
				}
				return []byte(secret), nil
			}, jwt.WithValidMethods([]string{"HS256"}))
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
