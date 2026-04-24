package api

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/storage"
	"comma-personal-backend/internal/worker"
)

// ThumbnailHandler serves the JPEG previews produced by the background
// thumbnail worker.
type ThumbnailHandler struct {
	storage *storage.Storage
}

// NewThumbnailHandler constructs a ThumbnailHandler backed by the same
// filesystem storage the worker writes into.
func NewThumbnailHandler(s *storage.Storage) *ThumbnailHandler {
	return &ThumbnailHandler{storage: s}
}

// GetThumbnail handles GET /v1/routes/:dongle_id/:route_name/thumbnail.
// It locates the thumbnail on disk (preferring segment 0, falling back to
// the lowest-numbered segment that has one) and streams it with a 1-day
// cache header plus a content-hash ETag. Responds with 404 when no
// thumbnail has been generated yet so the UI can fall back to a
// placeholder without needing a separate status endpoint.
func (h *ThumbnailHandler) GetThumbnail(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	segment, path, ok := h.locateThumbnail(dongleID, routeName)
	if !ok {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "thumbnail not generated yet",
			Code:  http.StatusNotFound,
		})
	}

	info, err := os.Stat(path)
	if err != nil {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "thumbnail not generated yet",
			Code:  http.StatusNotFound,
		})
	}

	etag := thumbnailETag(dongleID, routeName, segment, info.Size(), info.ModTime().UnixNano())
	resp := c.Response()
	resp.Header().Set("Cache-Control", "public, max-age=86400")
	resp.Header().Set("ETag", etag)

	// If-None-Match short-circuit so the UI does not re-download the image
	// on every route-list render when nothing has changed.
	if match := c.Request().Header.Get("If-None-Match"); match != "" && match == etag {
		return c.NoContent(http.StatusNotModified)
	}

	return c.File(path)
}

// locateThumbnail returns the segment and absolute on-disk path of the
// route's thumbnail, checking the same segments the worker considers.
// The third return value is false when no thumbnail exists yet.
func (h *ThumbnailHandler) locateThumbnail(dongleID, route string) (string, string, bool) {
	// Segment 0 is the fast path: one stat, no directory listing.
	if h.storage.Exists(dongleID, route, "0", worker.ThumbnailFileName) {
		return "0", h.storage.Path(dongleID, route, "0", worker.ThumbnailFileName), true
	}
	segments, err := h.storage.ListSegments(dongleID, route)
	if err != nil {
		return "", "", false
	}
	for _, n := range segments {
		s := strconv.Itoa(n)
		if h.storage.Exists(dongleID, route, s, worker.ThumbnailFileName) {
			return s, h.storage.Path(dongleID, route, s, worker.ThumbnailFileName), true
		}
	}
	return "", "", false
}

// thumbnailETag produces a short, stable tag for the cache validator. We
// mix in the dongle, route, size and mtime so any regeneration of the
// file invalidates the client cache.
func thumbnailETag(dongleID, route, segment string, size int64, modNanos int64) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%d", dongleID, route, segment, size, modNanos)
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

// RegisterRoutes wires the thumbnail endpoint onto an Echo group. The
// group should already have SessionOrJWT middleware applied so either
// the dashboard cookie or a device JWT can fetch the image.
func (h *ThumbnailHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/:dongle_id/:route_name/thumbnail", h.GetThumbnail)
}
