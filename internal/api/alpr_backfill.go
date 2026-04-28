// Package api -- alpr_backfill.go exposes the operator-facing controls for
// the ALPR historical backfill: start a job over an existing route set,
// poll its status (with throughput-derived ETA), pause/resume the
// cooperative state machine, and cancel a stuck or unwanted job. The
// worker that consumes these controls lives in
// internal/worker/alpr_backfill.go and is started from
// cmd/server/workers.go.
//
// # Endpoint surface
//
// All routes are session-only -- the operator is the only legitimate
// initiator. A device JWT must never start, pause, or cancel a backfill.
//
//	POST   /v1/alpr/backfill/start   {from_date?, to_date?, dongle_id?, max_routes?, newest_first?}
//	                                 -> 200 {job_id} | 409 if a job is already running
//	GET    /v1/alpr/backfill/status  -> 200 {job_id, started_at, finished_at, state,
//	                                          total_routes, processed_routes,
//	                                          last_processed_route, eta_seconds, error}
//	                                  -> 200 {state: "none"} when no job has ever been recorded
//	POST   /v1/alpr/backfill/pause   -> 200 {state}     (no-op if already paused/done/failed)
//	POST   /v1/alpr/backfill/resume  -> 200 {state} | 409 if no resumable job exists
//	POST   /v1/alpr/backfill/cancel  -> 200 {state}     (sets failed + error="cancelled by user")
//
// # Singleton enforcement
//
// "At most one running job" is enforced authoritatively at the database
// layer by the partial unique index alpr_backfill_jobs_one_running.
// The handler also runs a fast-path GetRunningBackfillJob check so the
// happy 409 response does not require a failed INSERT roundtrip.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// ALPRBackfillFilters is the parsed start-request body. The same shape
// is round-tripped through alpr_backfill_jobs.filters_json so the worker
// can read it back on every pause/resume without re-reading the original
// HTTP body. Fields are pointers / zero-valued strings so the JSON
// "field omitted" semantics are preserved through the round trip --
// e.g. an absent dongle_id means "all dongles", not "the empty
// dongle".
type ALPRBackfillFilters struct {
	// FromDate filters routes to those whose start_time is at or after
	// this instant. Nil disables the filter.
	FromDate *time.Time `json:"from_date,omitempty"`
	// ToDate filters routes to those whose start_time is strictly
	// before this instant. Nil disables the filter.
	ToDate *time.Time `json:"to_date,omitempty"`
	// DongleID restricts the backfill to a single device. Empty string
	// (the JSON-omitted default) disables the filter.
	DongleID string `json:"dongle_id,omitempty"`
	// MaxRoutes caps the number of routes the worker will process. 0
	// (the JSON-omitted default) means unlimited.
	MaxRoutes int `json:"max_routes,omitempty"`
	// NewestFirst flips the default oldest-first order so a user who
	// just enabled ALPR and wants results today processes recent
	// routes first.
	NewestFirst bool `json:"newest_first,omitempty"`
}

// ALPRBackfillStartResponse is the body returned on successful POST
// /v1/alpr/backfill/start. job_id is the row id of the newly inserted
// alpr_backfill_jobs row; clients can immediately poll status with no
// further parameters because GET /v1/alpr/backfill/status is "the
// latest job".
type ALPRBackfillStartResponse struct {
	JobID int64 `json:"job_id"`
}

// ALPRBackfillStatusResponse is the body returned by GET
// /v1/alpr/backfill/status. eta_seconds is null when throughput is not
// yet measurable (just-started job, or zero processed_routes); 999999
// is the cap returned when computed seconds would otherwise overflow
// the dashboard display.
type ALPRBackfillStatusResponse struct {
	JobID              int64   `json:"job_id"`
	StartedAt          string  `json:"started_at"`
	FinishedAt         *string `json:"finished_at"`
	State              string  `json:"state"`
	TotalRoutes        *int32  `json:"total_routes"`
	ProcessedRoutes    int32   `json:"processed_routes"`
	LastProcessedRoute *string `json:"last_processed_route"`
	ETASeconds         *int64  `json:"eta_seconds"`
	Error              *string `json:"error"`
}

