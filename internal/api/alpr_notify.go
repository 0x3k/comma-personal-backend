package api

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/alpr/notify"
)

// ALPRNotifyHandler exposes the operator-facing test endpoint for the
// alert-notification pipeline. Mounted on the session-only group so
// only logged-in operators can probe their relay (a device JWT must
// not be able to make the backend send mail).
//
// The handler is a thin shim over notify.Dispatcher.Test: it constructs
// a synthetic AlertPayload, calls Test, and returns the per-sender
// outcome verbatim. Real alerts go through the AlertCreated subscriber
// loop; this endpoint is intentionally orthogonal so a setup-validation
// run does not depend on a stalking event happening.
type ALPRNotifyHandler struct {
	dispatcher *notify.Dispatcher
}

// NewALPRNotifyHandler returns a handler bound to the supplied
// dispatcher. A nil dispatcher is permitted -- in that case the
// endpoint reports "not configured" instead of crashing, which is the
// behaviour for a deployment that runs the binary without setting any
// ALPR_NOTIFY_* env var.
func NewALPRNotifyHandler(dispatcher *notify.Dispatcher) *ALPRNotifyHandler {
	return &ALPRNotifyHandler{dispatcher: dispatcher}
}

// alprNotifyTestResponse is the wire shape returned by POST
// /v1/alpr/notify/test. The results array preserves the order the
// dispatcher fanned out to so a UI can render per-row icons stably.
type alprNotifyTestResponse struct {
	Configured bool                `json:"configured"`
	Results    []notify.SendResult `json:"results"`
}

// PostNotifyTest handles POST /v1/alpr/notify/test. The body is ignored
// (the synthetic payload is constructed server-side so the operator
// cannot accidentally emit a real-looking alert with arbitrary plate
// text). The response is a stable JSON shape.
func (h *ALPRNotifyHandler) PostNotifyTest(c echo.Context) error {
	if h.dispatcher == nil || h.dispatcher.SenderCount() == 0 {
		return c.JSON(http.StatusOK, alprNotifyTestResponse{
			Configured: false,
			Results:    []notify.SendResult{},
		})
	}
	payload := notify.AlertPayload{
		Severity: 5,
		Plate:    "TEST-123",
		// Synthetic hash that round-trips through the same
		// base64-URL-no-padding encoder the rest of the API uses, so
		// the constructed dashboard link in the email body resolves
		// to a real (404) URL the operator can recognise as test
		// output rather than a malformed link.
		PlateHashB64: notify.EncodePlateHashB64(make([]byte, 32)),
		Route:        "synthetic-test-route",
		DongleID:     "synthetic-test-device",
	}
	results := h.dispatcher.Test(c.Request().Context(), payload)
	if results == nil {
		results = []notify.SendResult{}
	}
	return c.JSON(http.StatusOK, alprNotifyTestResponse{
		Configured: true,
		Results:    results,
	})
}

// RegisterRoutes wires POST /v1/alpr/notify/test onto the supplied
// session-only Echo group. Mirrors the rest of the ALPR handler API.
func (h *ALPRNotifyHandler) RegisterRoutes(g *echo.Group) {
	g.POST("/alpr/notify/test", h.PostNotifyTest)
}
