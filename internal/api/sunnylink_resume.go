package api

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// SunnylinkResumeHandler handles POST /ws/:sunnylink_dongle_id/resume_queued,
// which sunnylinkd hits via SunnylinkApi.resume_queued() right after
// re-establishing its WS connection. Upstream's behavior is to acknowledge
// the request so the device knows it can drain its upload queue. There is no
// per-tenant work to do on a single-operator personal backend, so the handler
// just returns 200 with an empty body.
//
// We accept (but do not require) a device JWT here -- the upstream client
// passes one along, and rejecting requests that arrive without one would
// just push a retry onto the device. The endpoint reveals nothing about
// other devices, so being permissive is safe.
type SunnylinkResumeHandler struct{}

// NewSunnylinkResumeHandler creates a handler for the sunnylink resume_queued
// endpoint.
func NewSunnylinkResumeHandler() *SunnylinkResumeHandler {
	return &SunnylinkResumeHandler{}
}

// Resume returns 200 with `{"ok": true}` so the device parses a successful
// response shape.
func (h *SunnylinkResumeHandler) Resume(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]bool{"ok": true})
}

// RegisterRoutes wires up the resume_queued endpoint on the given Echo
// instance. The path mirrors openpilot's BaseApi.api_get behavior in
// sunnylink: POST /ws/<sunnylink_dongle_id>/resume_queued.
func (h *SunnylinkResumeHandler) RegisterRoutes(e *echo.Echo) {
	e.POST("/ws/:sunnylink_dongle_id/resume_queued", h.Resume)
}
