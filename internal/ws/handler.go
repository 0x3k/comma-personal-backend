package ws

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
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

// SunnylinkDeviceLookup is the subset of db.Queries needed to resolve a
// sunnylink connection: the WS handler reads the JWT's identity claim, looks
// up the device row by its sunnylink_dongle_id, and verifies the token
// against the stored sunnylink_public_key. Kept separate from
// middleware.DeviceLookup so tests can stub each one independently.
type SunnylinkDeviceLookup interface {
	GetDeviceBySunnylinkDongleID(ctx context.Context, sunnylinkDongleID pgtype.Text) (db.Device, error)
}

// Handler provides the Echo handler for WebSocket connections. It serves
// both the comma athena path (/ws/v2/:dongle_id, /ws/:dongle_id) and the
// sunnylink path (/ws/sunnylink), distinguished by which auth and lookup
// path runs at upgrade time.
type Handler struct {
	hub             *Hub
	lookup          middleware.DeviceLookup
	sunnylinkLookup SunnylinkDeviceLookup
	handlers        map[string]MethodHandler
	rpcCaller       *RPCCaller
}

// NewHandler creates a WebSocket handler.
// The lookup is used to resolve each connecting device's public key for JWT
// verification. The handlers map is passed to each new Client for JSON-RPC
// method dispatch. The rpcCaller, if non-nil, is passed to each Client so
// that RPC responses from the device are routed back to pending
// RPCCaller.Call invocations.
//
// If lookup also implements SunnylinkDeviceLookup (the production
// *db.Queries does), the sunnylink WS path is registered. Tests that don't
// care about the sunnylink path can pass a lookup that doesn't satisfy the
// extended interface; the route will still be registered but every connect
// will fail at lookup time with "sunnylink not configured".
func NewHandler(hub *Hub, lookup middleware.DeviceLookup, handlers map[string]MethodHandler, rpcCaller *RPCCaller) *Handler {
	if handlers == nil {
		handlers = make(map[string]MethodHandler)
	}
	h := &Handler{
		hub:       hub,
		lookup:    lookup,
		handlers:  handlers,
		rpcCaller: rpcCaller,
	}
	if sl, ok := lookup.(SunnylinkDeviceLookup); ok {
		h.sunnylinkLookup = sl
	}
	return h
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

// RegisterRoutes wires up the WebSocket routes on the given Echo instance.
//
// Athena (comma): /ws/v2/:dongle_id (the path openpilot's athenad connects
// to) and /ws/:dongle_id (kept for direct use).
//
// Sunnylink: /ws/sunnylink (no path component for the dongle_id, since
// sunnylinkd connects to SUNNYLINK_ATHENA_HOST verbatim and identifies the
// device via the Bearer JWT's `identity` claim only). Operators point
// SUNNYLINK_ATHENA_HOST at e.g. "wss://my-backend/ws/sunnylink".
func (h *Handler) RegisterRoutes(e *echo.Echo) {
	e.GET("/ws/v2/:dongle_id", h.HandleWebSocket)
	e.GET("/ws/:dongle_id", h.HandleWebSocket)
	e.GET("/ws/sunnylink", h.HandleSunnylinkWebSocket)
}

// HandleSunnylinkWebSocket is the Echo handler for GET /ws/sunnylink.
// It mirrors HandleWebSocket but identifies the device via the
// sunnylink_dongle_id encoded in the Bearer JWT's `identity` claim, and
// verifies the signature against devices.sunnylink_public_key.
//
// The accepted Client.DongleID is the *sunnylink* dongle_id, which keeps
// it disjoint from the comma athena hub entry for the same physical device.
// Storage paths and operator UIs that key on comma_dongle_id therefore see
// sunnylink-pushed payloads as a separate stream -- intentional, since the
// device sends a parallel forwardLogs from sunnylinkd.
func (h *Handler) HandleSunnylinkWebSocket(c echo.Context) error {
	if h.sunnylinkLookup == nil {
		return c.JSON(http.StatusServiceUnavailable, wsErrorResponse{
			Error: "sunnylink not configured",
			Code:  http.StatusServiceUnavailable,
		})
	}

	tokenStr := extractAuthHeaderToken(c)
	if tokenStr == "" {
		tokenStr = readCookieToken(c)
	}
	if tokenStr == "" {
		tokenStr = c.QueryParam("token")
	}
	if tokenStr == "" {
		return c.JSON(http.StatusUnauthorized, wsErrorResponse{
			Error: "missing authorization token",
			Code:  http.StatusUnauthorized,
		})
	}

	sunnylinkID, err := middleware.ParseIdentity(tokenStr)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, wsErrorResponse{
			Error: err.Error(),
			Code:  http.StatusUnauthorized,
		})
	}

	device, err := h.sunnylinkLookup.GetDeviceBySunnylinkDongleID(
		c.Request().Context(),
		pgtype.Text{String: sunnylinkID, Valid: true},
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusUnauthorized, wsErrorResponse{
				Error: "sunnylink device not registered",
				Code:  http.StatusUnauthorized,
			})
		}
		return c.JSON(http.StatusInternalServerError, wsErrorResponse{
			Error: "failed to look up sunnylink device",
			Code:  http.StatusInternalServerError,
		})
	}
	if !device.SunnylinkPublicKey.Valid || device.SunnylinkPublicKey.String == "" {
		return c.JSON(http.StatusUnauthorized, wsErrorResponse{
			Error: "sunnylink device has no registered public key",
			Code:  http.StatusUnauthorized,
		})
	}

	if err := middleware.VerifySignedToken(tokenStr, device.SunnylinkPublicKey.String); err != nil {
		return c.JSON(http.StatusUnauthorized, wsErrorResponse{
			Error: fmt.Sprintf("failed to validate token: %s", err.Error()),
			Code:  http.StatusUnauthorized,
		})
	}

	conn, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		log.Printf("ws/sunnylink: failed to upgrade connection: %v", err)
		return nil
	}

	client := NewClient(sunnylinkID, conn, h.hub, h.handlers, h.rpcCaller)
	h.hub.Register(client)
	client.Run()
	return nil
}