// ALPRBackfillNoJobResponse is the body returned when GET
// /v1/alpr/backfill/status is hit before any job has ever been inserted.
// The frontend uses state="none" as a sentinel to render "no backfill
// has run".
type ALPRBackfillNoJobResponse struct {
	State string `json:"state"`
}

// ALPRBackfillStateResponse is the body returned from pause/resume/
// cancel. Returning the post-update state lets the dashboard reflect
// the change without an extra round-trip.
type ALPRBackfillStateResponse struct {
	State string `json:"state"`
}

// alprBackfillETACap is the saturating upper bound on the ETA value we
// return to clients. Without this an absurdly slow throughput (one
// route per hour with a million routes left) would yield a 36-billion
// second integer the dashboard cannot display sensibly.
const alprBackfillETACap = int64(999999)

// alprBackfillRoutesPerBatch is the page size the API handler uses
// when computing total_routes for max-routes-capped jobs. The full
// CountBackfillRoutes is used when max_routes is unlimited; capped
// jobs only need an upper bound up to max_routes.
const alprBackfillRoutesPerBatch = 1000

// alprBackfillQuerier is the slice of *db.Queries that the handler
// needs. Carved out as an interface so tests can pass a small in-
// memory fake instead of standing up Postgres.
type alprBackfillQuerier interface {
	InsertBackfillJob(ctx context.Context, arg db.InsertBackfillJobParams) (db.AlprBackfillJob, error)
	GetLatestBackfillJob(ctx context.Context) (db.AlprBackfillJob, error)
	GetRunningBackfillJob(ctx context.Context) (db.AlprBackfillJob, error)
	UpdateBackfillJobState(ctx context.Context, arg db.UpdateBackfillJobStateParams) error
	CountBackfillRoutes(ctx context.Context, arg db.CountBackfillRoutesParams) (int64, error)
}

// Compile-time check that *db.Queries satisfies the handler's contract.
var _ alprBackfillQuerier = (*db.Queries)(nil)

// ALPRBackfillTrigger is the wakeup signal the worker exposes to the
// API. After a successful start INSERT the handler calls Wake() so the
// singleton goroutine notices the new job without waiting for its
// next periodic re-check. Nil-safe (a nil trigger means the worker
// will pick up the new row on its own poll cadence; the API still
// returns 200).
type ALPRBackfillTrigger interface {
	Wake()
}

// ALPRBackfillHandler exposes the operator-facing endpoints for the
// historical-backfill worker. Construct via NewALPRBackfillHandler.
type ALPRBackfillHandler struct {
	queries alprBackfillQuerier
	trigger ALPRBackfillTrigger
}

// NewALPRBackfillHandler wires the handler. trigger may be nil in tests
// or when the worker is intentionally not started; the handler still
// writes the row, the worker just won't notice it until its next poll.
func NewALPRBackfillHandler(q alprBackfillQuerier, trigger ALPRBackfillTrigger) *ALPRBackfillHandler {
	return &ALPRBackfillHandler{queries: q, trigger: trigger}
}

// RegisterRoutes mounts the five backfill endpoints on the given group.
// The group MUST require a session cookie (sessionOnly in the parent
// router) -- a device JWT must never start, pause, or cancel a
// backfill.
func (h *ALPRBackfillHandler) RegisterRoutes(g *echo.Group) {
	g.POST("/alpr/backfill/start", h.Start)
	g.GET("/alpr/backfill/status", h.Status)
	g.POST("/alpr/backfill/pause", h.Pause)
	g.POST("/alpr/backfill/resume", h.Resume)
	g.POST("/alpr/backfill/cancel", h.Cancel)
}

