// Package api -- route data request handler.
//
// Today only qlog.zst and qcamera.ts auto-upload from the device. The
// full-resolution HEVC files (fcamera/ecamera/dcamera.hevc) and the full
// rlog.zst stay on the device unless the route is preserved or the operator
// pulls them manually. This handler exposes an explicit on-demand pull: a
// single backend call that, for a given route, generates upload URLs for
// every missing full-res file across every segment and instructs the device
// to enqueue them via the existing uploadFilesToUrls RPC.
//
// Two endpoints back the workflow:
//
//   - POST /v1/route/:dongle_id/:route_name/request_full_data { kind: ... }
//     dispatches the batch and persists a request row. When the device is
//     not currently online on the WS hub the row is left in 'pending' for
//     the dispatcher worker to retry; the response is 202 Accepted in that
//     case so the UI can surface a clear "queued, waiting for device" state.
//
//   - GET /v1/route/:dongle_id/:route_name/request_full_data/:request_id
//     returns the request row plus a derived per-segment progress map so
//     the polling UI does not need to hit two endpoints. It also auto
//     -completes the row when files_uploaded == files_requested.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/ws"
)

// Request kinds. Keep the constants in sync with the CHECK constraint in
// sql/migrations/010_route_data_requests.up.sql.
const (
	requestKindFullVideo = "full_video"
	requestKindFullLogs  = "full_logs"
	requestKindAll       = "all"
)

// Request statuses. Same idea: keep aligned with the CHECK constraint.
const (
	requestStatusPending    = "pending"
	requestStatusDispatched = "dispatched"
	requestStatusPartial    = "partial"
	requestStatusComplete   = "complete"
	requestStatusFailed     = "failed"
)

// idempotencyWindow is the lookback used to short-circuit duplicate POSTs.
// A non-failed request for the same (route_id, kind) inside this window is
// returned with 200 OK rather than dispatching another batch.
const idempotencyWindow = 1 * time.Hour

// fullVideoFiles lists the camera HEVC files included in a full_video request.
// These are the files the device has on disk but does NOT auto-upload.
var fullVideoFiles = []string{"fcamera.hevc", "ecamera.hevc", "dcamera.hevc"}

// fullLogsFiles lists the log files included in a full_logs request. Only the
// zstd-compressed form is requested; modern openpilot/sunnypilot devices do
// not retain uncompressed rlogs.
var fullLogsFiles = []string{"rlog.zst"}

// RouteDataRequestQueries is the subset of db.Queries the handler needs.
// Narrow interface so tests can substitute a fake without standing up a
// real Postgres pool. The methods here are exactly the queries declared in
// sql/queries/route_data_requests.sql plus the lookups required to resolve
// the route id and segment list.
type RouteDataRequestQueries interface {
	GetRoute(ctx context.Context, arg db.GetRouteParams) (db.Route, error)
	ListSegmentsByRoute(ctx context.Context, routeID int32) ([]db.Segment, error)
	CreateRouteDataRequest(ctx context.Context, arg db.CreateRouteDataRequestParams) (db.RouteDataRequest, error)
	GetRouteDataRequestByID(ctx context.Context, id int32) (db.RouteDataRequest, error)
	GetLatestRouteDataRequestByRoute(ctx context.Context, arg db.GetLatestRouteDataRequestByRouteParams) (db.RouteDataRequest, error)
	UpdateRouteDataRequestStatus(ctx context.Context, arg db.UpdateRouteDataRequestStatusParams) error
}

// DeviceUploadDispatcher is the abstraction the handler uses to push the
// batch over the WebSocket. It is satisfied in production by a small adapter
// around ws.Hub + ws.RPCCaller (see hubDispatcher below) and stubbed by
// tests so they can assert what would have been sent without standing up a
// live socket.
type DeviceUploadDispatcher interface {
	// Dispatch returns (true, nil) when the device is online and the RPC
	// succeeded. (false, nil) means the device is offline -- the handler
	// keeps the row in 'pending' so the worker can retry. A non-nil error
	// signals an RPC failure (online but the call failed) and the handler
	// records it in the row's error column with status='failed'.
	Dispatch(ctx context.Context, dongleID string, items []ws.UploadFileToUrlParams) (online bool, err error)
}

// hubDispatcher is the production DeviceUploadDispatcher: it consults the
// shared WebSocket hub for the device's live client and forwards through
// the shared RPC caller.
type hubDispatcher struct {
	hub *ws.Hub
	rpc *ws.RPCCaller
}

