package api

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/alpr/heuristic"
	"comma-personal-backend/internal/db"
)

// alprReevaluateMaxDaysBack caps the days_back parameter so a typo'd
// 100000 cannot enumerate the entire encounter history in one
// synchronous request. 365 matches the spec.
const alprReevaluateMaxDaysBack = 365

// alprReevaluateDefaultDaysBack is the default window when the body
// omits days_back. Matches the heuristic's own default lookback so a
// user who hits the button without tweaking gets the historically
// expected coverage.
const alprReevaluateDefaultDaysBack = 30

// alprReevaluateETASecondsPerJob is the rough per-plate budget the
// endpoint reports back to the dashboard so it can render an ETA on
// the loading spinner. The actual evaluation is much faster (a few
// ms per plate), but we report a conservative estimate so a slow
// signature-fusion read does not surprise the UI.
const alprReevaluateETASecondsPerJob = 0.05

// alprReevaluateQuerier is the slice of *db.Queries this handler
// needs. Carved out so tests can drop in an in-memory fake without
// standing up Postgres.
type alprReevaluateQuerier interface {
	heuristic.HeuristicQuerier
	ListDistinctPlatesEncounteredInWindow(
		ctx context.Context,
		arg db.ListDistinctPlatesEncounteredInWindowParams,
	) ([]db.ListDistinctPlatesEncounteredInWindowRow, error)
}

// ALPRReevaluateHandler exposes POST /v1/alpr/heuristic/reevaluate.
// The endpoint is session-only and synchronously rescores every
// distinct (dongle_id, plate_hash) pair in the requested window.
//
// The synchronous design intentionally trades latency on a large
// fleet for simplicity: each plate is a few milliseconds to score, a
// 30-day window with a single device commonly resolves in under a
// second, and the dashboard already shows a spinner. If the workload
// outgrows this endpoint, replace the inline loop with an enqueue
// onto the existing aggregator -> heuristic event channel.
type ALPRReevaluateHandler struct {
	queries alprReevaluateQuerier
	worker  *heuristic.Worker
}

// NewALPRReevaluateHandler wires the handler with the queries needed
// for the affected-plate enumeration and a constructed heuristic
// worker for the per-plate evaluation. The worker's Settings, Queries,
// Metrics fields are read by both the dry-run and live paths.
func NewALPRReevaluateHandler(q alprReevaluateQuerier, w *heuristic.Worker) *ALPRReevaluateHandler {
	return &ALPRReevaluateHandler{queries: q, worker: w}
}

// alprReevaluateRequest is the JSON body. Both fields are optional
// with the documented defaults.
type alprReevaluateRequest struct {
	DaysBack *int  `json:"days_back,omitempty"`
	DryRun   *bool `json:"dry_run,omitempty"`
}

// alprReevaluateResponse is the JSON body the endpoint returns. The
// dry-run path populates Summary; the live path leaves it nil.
type alprReevaluateResponse struct {
	Accepted     bool                   `json:"accepted"`
	ETASeconds   float64                `json:"eta_seconds"`
	JobsEnqueued int                    `json:"jobs_enqueued"`
	DryRun       bool                   `json:"dry_run"`
	DaysBack     int                    `json:"days_back"`
	Summary      *alprReevaluateSummary `json:"summary,omitempty"`
}

// alprReevaluateSummary is the dry-run histogram. Counts are keyed on
// integer severity (0..5); the dashboard renders a side-by-side
// before/after table.
type alprReevaluateSummary struct {
	BySeverityBefore map[string]int `json:"by_severity_before"`
	BySeverityAfter  map[string]int `json:"by_severity_after"`
	Whitelisted      int            `json:"whitelisted"`
}

// Reevaluate handles POST /v1/alpr/heuristic/reevaluate.
func (h *ALPRReevaluateHandler) Reevaluate(c echo.Context) error {
	var req alprReevaluateRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}
	daysBack := alprReevaluateDefaultDaysBack
	if req.DaysBack != nil {
		daysBack = *req.DaysBack
	}
	if daysBack < 1 {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "days_back must be >= 1",
			Code:  http.StatusBadRequest,
		})
	}
	if daysBack > alprReevaluateMaxDaysBack {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "days_back exceeds maximum allowed window",
			Code:  http.StatusBadRequest,
		})
	}
	dryRun := false
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}

	if h.queries == nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "alpr re-evaluation: queries not configured",
			Code:  http.StatusInternalServerError,
		})
	}
	if h.worker == nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "alpr re-evaluation: heuristic worker not configured",
			Code:  http.StatusInternalServerError,
		})
	}

	ctx := c.Request().Context()
	now := time.Now().UTC()
	windowStart := now.Add(-time.Duration(daysBack) * 24 * time.Hour)
	rows, err := h.queries.ListDistinctPlatesEncounteredInWindow(ctx, db.ListDistinctPlatesEncounteredInWindowParams{
		WindowStart: pgtype.Timestamptz{Time: windowStart, Valid: true},
		WindowEnd:   pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to enumerate plates for re-evaluation",
			Code:  http.StatusInternalServerError,
		})
	}

	resp := alprReevaluateResponse{
		Accepted:     true,
		ETASeconds:   float64(len(rows)) * alprReevaluateETASecondsPerJob,
		JobsEnqueued: len(rows),
		DryRun:       dryRun,
		DaysBack:     daysBack,
	}

	if dryRun {
		summary := alprReevaluateSummary{
			BySeverityBefore: map[string]int{},
			BySeverityAfter:  map[string]int{},
		}
		for _, r := range rows {
			res, err := h.worker.EvaluatePlateDryRun(ctx, r.PlateHash)
			if err != nil {
				// Per-plate errors are logged and skipped; the
				// histogram should still represent the plates that
				// did score so the operator can act on the
				// majority.
				c.Logger().Errorf("alpr reevaluate dry-run %x: %v", r.PlateHash, err)
				continue
			}
			if res.Whitelisted {
				summary.Whitelisted++
			}
			incSeverity(summary.BySeverityBefore, res.PriorSeverity)
			incSeverity(summary.BySeverityAfter, res.ProposedSeverity)
		}
		resp.Summary = &summary
		return c.JSON(http.StatusOK, resp)
	}

	// Live path: walk every affected plate and call the same
	// EvaluatePlate the worker uses. Errors are logged but do not
	// stop the loop -- a single bad plate must not poison the
	// re-run.
	for _, r := range rows {
		// The route argument is informational on the alert_events
		// row; for re-evaluation we tag it as empty so the audit
		// trail records "no causal route".
		if err := h.worker.EvaluatePlate(ctx, r.PlateHash, "", r.DongleID); err != nil {
			c.Logger().Errorf("alpr reevaluate live %x dongle=%s: %v", r.PlateHash, r.DongleID, err)
		}
	}
	return c.JSON(http.StatusOK, resp)
}

// incSeverity bumps the count under the integer-severity string key.
// Stringifying makes the JSON shape stable regardless of how the
// dashboard library serializes integer map keys.
func incSeverity(m map[string]int, severity int) {
	switch severity {
	case 0:
		m["0"]++
	case 1:
		m["1"]++
	case 2:
		m["2"]++
	case 3:
		m["3"]++
	case 4:
		m["4"]++
	case 5:
		m["5"]++
	default:
		m["other"]++
	}
}

// RegisterRoutes wires the re-evaluate endpoint onto the supplied
// session-only group.
func (h *ALPRReevaluateHandler) RegisterRoutes(g *echo.Group) {
	g.POST("/alpr/heuristic/reevaluate", h.Reevaluate)
}
