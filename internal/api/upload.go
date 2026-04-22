package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/metrics"
	"comma-personal-backend/internal/storage"
)

// countingReader tallies bytes read through the upload path so the metrics
// layer can record upload_bytes_total without reading Content-Length.
type countingReader struct {
	r io.Reader
	n int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	return n, err
}

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

// filenameToFlag maps uploaded filenames to the corresponding upload flag
// field on the segment record. Compressed variants (e.g. rlog.bz2) and
// in-progress markers (e.g. fcamera.hevc~) map to the same base flag.
var filenameToFlag = map[string]string{
	"rlog":          "rlog",
	"rlog.bz2":      "rlog",
	"qlog":          "qlog",
	"qlog.bz2":      "qlog",
	"fcamera.hevc":  "fcamera",
	"fcamera.hevc~": "fcamera",
	"ecamera.hevc":  "ecamera",
	"ecamera.hevc~": "ecamera",
	"dcamera.hevc":  "dcamera",
	"dcamera.hevc~": "dcamera",
	"qcamera.ts":    "qcamera",
}

// UploadHandler holds the dependencies for upload-related HTTP handlers.
type UploadHandler struct {
	storage *storage.Storage
	queries *db.Queries
	metrics *metrics.Metrics
}

// NewUploadHandler creates an UploadHandler with the given storage backend
// and optional database queries. If queries is nil, database tracking is
// skipped (useful for tests that only exercise storage).
func NewUploadHandler(s *storage.Storage, queries *db.Queries) *UploadHandler {
	return NewUploadHandlerWithMetrics(s, queries, nil)
}

// NewUploadHandlerWithMetrics creates an UploadHandler that also records the
// number of bytes received per device via the provided metrics. A nil m is
// treated as a no-op.
func NewUploadHandlerWithMetrics(s *storage.Storage, queries *db.Queries, m *metrics.Metrics) *UploadHandler {
	return &UploadHandler{storage: s, queries: queries, metrics: m}
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

	if !validFilenames[filename] {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("unsupported file type: %s", filename),
			Code:  http.StatusBadRequest,
		})
	}

	body := c.Request().Body
	defer body.Close()

	// Count bytes through the body reader so the metric reflects what was
	// actually received (Content-Length can be missing for chunked uploads).
	counter := &countingReader{r: body}
	if err := h.storage.Store(dongleID, route, segment, filename, counter); err != nil {
		h.metrics.AddUploadBytes(dongleID, counter.n)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to store uploaded file",
			Code:  http.StatusInternalServerError,
		})
	}
	h.metrics.AddUploadBytes(dongleID, counter.n)

	// Track the upload in the database if queries are available.
	if h.queries != nil {
		if err := h.trackUpload(c.Request().Context(), dongleID, route, segment, filename); err != nil {
			// Log the error but do not fail the upload -- the file is already
			// stored on disk. DB tracking can be reconciled later.
			log.Printf("warning: failed to track upload in database: %v", err)
		}
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// trackUpload ensures the route and segment records exist in the database and
// sets the upload flag corresponding to the given filename.
func (h *UploadHandler) trackUpload(ctx context.Context, dongleID, routeName, segmentStr, filename string) error {
	segmentNum, err := strconv.Atoi(segmentStr)
	if err != nil {
		return fmt.Errorf("invalid segment number %q: %w", segmentStr, err)
	}

	// Get or create the route record.
	routeRecord, err := h.queries.GetRoute(ctx, db.GetRouteParams{
		DongleID:  dongleID,
		RouteName: routeName,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("failed to get route: %w", err)
		}
		// Route does not exist; create it.
		routeRecord, err = h.queries.CreateRoute(ctx, db.CreateRouteParams{
			DongleID:  dongleID,
			RouteName: routeName,
		})
		if err != nil {
			// A concurrent upload may have created the route between our
			// GetRoute and CreateRoute calls. Fetch it again.
			routeRecord, err = h.queries.GetRoute(ctx, db.GetRouteParams{
				DongleID:  dongleID,
				RouteName: routeName,
			})
			if err != nil {
				return fmt.Errorf("failed to get or create route: %w", err)
			}
		}
	}

	// Create the segment if it does not exist. CreateSegmentIfNotExists uses
	// ON CONFLICT DO NOTHING, which returns no rows when the segment already
	// exists. In that case we fall back to GetSegment.
	_, err = h.queries.CreateSegmentIfNotExists(ctx, db.CreateSegmentIfNotExistsParams{
		RouteID:       routeRecord.ID,
		SegmentNumber: int32(segmentNum),
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("failed to create segment: %w", err)
	}

	// Update the upload flag for this file type.
	uploadParams := buildUploadParams(routeRecord.ID, int32(segmentNum), filename)
	if uploadParams != nil {
		if err := h.queries.UpdateSegmentUpload(ctx, *uploadParams); err != nil {
			return fmt.Errorf("failed to update segment upload flag: %w", err)
		}
	}

	return nil
}

// buildUploadParams creates UpdateSegmentUploadParams with the appropriate
// flag set for the given filename. Returns nil if the filename does not map
// to a known upload flag.
func buildUploadParams(routeID, segmentNumber int32, filename string) *db.UpdateSegmentUploadParams {
	flag, ok := filenameToFlag[filename]
	if !ok {
		return nil
	}

	params := db.UpdateSegmentUploadParams{
		RouteID:       routeID,
		SegmentNumber: segmentNumber,
	}

	trueVal := pgtype.Bool{Bool: true, Valid: true}

	switch flag {
	case "rlog":
		params.RlogUploaded = trueVal
	case "qlog":
		params.QlogUploaded = trueVal
	case "fcamera":
		params.FcameraUploaded = trueVal
	case "ecamera":
		params.EcameraUploaded = trueVal
	case "dcamera":
		params.DcameraUploaded = trueVal
	case "qcamera":
		params.QcameraUploaded = trueVal
	}

	return &params
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
// Expected format: "route/segment/filename" where segment is a non-negative
// integer. Segment is validated here so invalid uploads are rejected before
// any bytes touch disk.
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

	segNum, convErr := strconv.Atoi(segment)
	if convErr != nil || segNum < 0 {
		return "", "", "", fmt.Errorf("failed to parse upload path: invalid segment number %q", segment)
	}

	return route, segment, filename, nil
}