// NewHubDispatcher returns a DeviceUploadDispatcher backed by the given hub
// and RPC caller. A nil hub or rpc collapses Dispatch to "device offline".
func NewHubDispatcher(hub *ws.Hub, rpc *ws.RPCCaller) DeviceUploadDispatcher {
	return &hubDispatcher{hub: hub, rpc: rpc}
}

// Dispatch implements DeviceUploadDispatcher.
func (d *hubDispatcher) Dispatch(_ context.Context, dongleID string, items []ws.UploadFileToUrlParams) (bool, error) {
	if d.hub == nil || d.rpc == nil {
		return false, nil
	}
	client, ok := d.hub.GetClient(dongleID)
	if !ok || client == nil {
		return false, nil
	}
	if _, err := ws.CallUploadFilesToUrls(d.rpc, client, items); err != nil {
		return true, err
	}
	return true, nil
}

// RouteDataRequestHandler holds the dependencies for the on-demand route
// data request endpoints.
type RouteDataRequestHandler struct {
	queries       RouteDataRequestQueries
	dispatcher    DeviceUploadDispatcher
	uploadSecret  []byte
	publicBaseURL string
	now           func() time.Time
}

// NewRouteDataRequestHandler builds a handler from the live deps. Pass the
// hub-backed dispatcher in production; tests may inject a fake. uploadSecret
// is the HMAC key used to sign the upload URLs the device receives via
// athena RPC; pass nil/empty to fall back to unsigned URLs (the upload
// endpoint will then reject the device's anonymous PUT with 401).
func NewRouteDataRequestHandler(queries RouteDataRequestQueries, dispatcher DeviceUploadDispatcher, uploadSecret []byte) *RouteDataRequestHandler {
	return &RouteDataRequestHandler{
		queries:      queries,
		dispatcher:   dispatcher,
		uploadSecret: uploadSecret,
		now:          time.Now,
	}
}

// WithPublicBaseURL configures the absolute origin (e.g.
// "https://comma.example.com") that buildUploadItems prefixes every minted
// PUT URL with. Empty preserves the legacy behaviour of deriving scheme +
// host from the inbound request. Returns the receiver so it can be chained
// off the constructor.
func (h *RouteDataRequestHandler) WithPublicBaseURL(baseURL string) *RouteDataRequestHandler {
	h.publicBaseURL = baseURL
	return h
}

// RegisterRoutes wires the POST and GET endpoints on the given Echo group.
// The caller is responsible for choosing the auth middleware (see
// cmd/server/routes.go); this handler relies on checkDongleAccess for the
// per-device authorization layer.
func (h *RouteDataRequestHandler) RegisterRoutes(g *echo.Group) {
	g.POST("/:dongle_id/:route_name/request_full_data", h.RequestFullData)
	g.GET("/:dongle_id/:route_name/request_full_data/:request_id", h.GetFullDataRequest)
}

// requestFullDataBody is the JSON body accepted by the POST endpoint.
type requestFullDataBody struct {
	Kind string `json:"kind"`
}

// segmentProgress is the per-segment row in the GET response.
type segmentProgress struct {
	SegmentNumber   int32 `json:"segmentNumber"`
	FcameraUploaded bool  `json:"fcameraUploaded"`
	EcameraUploaded bool  `json:"ecameraUploaded"`
	DcameraUploaded bool  `json:"dcameraUploaded"`
	RlogUploaded    bool  `json:"rlogUploaded"`
}

// requestProgress is the aggregate progress block in the GET response.
type requestProgress struct {
	FilesRequested int32 `json:"filesRequested"`
	FilesUploaded  int32 `json:"filesUploaded"`
	Percent        int32 `json:"percent"`
}

// requestRowResponse is the JSON view of a route_data_requests row. The
// pgtype-flavored fields are flattened into nullable scalars because the
// frontend has no use for the pgtype wrapper shape.
type requestRowResponse struct {
	ID             int32      `json:"id"`
	RouteID        int32      `json:"routeId"`
	RequestedBy    *string    `json:"requestedBy"`
	RequestedAt    *time.Time `json:"requestedAt"`
	Kind           string     `json:"kind"`
	Status         string     `json:"status"`
	DispatchedAt   *time.Time `json:"dispatchedAt"`
	CompletedAt    *time.Time `json:"completedAt"`
	Error          *string    `json:"error"`
	FilesRequested int32      `json:"filesRequested"`
}

