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
	"time"

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
// Current openpilot/sunnypilot uploads logs as .zst; older builds used .bz2;
// the raw form is rare but still permitted for compatibility.
var validFilenames = map[string]bool{
	"rlog":          true,
	"qlog":          true,
	"fcamera.hevc":  true,
	"ecamera.hevc":  true,
	"dcamera.hevc":  true,
	"qcamera.ts":    true,
	"rlog.bz2":      true,
	"qlog.bz2":      true,
	"rlog.zst":      true,
	"qlog.zst":      true,
	"fcamera.hevc~": true,
	"ecamera.hevc~": true,
	"dcamera.hevc~": true,
}

// filenameToFlag maps uploaded filenames to the corresponding upload flag
// field on the segment record. Compressed variants (e.g. rlog.bz2,
// rlog.zst) and in-progress markers (e.g. fcamera.hevc~) map to the same
// base flag.
var filenameToFlag = map[string]string{
	"rlog":          "rlog",
	"rlog.bz2":      "rlog",
	"rlog.zst":      "rlog",
	"qlog":          "qlog",
	"qlog.bz2":      "qlog",
	"qlog.zst":      "qlog",
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
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// schemeFromRequest derives the public-facing scheme for an incoming request.
// X-Forwarded-Proto wins over the on-the-wire TLS check so that requests
// arriving via a TLS-terminating reverse proxy (e.g. `tailscale serve`) still
// produce https:// URLs for the device to PUT back to.
func schemeFromRequest(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		if i := strings.Index(proto, ","); i >= 0 {
			proto = proto[:i]
		}
		return strings.TrimSpace(proto)
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// BuildSegmentUploadURL builds the self-hosted PUT URL for a single segment
// file given the request context (used to derive scheme + host) and the
// segment coordinates. It is shared by GetUploadURL (the device-facing v1.4
// endpoint) and the on-demand route data request handler so the URL shape
// stays in lockstep across both call sites.
//
// The empty headers map mirrors the shape of GetUploadURL today: openpilot's
// uploader / athenad clients accept a headers field on the upload_url
// response and forward it verbatim on the PUT, but we have nothing to add at
// this layer (the PUT handler reads only the URL path components).
func BuildSegmentUploadURL(c echo.Context, dongleID, route, segment, filename string) (string, map[string]string) {
	scheme := schemeFromRequest(c.Request())
	host := c.Request().Host
	return BuildSegmentUploadURLAt(scheme+"://"+host, dongleID, route, segment, filename)
}

// BuildBootUploadURL builds the self-hosted PUT URL for a boot log. Boot logs
// have no route or segment; they live at /upload/<dongle_id>/boot/<id>.zst.
func BuildBootUploadURL(c echo.Context, dongleID, bootID string) (string, map[string]string) {
	scheme := schemeFromRequest(c.Request())
	host := c.Request().Host
	uploadPath := fmt.Sprintf("/upload/%s/boot/%s.zst", dongleID, bootID)
	return scheme + "://" + host + uploadPath, map[string]string{}
}

// BuildSegmentUploadURLAt is the pure-function flavour of
// BuildSegmentUploadURL. It is used by the dispatcher worker, which has no
// echo.Context to crib scheme + host from. baseURL is the public origin of
// the server (e.g. "https://comma.example.com"); pass empty to fall back to
// a path-only URL the device will resolve against its own request origin.
func BuildSegmentUploadURLAt(baseURL, dongleID, route, segment, filename string) (string, map[string]string) {
	uploadPath := fmt.Sprintf("/upload/%s/%s/%s/%s", dongleID, route, segment, filename)
	uploadURL := baseURL + uploadPath
	return uploadURL, map[string]string{}
}

// BuildSignedSegmentUploadURLAt is BuildSegmentUploadURLAt plus an HMAC
// signature on the URL path so the device's anonymous PUT is still bound to
// this backend. Used by the on-demand "Get full quality" path: the request
// arrives at the device via athena RPC and the device cannot present a JWT
// (the backend doesn't have its private key), so the URL itself carries the
// authorisation. uploadSecret must be non-empty; pass through to
// BuildSegmentUploadURLAt for legacy unsigned URLs.
func BuildSignedSegmentUploadURLAt(uploadSecret []byte, baseURL, dongleID, route, segment, filename string, exp time.Time) (string, map[string]string, error) {
	uploadURL, headers := BuildSegmentUploadURLAt(baseURL, dongleID, route, segment, filename)
	uploadPath := fmt.Sprintf("/upload/%s/%s/%s/%s", dongleID, route, segment, filename)
	sig := SignUploadPath(uploadSecret, uploadPath, exp)
	signedURL, err := AppendUploadSignature(uploadURL, exp, sig)
	if err != nil {
		return "", nil, err
	}
	return signedURL, headers, nil
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

	var (
		uploadURL string
		headers   map[string]string
	)

	if strings.HasPrefix(path, "boot/") {
		bootID, err := parseBootPath(path)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errorResponse{
				Error: err.Error(),
				Code:  http.StatusBadRequest,
			})
		}
		uploadURL, headers = BuildBootUploadURL(c, dongleID, bootID)
	} else {
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

		uploadURL, headers = BuildSegmentUploadURL(c, dongleID, route, segment, filename)
	}

	// Echo the request's Authorization header so the device's PUT carries the
	// same JWT the upload_url request did. The reverse proxy may strip
	// credentials between hops; surfacing them in the response keeps the
	// upload's auth context aligned with the URL's.
	if auth := c.Request().Header.Get("Authorization"); auth != "" {
		headers["Authorization"] = auth
	}

	return c.JSON(http.StatusOK, uploadURLResponse{URL: uploadURL, Headers: headers})
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

	body := c.Request().Body
	defer body.Close()
	counter := &countingReader{r: body}

	if strings.HasPrefix(filePath, "boot/") {
		bootID, err := parseBootPath(filePath)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errorResponse{
				Error: err.Error(),
				Code:  http.StatusBadRequest,
			})
		}
		if err := h.storage.WriteBootLog(dongleID, bootID, counter); err != nil {
			h.metrics.AddUploadBytes(dongleID, counter.n)
			return c.JSON(http.StatusInternalServerError, errorResponse{
				Error: "failed to store boot log",
				Code:  http.StatusInternalServerError,
			})
		}
		h.metrics.AddUploadBytes(dongleID, counter.n)
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
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

	// Count bytes through the body reader so the metric reflects what was
	// actually received (Content-Length can be missing for chunked uploads).
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

// parseBootPath parses a boot-log path of the form "boot/<id>.zst" and
// returns the bare id. Boot logs are device-side files under
// /data/media/0/realdata/boot/ that the uploader PUTs verbatim, so the path
// has a fixed shape and any deviation (missing prefix, non-.zst extension,
// nested separator, or traversal segment) is rejected up-front to keep the
// device from steering uploads outside <basePath>/<dongleID>/boot/.
func parseBootPath(path string) (string, error) {
	const prefix = "boot/"
	if !strings.HasPrefix(path, prefix) {
		return "", fmt.Errorf("invalid boot id: missing %q prefix", prefix)
	}
	rest := strings.TrimPrefix(path, prefix)
	if !strings.HasSuffix(rest, ".zst") {
		return "", fmt.Errorf("boot log must end in .zst")
	}
	id := strings.TrimSuffix(rest, ".zst")
	if id == "" {
		return "", fmt.Errorf("invalid boot id: empty id")
	}
	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return "", fmt.Errorf("invalid boot id: %q", id)
	}
	return id, nil
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