// Start handles POST /v1/alpr/backfill/start. The handler:
//  1. Validates the filter body (ToDate >= FromDate, MaxRoutes >= 0).
//  2. Fast-paths a 409 by checking GetRunningBackfillJob; this avoids
//     a unique-violation roundtrip on the common contention case.
//  3. Computes total_routes via CountBackfillRoutes, applying the
//     max_routes cap if set.
//  4. Inserts the job row in state='running'. The partial unique
//     index is the authoritative singleton guard; a 23505 here is
//     translated to 409 (a race against the fast-path check).
//  5. Wakes the worker (if a trigger was wired in) so the new job is
//     picked up immediately rather than on the next periodic re-check.
func (h *ALPRBackfillHandler) Start(c echo.Context) error {
	var req ALPRBackfillFilters
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	if req.FromDate != nil && req.ToDate != nil && req.ToDate.Before(*req.FromDate) {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "to_date must be on or after from_date",
			Code:  http.StatusBadRequest,
		})
	}
	if req.MaxRoutes < 0 {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "max_routes must be >= 0",
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()

	// Fast-path 409: a single SELECT on the partial unique index is
	// cheaper than the failed INSERT we would otherwise pay.
	if _, err := h.queries.GetRunningBackfillJob(ctx); err == nil {
		return c.JSON(http.StatusConflict, errorResponse{
			Error: "a backfill job is already running",
			Code:  http.StatusConflict,
		})
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to check running backfill job",
			Code:  http.StatusInternalServerError,
		})
	}

	// total_routes is snapshotted at start time so progress and ETA
	// remain stable even if new routes arrive mid-backfill.
	countParams := buildCountParams(req, "")
	total, err := h.queries.CountBackfillRoutes(ctx, countParams)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to count routes for backfill",
			Code:  http.StatusInternalServerError,
		})
	}
	if req.MaxRoutes > 0 && total > int64(req.MaxRoutes) {
		total = int64(req.MaxRoutes)
	}

	filtersJSON, err := json.Marshal(req)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to encode backfill filters",
			Code:  http.StatusInternalServerError,
		})
	}

	job, err := h.queries.InsertBackfillJob(ctx, db.InsertBackfillJobParams{
		State:       "running",
		FiltersJson: filtersJSON,
		TotalRoutes: pgtype.Int4{Int32: int32(total), Valid: true},
		StartedBy:   actorFromContext(c),
	})
	if err != nil {
		// A unique-violation here means another start raced past our
		// fast-path SELECT. Translate to 409 so the client sees the
		// same response either way.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return c.JSON(http.StatusConflict, errorResponse{
				Error: "a backfill job is already running",
				Code:  http.StatusConflict,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to insert backfill job",
			Code:  http.StatusInternalServerError,
		})
	}

	// Soft trigger: the worker will notice via its periodic re-check
	// even if Wake() is a no-op or the channel is full.
	if h.trigger != nil {
		h.trigger.Wake()
	}

	return c.JSON(http.StatusOK, ALPRBackfillStartResponse{JobID: job.ID})
}

// Status handles GET /v1/alpr/backfill/status. Returns the latest job
// row plus a computed eta_seconds derived from observed throughput.
// When no job has ever been inserted, returns 200 with a small
// {state: "none"} envelope so the dashboard does not have to special-
// case 404s.
func (h *ALPRBackfillHandler) Status(c echo.Context) error {
	ctx := c.Request().Context()
	job, err := h.queries.GetLatestBackfillJob(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusOK, ALPRBackfillNoJobResponse{State: "none"})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read backfill status",
			Code:  http.StatusInternalServerError,
		})
	}
	return c.JSON(http.StatusOK, statusFromJob(job, time.Now()))
}

// Pause handles POST /v1/alpr/backfill/pause. Only a job currently in
// state='running' can be paused; for any other state we return the
// current state without modification (idempotent for the dashboard).
func (h *ALPRBackfillHandler) Pause(c echo.Context) error {
	return h.transitionFrom(c, "running", "paused", false, "")
}