// requestStatusResponse is the GET endpoint's body shape.
type requestStatusResponse struct {
	Request  requestRowResponse `json:"request"`
	Progress requestProgress    `json:"progress"`
	Segments []segmentProgress  `json:"segments"`
}

// requestPostResponse is the POST endpoint's body shape. Returns the created
// (or reused) row plus how many files the device was asked to upload.
type requestPostResponse struct {
	Request requestRowResponse `json:"request"`
}

// RequestFullData handles POST /v1/route/:dongle_id/:route_name/request_full_data.
func (h *RouteDataRequestHandler) RequestFullData(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	var body requestFullDataBody
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}
	wanted, err := filesForKind(body.Kind)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: err.Error(),
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()

	route, err := h.queries.GetRoute(ctx, db.GetRouteParams{
		DongleID:  dongleID,
		RouteName: routeName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: fmt.Sprintf("route %s not found", routeName),
				Code:  http.StatusNotFound,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to retrieve route",
			Code:  http.StatusInternalServerError,
		})
	}

	// Idempotency: a non-failed request for the same (route_id, kind) inside
	// the idempotency window is returned as-is. This guards against the user
	// double-clicking the UI button or a flaky network retrying a POST.
	existing, err := h.queries.GetLatestRouteDataRequestByRoute(ctx, db.GetLatestRouteDataRequestByRouteParams{
		RouteID: route.ID,
		Kind:    body.Kind,
	})
	if err == nil && existing.Status != requestStatusFailed && h.now().Sub(existing.RequestedAt.Time) < idempotencyWindow {
		return c.JSON(http.StatusOK, requestPostResponse{Request: rowToResponse(existing)})
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to look up existing request",
			Code:  http.StatusInternalServerError,
		})
	}

	segments, err := h.queries.ListSegmentsByRoute(ctx, route.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list segments",
			Code:  http.StatusInternalServerError,
		})
	}

	items := h.buildUploadItems(c, dongleID, routeName, segments, wanted)

	requestedBy := requesterFromContext(c)

	// Try to dispatch immediately if the device is online. Offline -> row
	// stays 'pending' and the dispatcher worker will retry later (and the
	// response is 202 Accepted). Online + RPC error -> 'failed' with the
	// error captured for the UI. Online + RPC success -> 'dispatched'.
	online, dispatchErr := h.dispatcher.Dispatch(ctx, dongleID, items)

	createParams := db.CreateRouteDataRequestParams{
		RouteID:        route.ID,
		RequestedBy:    requestedBy,
		Kind:           body.Kind,
		Status:         requestStatusPending,
		FilesRequested: int32(len(items)),
	}

	switch {
	case !online:
		// Leave status=pending; the worker will retry.
	case dispatchErr != nil:
		createParams.Status = requestStatusFailed
		createParams.Error = pgtype.Text{String: dispatchErr.Error(), Valid: true}
	default:
		createParams.Status = requestStatusDispatched
		createParams.DispatchedAt = pgtype.Timestamptz{Time: h.now().UTC(), Valid: true}
	}

	row, err := h.queries.CreateRouteDataRequest(ctx, createParams)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to persist route data request",
			Code:  http.StatusInternalServerError,
		})
	}

	resp := requestPostResponse{Request: rowToResponse(row)}
	if !online {
		return c.JSON(http.StatusAccepted, resp)
	}
	return c.JSON(http.StatusCreated, resp)
}

