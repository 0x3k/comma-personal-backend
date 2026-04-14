package ws

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
)

// wsErrorResponse is the JSON error envelope for pre-upgrade HTTP responses.
type wsErrorResponse struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// upgrader configures the WebSocket upgrade with permissive origin checks
// (this is a personal backend, not a public service).
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Handler provides the Echo handler for WebSocket connections.
type Handler struct {
	hub       *Hub
	jwtSecret string
	handlers  map[string]MethodHandler
	rpcCaller *RPCCaller
}

// NewHandler creates a WebSocket handler.
// The jwtSecret is used to validate HS256 device tokens.
// The handlers map is passed to each new Client for JSON-RPC method dispatch.
// The rpcCaller, if non-nil, is passed to each Client so that RPC responses
// from the device are routed back to pending RPCCaller.Call invocations.
func NewHandler(hub *Hub, jwtSecret string, handlers map[string]MethodHandler, rpcCaller *RPCCaller) *Handler {
	if handlers == nil {
		handlers = make(map[string]MethodHandler)
	}
	return &Handler{
		hub:       hub,
		jwtSecret: jwtSecret,
		handlers:  handlers,
		rpcCaller: rpcCaller,
	}
}

// HandleWebSocket is the Echo handler for GET /ws/:dongle_id.
// It authenticates the request, upgrades to WebSocket, and manages the connection.
func (h *Handler) HandleWebSocket(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	if dongleID == "" {
		return c.JSON(http.StatusBadRequest, wsErrorResponse{
			Error: "dongle_id is required",
			Code:  http.StatusBadRequest,
		})
	}

	tokenDongleID, err := h.authenticate(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, wsErrorResponse{
			Error: err.Error(),
			Code:  http.StatusUnauthorized,
		})
	}

	// Ensure the authenticated device matches the requested dongle_id.
	if tokenDongleID != dongleID {
		return c.JSON(http.StatusForbidden, wsErrorResponse{
			Error: "token dongle_id does not match requested dongle_id",
			Code:  http.StatusForbidden,
		})
	}

	conn, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return fmt.Errorf("failed to upgrade connection: %w", err)
	}

	client := NewClient(dongleID, conn, h.hub, h.handlers, h.rpcCaller)
	h.hub.Register(client)

	// Run blocks until the client disconnects, so the handler goroutine is
	// occupied for the lifetime of the connection.
	client.Run()

	return nil
}

// authenticate extracts and validates a JWT from the request. It checks the
// "token" query parameter first, then the Authorization: Bearer header.
// Returns the dongle_id from the token claims.
func (h *Handler) authenticate(c echo.Context) (string, error) {
	tokenStr := c.QueryParam("token")
	if tokenStr == "" {
		tokenStr = extractBearerToken(c)
	}
	if tokenStr == "" {
		return "", fmt.Errorf("missing authorization token")
	}

	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(h.jwtSecret), nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to validate token: %w", err)
	}

	if !token.Valid {
		return "", fmt.Errorf("invalid token")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", fmt.Errorf("failed to parse token claims")
	}

	dongleID, err := extractDongleIDFromClaims(claims)
	if err != nil {
		return "", err
	}

	return dongleID, nil
}

// extractBearerToken reads the Bearer token from the Authorization header.
func extractBearerToken(c echo.Context) string {
	auth := c.Request().Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

// extractDongleIDFromClaims pulls the dongle_id from JWT claims, checking
// "dongle_id", "identity", and "sub" in order.
func extractDongleIDFromClaims(claims jwt.MapClaims) (string, error) {
	for _, key := range []string{"dongle_id", "identity", "sub"} {
		if val, ok := claims[key]; ok {
			if s, ok := val.(string); ok && s != "" {
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("failed to extract dongle_id from token claims")
}

// RegisterRoutes wires up the WebSocket route on the given Echo instance.
// Both paths are registered: /ws/v2/:dongle_id is the path openpilot's
// athenad connects to, and /ws/:dongle_id is kept for direct use.
func (h *Handler) RegisterRoutes(e *echo.Echo) {
	e.GET("/ws/v2/:dongle_id", h.HandleWebSocket)
	e.GET("/ws/:dongle_id", h.HandleWebSocket)
}
