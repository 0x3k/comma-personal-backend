package ws

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
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
	lookup    middleware.DeviceLookup
	handlers  map[string]MethodHandler
	rpcCaller *RPCCaller
}

// NewHandler creates a WebSocket handler.
// The lookup is used to resolve each connecting device's public key for JWT
// verification. The handlers map is passed to each new Client for JSON-RPC
// method dispatch. The rpcCaller, if non-nil, is passed to each Client so
// that RPC responses from the device are routed back to pending
// RPCCaller.Call invocations.
func NewHandler(hub *Hub, lookup middleware.DeviceLookup, handlers map[string]MethodHandler, rpcCaller *RPCCaller) *Handler {
	if handlers == nil {
		handlers = make(map[string]MethodHandler)
	}
	return &Handler{
		hub:       hub,
		lookup:    lookup,
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

	tokenDongleID, status, err := h.authenticate(c)
	if err != nil {
		return c.JSON(status, wsErrorResponse{
			Error: err.Error(),
			Code:  status,
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
		// gorilla's Upgrade already wrote an HTTP error response. Returning an
		// error here would make Echo try to write a second one.
		log.Printf("ws: failed to upgrade connection: %v", err)
		return nil
	}

	client := NewClient(dongleID, conn, h.hub, h.handlers, h.rpcCaller)
	h.hub.Register(client)

	// Run blocks until the client disconnects, so the handler goroutine is
	// occupied for the lifetime of the connection.
	client.Run()

	return nil
}

// authenticate resolves the identity claim in the token, looks up the
// device's public key, and verifies the token's signature against it. It
// returns the dongle_id along with an HTTP status for the caller to surface
// (401 for auth failures, 500 for lookup errors).
//
// Token sources are checked in openpilot's order of use: the `jwt=` cookie
// (athenad's primary path), then ?token= query param, then the Authorization
// header ("JWT " or "Bearer ").
func (h *Handler) authenticate(c echo.Context) (string, int, error) {
	tokenStr := readCookieToken(c)
	if tokenStr == "" {
		tokenStr = c.QueryParam("token")
	}
	if tokenStr == "" {
		tokenStr = extractAuthHeaderToken(c)
	}
	if tokenStr == "" {
		return "", http.StatusUnauthorized, fmt.Errorf("missing authorization token")
	}

	dongleID, err := middleware.ParseIdentity(tokenStr)
	if err != nil {
		return "", http.StatusUnauthorized, err
	}

	device, err := h.lookup.GetDevice(c.Request().Context(), dongleID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", http.StatusUnauthorized, fmt.Errorf("device not registered")
		}
		return "", http.StatusInternalServerError, fmt.Errorf("failed to look up device")
	}
	if !device.PublicKey.Valid || device.PublicKey.String == "" {
		return "", http.StatusUnauthorized, fmt.Errorf("device has no registered public key")
	}

	if err := middleware.VerifySignedToken(tokenStr, device.PublicKey.String); err != nil {
		return "", http.StatusUnauthorized, fmt.Errorf("failed to validate token: %w", err)
	}

	return dongleID, http.StatusOK, nil
}

// readCookieToken returns the value of the `jwt` cookie, which is how
// openpilot's athenad carries its identity token (see athenad.py).
func readCookieToken(c echo.Context) string {
	cookie, err := c.Cookie("jwt")
	if err != nil || cookie == nil {
		return ""
	}
	return strings.TrimSpace(cookie.Value)
}

// extractAuthHeaderToken reads "JWT <token>" or "Bearer <token>" from the
// Authorization header.
func extractAuthHeaderToken(c echo.Context) string {
	auth := c.Request().Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 {
		return ""
	}
	if !strings.EqualFold(parts[0], "JWT") && !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// RegisterRoutes wires up the WebSocket route on the given Echo instance.
// Both paths are registered: /ws/v2/:dongle_id is the path openpilot's
// athenad connects to, and /ws/:dongle_id is kept for direct use.
func (h *Handler) RegisterRoutes(e *echo.Echo) {
	e.GET("/ws/v2/:dongle_id", h.HandleWebSocket)
	e.GET("/ws/:dongle_id", h.HandleWebSocket)
}