// GetFullDataRequest handles
// GET /v1/route/:dongle_id/:route_name/request_full_data/:request_id.
func (h *RouteDataRequestHandler) GetFullDataRequest(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")
	requestIDStr := c.Param("request_id")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	requestID, err := strconv.Atoi(requestIDStr)
	if err != nil || requestID <= 0 {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid request_id",
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()

	row, err := h.queries.GetRouteDataRequestByID(ctx, int32(requestID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: "request not found",
				Code:  http.StatusNotFound,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to retrieve request",
			Code:  http.StatusInternalServerError,
		})
	}

	// Re-validate that the request actually belongs to the (dongle, route)
	// pair on the URL. Without this a session caller could enumerate
	// request ids cross-route, and a JWT caller could read another route's
	// row by guessing the id.
	route, err := h.queries.GetRoute(ctx, db.GetRouteParams{
		DongleID:  dongleID,
		RouteName: routeName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: fmt.Sprintf("route %s not found", routeName),
				Code:  http.StatusNotFound,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to retrieve route",
			Code:  http.StatusInternalServerError,
		})
	}
	if row.RouteID != route.ID {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "request not found",
			Code:  http.StatusNotFound,
		})
	}

	segments, err := h.queries.ListSegmentsByRoute(ctx, route.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list segments",
			Code:  http.StatusInternalServerError,
		})
	}

	uploaded := countUploadedForKind(segments, row.Kind)
	progress := requestProgress{
		FilesRequested: row.FilesRequested,
		FilesUploaded:  uploaded,
	}
	if row.FilesRequested > 0 {
		progress.Percent = int32((int64(uploaded) * 100) / int64(row.FilesRequested))
	} else {
		progress.Percent = 100
	}

	// Auto-complete: when every requested file has uploaded, stamp the row
	// so subsequent reads do not have to recompute the same arithmetic.
	// Skip when the row is already in a terminal state.
	if row.Status != requestStatusComplete && row.Status != requestStatusFailed &&
		row.FilesRequested > 0 && uploaded >= row.FilesRequested {
		updateParams := db.UpdateRouteDataRequestStatusParams{
			ID:          row.ID,
			Status:      requestStatusComplete,
			CompletedAt: pgtype.Timestamptz{Time: h.now().UTC(), Valid: true},
		}
		if err := h.queries.UpdateRouteDataRequestStatus(ctx, updateParams); err == nil {
			row.Status = requestStatusComplete
			row.CompletedAt = updateParams.CompletedAt
		}
		// On error: serve the stale row -- the next GET will retry the
		// transition. Failing the read because we could not write would
		// be worse for the polling UI.
	}

	resp := requestStatusResponse{
		Request:  rowToResponse(row),
		Progress: progress,
		Segments: segmentsToProgress(segments),
	}
	return c.JSON(http.StatusOK, resp)
}

// filesForKind maps the request kind to the per-segment file list. An empty
// or unknown kind is rejected so the handler does not silently no-op.
func filesForKind(kind string) ([]string, error) {
	switch kind {
	case requestKindFullVideo:
		out := make([]string, len(fullVideoFiles))
		copy(out, fullVideoFiles)
		return out, nil
	case requestKindFullLogs:
		out := make([]string, len(fullLogsFiles))
		copy(out, fullLogsFiles)
		return out, nil
	case requestKindAll:
		out := make([]string, 0, len(fullVideoFiles)+len(fullLogsFiles))
		out = append(out, fullVideoFiles...)
		out = append(out, fullLogsFiles...)
		return out, nil
	case "":
		return nil, fmt.Errorf("missing required field: kind")
	default:
		return nil, fmt.Errorf("unsupported kind %q (allowed: full_video, full_logs, all)", kind)
	}
}

// segmentHasFile returns true when the segment's upload flag for the given
// filename is already set, so the caller can skip enqueuing it. Filenames
// outside the known set return false (the handler will skip them anyway via
// validFilenames -- but keep this defensive in case the constants drift).
func segmentHasFile(seg db.Segment, filename string) bool {
	switch filename {
	case "fcamera.hevc":
		return seg.FcameraUploaded
	case "ecamera.hevc":
		return seg.EcameraUploaded
	case "dcamera.hevc":
		return seg.DcameraUploaded
	case "rlog.zst":
		return seg.RlogUploaded
	default:
		return false
	}
}

// buildUploadItems constructs the slice of UploadFileToUrlParams the device
// expects for an uploadFilesToUrls RPC. fn is "<route_name>--<seg>/<filename>"
// so the device joins it with its log_root (Paths.log_root() in athenad.py)
// to find the on-disk file.
func (h *RouteDataRequestHandler) buildUploadItems(c echo.Context, dongleID, routeName string, segments []db.Segment, wanted []string) []ws.UploadFileToUrlParams {
	return BuildUploadItemsAt(resolveBaseURL(h.publicBaseURL, c), h.uploadSecret, dongleID, routeName, segments, wanted)
}