// Resume handles POST /v1/alpr/backfill/resume. Only a job currently in
// state='paused' can be resumed. For any other state we return 409 so
// the dashboard can prompt the user (e.g. "no paused job to resume").
func (h *ALPRBackfillHandler) Resume(c echo.Context) error {
	ctx := c.Request().Context()
	job, err := h.queries.GetLatestBackfillJob(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusConflict, errorResponse{
				Error: "no paused backfill job to resume",
				Code:  http.StatusConflict,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read backfill state",
			Code:  http.StatusInternalServerError,
		})
	}
	if job.State != "paused" {
		return c.JSON(http.StatusConflict, errorResponse{
			Error: "no paused backfill job to resume",
			Code:  http.StatusConflict,
		})
	}
	if err := h.queries.UpdateBackfillJobState(ctx, db.UpdateBackfillJobStateParams{
		ID:    job.ID,
		State: "running",
	}); err != nil {
		// Resume races against a parallel start (the singleton index
		// guards). Translate the unique violation to 409 so the
		// dashboard can refresh the status and try again.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return c.JSON(http.StatusConflict, errorResponse{
				Error: "a backfill job is already running",
				Code:  http.StatusConflict,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to resume backfill job",
			Code:  http.StatusInternalServerError,
		})
	}
	if h.trigger != nil {
		h.trigger.Wake()
	}
	return c.JSON(http.StatusOK, ALPRBackfillStateResponse{State: "running"})
}

// Cancel handles POST /v1/alpr/backfill/cancel. Sets the latest job's
// state to 'failed' with error='cancelled by user' and stamps
// finished_at so a fresh start can succeed. Idempotent for terminal
// states (done/failed): the existing state is returned unchanged.
func (h *ALPRBackfillHandler) Cancel(c echo.Context) error {
	ctx := c.Request().Context()
	job, err := h.queries.GetLatestBackfillJob(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusConflict, errorResponse{
				Error: "no backfill job to cancel",
				Code:  http.StatusConflict,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read backfill state",
			Code:  http.StatusInternalServerError,
		})
	}
	if job.State == "done" || job.State == "failed" {
		return c.JSON(http.StatusOK, ALPRBackfillStateResponse{State: job.State})
	}
	now := time.Now().UTC()
	if err := h.queries.UpdateBackfillJobState(ctx, db.UpdateBackfillJobStateParams{
		ID:         job.ID,
		State:      "failed",
		FinishedAt: pgtype.Timestamptz{Time: now, Valid: true},
		ErrorText:  pgtype.Text{String: "cancelled by user", Valid: true},
	}); err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to cancel backfill job",
			Code:  http.StatusInternalServerError,
		})
	}
	return c.JSON(http.StatusOK, ALPRBackfillStateResponse{State: "failed"})
}

// transitionFrom is the shared body for pause: read the latest job, if
// it is in fromState write toState, otherwise return the current state
// unchanged. setFinished controls whether the transition stamps
// finished_at; pause never does, but the helper is shape-compatible
// with future transitions that do (e.g. an explicit "mark done").
func (h *ALPRBackfillHandler) transitionFrom(c echo.Context, fromState, toState string, setFinished bool, errMessage string) error {
	ctx := c.Request().Context()
	job, err := h.queries.GetLatestBackfillJob(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusOK, ALPRBackfillStateResponse{State: "none"})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read backfill state",
			Code:  http.StatusInternalServerError,
		})
	}
	if job.State != fromState {
		return c.JSON(http.StatusOK, ALPRBackfillStateResponse{State: job.State})
	}
	params := db.UpdateBackfillJobStateParams{
		ID:    job.ID,
		State: toState,
	}
	if setFinished {
		params.FinishedAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	if errMessage != "" {
		params.ErrorText = pgtype.Text{String: errMessage, Valid: true}
	}
	if err := h.queries.UpdateBackfillJobState(ctx, params); err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to transition backfill job",
			Code:  http.StatusInternalServerError,
		})
	}
	return c.JSON(http.StatusOK, ALPRBackfillStateResponse{State: toState})
}

