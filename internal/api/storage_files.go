package api

import (
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/storage"
)

// storageFilesAllowedTopLevel whitelists the top-level segment files the
// authenticated /storage/... endpoint will stream. The player only ever
// requests qcamera.ts directly; everything else (HLS playlists + .ts
// chunks) lives under a per-camera subdirectory and goes through the
// camera-scoped route below.
var storageFilesAllowedTopLevel = map[string]bool{
	"qcamera.ts": true,
}

// storageFilesAllowedCameras is the set of per-camera subdirectories the
// transcoder writes HLS output into. Unlike the public share endpoint,
// the authenticated handler exposes all four because the dashboard
// operator is trusted to view every camera on their own devices.
var storageFilesAllowedCameras = map[string]bool{
	"fcamera": true,
	"ecamera": true,
	"dcamera": true,
	"qcamera": true,
}

// StorageFilesHandler serves segment artefacts (HLS playlists, .ts chunks,
// and the qcamera.ts preview) directly from the local storage layer for
// authenticated callers. It mirrors the public share handler's media
// routes but enforces session-or-JWT auth + checkDongleAccess instead of
// a signed token.
type StorageFilesHandler struct {
	storage *storage.Storage
}

// NewStorageFilesHandler constructs a StorageFilesHandler. storage must be
// non-nil; the handler does not consult the database (the segment list
// and upload flags are already surfaced via the route detail endpoint).
func NewStorageFilesHandler(s *storage.Storage) *StorageFilesHandler {
	return &StorageFilesHandler{storage: s}
}

// RegisterRoutes wires the storage file endpoints onto the top-level
// Echo instance. mw is the sessionOrJWT middleware so both the dashboard
// cookie and a device JWT can fetch segment media; per-route middleware
// is used (rather than an Echo group) because the registered paths
// already start with /storage/.
func (h *StorageFilesHandler) RegisterRoutes(e *echo.Echo, mw echo.MiddlewareFunc) {
	e.GET("/storage/:dongle_id/:route_name/:segment_num/:file", h.GetSegmentFile, mw)
	e.GET("/storage/:dongle_id/:route_name/:segment_num/:camera/:file", h.GetCameraFile, mw)
}

// GetSegmentFile streams a top-level segment artefact such as qcamera.ts.
// The dongle_id, route_name, and segment_num path parameters are
// validated before the filesystem is touched so a malformed URL cannot
// reach os.Open.
func (h *StorageFilesHandler) GetSegmentFile(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")
	if !isSafePathComponent(dongleID) || !isSafePathComponent(routeName) {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid path",
			Code:  http.StatusBadRequest,
		})
	}
	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	segNum, ok := parseSegmentNumber(c.Param("segment_num"))
	if !ok {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid segment_num",
			Code:  http.StatusBadRequest,
		})
	}

	file := c.Param("file")
	if !storageFilesAllowedTopLevel[file] {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "file not found",
			Code:  http.StatusNotFound,
		})
	}

	path := h.storage.Path(dongleID, routeName, strconv.Itoa(segNum), file)
	return serveShareFile(c, path, file)
}

// GetCameraFile streams an HLS playlist or .ts chunk from the per-camera
// subdirectory the transcoder writes under each segment folder. The
// :camera parameter is whitelisted against storageFilesAllowedCameras
// and the :file parameter is restricted to index.m3u8 + *.ts via the
// existing isAllowedHLSFile helper.
func (h *StorageFilesHandler) GetCameraFile(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")
	if !isSafePathComponent(dongleID) || !isSafePathComponent(routeName) {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid path",
			Code:  http.StatusBadRequest,
		})
	}
	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	segNum, ok := parseSegmentNumber(c.Param("segment_num"))
	if !ok {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid segment_num",
			Code:  http.StatusBadRequest,
		})
	}

	camera := c.Param("camera")
	if !storageFilesAllowedCameras[camera] {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "file not found",
			Code:  http.StatusNotFound,
		})
	}

	file := c.Param("file")
	if !isAllowedHLSFile(file) {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "file not found",
			Code:  http.StatusNotFound,
		})
	}

	segDir := h.storage.Path(dongleID, routeName, strconv.Itoa(segNum), "")
	path := filepath.Join(segDir, camera, file)
	return serveShareFile(c, path, file)
}

// isSafePathComponent rejects empty strings, anything containing path
// separators, and anything containing a parent-directory reference.
// dongle_id and route_name come straight from URL parameters, so the
// check is the only thing standing between a malicious caller and an
// arbitrary file under STORAGE_PATH.
func isSafePathComponent(s string) bool {
	if s == "" {
		return false
	}
	if strings.ContainsAny(s, "/\\") {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	return true
}