// BuildUploadItemsAt is the pure-function flavour of buildUploadItems used by
// the dispatcher worker, which has no echo.Context to crib scheme + host
// from. See BuildSegmentUploadURLAt for the baseURL contract. uploadSecret
// is the HMAC key used to sign upload URLs; pass nil/empty to fall back to
// unsigned URLs (only useful when the upload route accepts unauthenticated
// PUTs, which is not the default).
func BuildUploadItemsAt(baseURL string, uploadSecret []byte, dongleID, routeName string, segments []db.Segment, wanted []string) []ws.UploadFileToUrlParams {
	exp := time.Now().Add(UploadSignatureTTL)
	items := make([]ws.UploadFileToUrlParams, 0, len(segments)*len(wanted))
	for _, seg := range segments {
		segStr := strconv.Itoa(int(seg.SegmentNumber))
		for _, filename := range wanted {
			if segmentHasFile(seg, filename) {
				continue
			}
			var (
				url     string
				headers map[string]string
			)
			if len(uploadSecret) > 0 {
				signed, h, err := BuildSignedSegmentUploadURLAt(uploadSecret, baseURL, dongleID, routeName, segStr, filename, exp)
				if err != nil {
					url, headers = BuildSegmentUploadURLAt(baseURL, dongleID, routeName, segStr, filename)
				} else {
					url, headers = signed, h
				}
			} else {
				url, headers = BuildSegmentUploadURLAt(baseURL, dongleID, routeName, segStr, filename)
			}
			items = append(items, ws.UploadFileToUrlParams{
				URL:     url,
				Headers: headers,
				Path:    fmt.Sprintf("%s--%s/%s", routeName, segStr, filename),
			})
		}
	}
	return items
}

// FilesForKind exposes the kind-to-files mapping for callers outside the
// api package (e.g. the dispatcher worker). Returns an error for unknown
// kinds so the caller can short-circuit before doing more work.
func FilesForKind(kind string) ([]string, error) {
	return filesForKind(kind)
}

// countUploadedForKind sums the upload flags relevant to the request kind so
// the GET endpoint can report progress without re-deriving the file list.
func countUploadedForKind(segments []db.Segment, kind string) int32 {
	wanted, err := filesForKind(kind)
	if err != nil {
		return 0
	}
	var n int32
	for _, seg := range segments {
		for _, filename := range wanted {
			if segmentHasFile(seg, filename) {
				n++
			}
		}
	}
	return n
}

// segmentsToProgress projects the segments slice into the GET response
// shape. The order matches segments (already sorted by segment_number on
// the query side).
func segmentsToProgress(segments []db.Segment) []segmentProgress {
	out := make([]segmentProgress, 0, len(segments))
	for _, seg := range segments {
		out = append(out, segmentProgress{
			SegmentNumber:   seg.SegmentNumber,
			FcameraUploaded: seg.FcameraUploaded,
			EcameraUploaded: seg.EcameraUploaded,
			DcameraUploaded: seg.DcameraUploaded,
			RlogUploaded:    seg.RlogUploaded,
		})
	}
	return out
}

// rowToResponse converts the sqlc row into the JSON shape the frontend
// consumes, flattening the pgtype.* nullables into pointers.
func rowToResponse(row db.RouteDataRequest) requestRowResponse {
	resp := requestRowResponse{
		ID:             row.ID,
		RouteID:        row.RouteID,
		Kind:           row.Kind,
		Status:         row.Status,
		FilesRequested: row.FilesRequested,
	}
	if row.RequestedBy.Valid {
		s := row.RequestedBy.String
		resp.RequestedBy = &s
	}
	if row.RequestedAt.Valid {
		t := row.RequestedAt.Time
		resp.RequestedAt = &t
	}
	if row.DispatchedAt.Valid {
		t := row.DispatchedAt.Time
		resp.DispatchedAt = &t
	}
	if row.CompletedAt.Valid {
		t := row.CompletedAt.Time
		resp.CompletedAt = &t
	}
	if row.Error.Valid {
		s := row.Error.String
		resp.Error = &s
	}
	return resp
}

// requesterFromContext records who requested the pull. For session callers
// we stamp "user:<id>" (the username is not in the Echo context, only the
// numeric ui_users.id), which is enough to attribute a request later
// without an extra DB round-trip on every POST. For device JWT callers we
// leave the column NULL because the dongle is already pinned through
// route_id and there is no human to attribute it to.
func requesterFromContext(c echo.Context) pgtype.Text {
	mode, _ := c.Get(middleware.ContextKeyAuthMode).(string)
	if mode != middleware.AuthModeSession {
		return pgtype.Text{}
	}
	userID, ok := c.Get(middleware.ContextKeyUserID).(int32)
	if !ok || userID == 0 {
		return pgtype.Text{}
	}
	return pgtype.Text{String: fmt.Sprintf("user:%d", userID), Valid: true}
}
