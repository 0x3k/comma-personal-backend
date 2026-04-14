package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/storage"
)

// validFilenames lists the segment file types that comma devices may upload.
var validFilenames = map[string]bool{
	"rlog":          true,
	"qlog":          true,
	"fcamera.hevc":  true,
	"ecamera.hevc":  true,
	"dcamera.hevc":  true,
	"qcamera.ts":    true,
	"rlog.bz2":      true,
	"qlog.bz2":      true,
	"fcamera.hevc~": true,
	"ecamera.hevc~": true,
	"dcamera.hevc~": true,
}

// UploadHandler holds the dependencies for upload-related HTTP handlers.
type UploadHandler struct {
	storage *storage.Storage
}

// NewUploadHandler creates an UploadHandler with the given storage backend.
func NewUploadHandler(s *storage.Storage) *UploadHandler {
	return &UploadHandler{storage: s}
}

// errorResponse is the JSON envelope returned on error.
type errorResponse struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// uploadURLResponse is the JSON response for the upload URL endpoint.
type uploadURLResponse struct {
	URL string `json:"url"`
}

// GetUploadURL handles GET /v1.4/:dongle_id/upload_url/ and returns a URL
// that the device should PUT the file to. The path query parameter specifies
// the relative file path (e.g. "2024-03-15--12-30-00--0/fcamera.hevc").
func (h *UploadHandler) GetUploadURL(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	authDongleID, _ := c.Get(middleware.ContextKeyDongleID).(string)
	if authDongleID != dongleID {
		return c.JSON(http.StatusForbidden, errorResponse{
			Error: "dongle_id does not match authenticated device",
			Code:  http.StatusForbidden,
		})
	}

	path := c.QueryParam("path")
	if path == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "missing required query parameter: path",
			Code:  http.StatusBadRequest,
		})
	}

	// Parse path: expected format is "route_name/segment/filename" or
	// "route_name--segment/filename" (segment embedded in route name).
	// The comma device sends paths like "2024-03-15--12-30-00--0/fcamera.hevc"
	// where the segment number is appended to the route timestamp.
	route, segment, filename, err := parsePath(path)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: err.Error(),
			Code:  http.StatusBadRequest,
		})
	}

	if !validFilenames[filename] {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("unsupported file type: %s", filename),
			Code:  http.StatusBadRequest,
		})
	}

	// Build a self-hosted upload URL pointing back at this server.
	scheme := "http"
	if c.Request().TLS != nil {
		scheme = "https"
	}
	host := c.Request().Host
	uploadURL := fmt.Sprintf("%s://%s/upload/%s/%s/%s/%s",
		scheme, host, dongleID, route, segment, filename)

	return c.JSON(http.StatusOK, uploadURLResponse{URL: uploadURL})
}

// UploadFile handles PUT /upload/:dongle_id/* and stores the uploaded file
// via the storage layer. The path after the dongle_id should contain
// route/segment/filename components.
func (h *UploadHandler) UploadFile(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	authDongleID, _ := c.Get(middleware.ContextKeyDongleID).(string)
	if authDongleID != dongleID {
		return c.JSON(http.StatusForbidden, errorResponse{
			Error: "dongle_id does not match authenticated device",
			Code:  http.StatusForbidden,
		})
	}

	// The wildcard param captures everything after /upload/:dongle_id/
	filePath := c.Param("*")
	if filePath == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "missing file path in URL",
			Code:  http.StatusBadRequest,
		})
	}

	route, segment, filename, err := parseUploadPath(filePath)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: err.Error(),
			Code:  http.StatusBadRequest,
		})
	}

	body := c.Request().Body
	defer body.Close()

	if err := h.storage.Store(dongleID, route, segment, filename, body); err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to store uploaded file",
			Code:  http.StatusInternalServerError,
		})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// parsePath parses the path query parameter from the upload URL request.
// The comma device sends paths like "2024-03-15--12-30-00--0/fcamera.hevc"
// where the format is "route_timestamp--segment_number/filename".
func parsePath(path string) (route, segment, filename string, err error) {
	// Split on "/" to separate the route+segment from the filename.
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("failed to parse path: expected format route--segment/filename, got %q", path)
	}

	filename = parts[1]
	routeSegment := parts[0]

	// The route+segment string looks like "2024-03-15--12-30-00--0"
	// where the last "--N" is the segment number. Split on the last "--".
	lastSep := strings.LastIndex(routeSegment, "--")
	if lastSep < 0 || lastSep == len(routeSegment)-2 {
		return "", "", "", fmt.Errorf("failed to parse path: no segment number in %q", routeSegment)
	}

	route = routeSegment[:lastSep]
	segment = routeSegment[lastSep+2:]

	if route == "" || segment == "" {
		return "", "", "", fmt.Errorf("failed to parse path: empty route or segment in %q", routeSegment)
	}

	return route, segment, filename, nil
}

// parseUploadPath parses the path from the upload URL (after /upload/:dongle_id/).
// Expected format: "route/segment/filename".
func parseUploadPath(path string) (route, segment, filename string, err error) {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("failed to parse upload path: expected route/segment/filename, got %q", path)
	}

	route = parts[0]
	segment = parts[1]
	filename = parts[2]

	if route == "" || segment == "" || filename == "" {
		return "", "", "", fmt.Errorf("failed to parse upload path: empty component in %q", path)
	}

	return route, segment, filename, nil
}