// buildCountParams converts the API filter struct into the sqlc-
// generated params for CountBackfillRoutes. afterRoute is the resume
// token; pass an empty string at start time and last_processed_route
// when re-counting on resume.
func buildCountParams(f ALPRBackfillFilters, afterRoute string) db.CountBackfillRoutesParams {
	p := db.CountBackfillRoutesParams{}
	if f.FromDate != nil {
		p.FromDate = pgtype.Timestamptz{Time: *f.FromDate, Valid: true}
	}
	if f.ToDate != nil {
		p.ToDate = pgtype.Timestamptz{Time: *f.ToDate, Valid: true}
	}
	if strings.TrimSpace(f.DongleID) != "" {
		p.DongleID = pgtype.Text{String: f.DongleID, Valid: true}
	}
	if afterRoute != "" {
		p.AfterRoute = pgtype.Text{String: afterRoute, Valid: true}
	}
	return p
}

// statusFromJob converts a job row into the API response shape and
// computes eta_seconds from observed throughput. now is taken as a
// parameter so tests can pin time deterministically.
//
// ETA math: routes_per_minute = processed_routes / elapsed_minutes;
// eta_seconds = (total_routes - processed_routes) / routes_per_minute
// * 60. Capped at alprBackfillETACap so an absurdly slow throughput
// does not produce overflow-looking values in the dashboard.
//
// When throughput cannot be computed (zero processed_routes, zero
// elapsed time, terminal state) we return nil so the JSON serializes
// as null and the frontend renders "computing..." instead of "0s".
func statusFromJob(job db.AlprBackfillJob, now time.Time) ALPRBackfillStatusResponse {
	resp := ALPRBackfillStatusResponse{
		JobID:           job.ID,
		StartedAt:       job.StartedAt.Time.UTC().Format(time.RFC3339),
		State:           job.State,
		ProcessedRoutes: job.ProcessedRoutes,
	}
	if job.FinishedAt.Valid {
		s := job.FinishedAt.Time.UTC().Format(time.RFC3339)
		resp.FinishedAt = &s
	}
	if job.TotalRoutes.Valid {
		v := job.TotalRoutes.Int32
		resp.TotalRoutes = &v
	}
	if job.LastProcessedRoute.Valid {
		v := job.LastProcessedRoute.String
		resp.LastProcessedRoute = &v
	}
	if job.Error.Valid {
		v := job.Error.String
		resp.Error = &v
	}

	// ETA only makes sense for an actively progressing job. Terminal
	// states return nil so the dashboard doesn't show a stale ETA on
	// a finished row.
	if job.State == "running" || job.State == "paused" {
		eta := computeETASeconds(job, now)
		if eta != nil {
			resp.ETASeconds = eta
		}
	}
	return resp
}

// computeETASeconds returns the throughput-derived ETA, or nil when
// throughput is not yet measurable. Exposed as a package-level helper
// so tests can pin behaviour without going through the full HTTP
// path.
func computeETASeconds(job db.AlprBackfillJob, now time.Time) *int64 {
	if !job.TotalRoutes.Valid {
		return nil
	}
	processed := int64(job.ProcessedRoutes)
	total := int64(job.TotalRoutes.Int32)
	if processed <= 0 {
		return nil
	}
	if processed >= total {
		zero := int64(0)
		return &zero
	}
	elapsed := now.Sub(job.StartedAt.Time)
	if elapsed <= 0 {
		return nil
	}
	elapsedMinutes := elapsed.Minutes()
	if elapsedMinutes <= 0 {
		return nil
	}
	throughputRPM := float64(processed) / elapsedMinutes
	if throughputRPM <= 0 {
		return nil
	}
	remaining := total - processed
	etaSeconds := int64(float64(remaining) / throughputRPM * 60.0)
	if etaSeconds < 0 || etaSeconds > alprBackfillETACap {
		etaSeconds = alprBackfillETACap
	}
	return &etaSeconds
}
