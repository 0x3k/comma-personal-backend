package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// CrashesReader is the subset of *db.Queries the crashes dashboard
// endpoints depend on. Kept narrow so tests can stub the database.
type CrashesReader interface {
	ListCrashes(ctx context.Context, arg db.ListCrashesParams) ([]db.ListCrashesRow, error)
	CountCrashes(ctx context.Context, dongleIDFilter string) (int64, error)
	GetCrash(ctx context.Context, id int32) (db.Crash, error)
}

// CrashesHandler exposes the crash table to the operator dashboard. The
// Sentry envelope endpoint at /api/:project_id/envelope/ is the writer;
// these handlers are read-only views over what landed there.
type CrashesHandler struct {
	queries CrashesReader
}

// NewCrashesHandler creates a handler for the crashes dashboard endpoints.
func NewCrashesHandler(queries CrashesReader) *CrashesHandler {
	return &CrashesHandler{queries: queries}
}

// crashListItem is the row shape rendered in the dashboard list. We keep
// the JSONB columns as raw strings so the frontend can decode them on
// demand without burdening the list response with deeply-nested fields.
type crashListItem struct {
	ID         int32      `json:"id"`
	EventID    string     `json:"event_id"`
	DongleID   string     `json:"dongle_id,omitempty"`
	Level      string     `json:"level"`
	Message    string     `json:"message"`
	Tags       any        `json:"tags"`
	Exception  any        `json:"exception"`
	OccurredAt *time.Time `json:"occurred_at"`
	ReceivedAt *time.Time `json:"received_at"`
}

// crashListResponse mirrors momentsListResponse: we paginate with limit +
// offset and surface the unfiltered total so the dashboard can render
// "showing 1-50 of N".
type crashListResponse struct {
	Crashes []crashListItem `json:"crashes"`
	Total   int64           `json:"total"`
}

const (
	defaultCrashesLimit = 50
	maxCrashesLimit     = 500
)

// ListCrashes handles GET /v1/crashes with optional query params:
//   - device: filter by dongle_id
//   - limit, offset: pagination (defaults: 50/0, max: 500)
func (h *CrashesHandler) ListCrashes(c echo.Context) error {
	dongleFilter := c.QueryParam("device")

	limit := defaultCrashesLimit
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c.JSON(http.StatusBadRequest, errorResponse{
				Error: "invalid limit",
				Code:  http.StatusBadRequest,
			})
		}
		if n > maxCrashesLimit {
			n = maxCrashesLimit
		}
		limit = n
	}
	offset := 0
	if v := c.QueryParam("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c.JSON(http.StatusBadRequest, errorResponse{
				Error: "invalid offset",
				Code:  http.StatusBadRequest,
			})
		}
		offset = n
	}

	rows, err := h.queries.ListCrashes(c.Request().Context(), db.ListCrashesParams{
		Limit:          int32(limit),
		Offset:         int32(offset),
		DongleIDFilter: dongleFilter,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list crashes",
			Code:  http.StatusInternalServerError,
		})
	}

	total, err := h.queries.CountCrashes(c.Request().Context(), dongleFilter)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to count crashes",
			Code:  http.StatusInternalServerError,
		})
	}

	out := make([]crashListItem, 0, len(rows))
	for _, r := range rows {
		item := crashListItem{
			ID:      r.ID,
			EventID: r.EventID,
			Level:   r.Level,
			Message: r.Message,
		}
		if r.DongleID.Valid {
			item.DongleID = r.DongleID.String
		}
		if r.OccurredAt.Valid {
			t := r.OccurredAt.Time
			item.OccurredAt = &t
		}
		if r.ReceivedAt.Valid {
			t := r.ReceivedAt.Time
			item.ReceivedAt = &t
		}
		item.Tags = decodeJSONBOrEmpty(r.Tags, map[string]interface{}{})
		item.Exception = decodeJSONBOrEmpty(r.Exception, map[string]interface{}{})
		out = append(out, item)
	}

	return c.JSON(http.StatusOK, crashListResponse{
		Crashes: out,
		Total:   total,
	})
}

// crashDetail is the full crash response shape, including the raw event
// blob so operators can drill in.
type crashDetail struct {
	ID          int32      `json:"id"`
	EventID     string     `json:"event_id"`
	DongleID    string     `json:"dongle_id,omitempty"`
	Level       string     `json:"level"`
	Message     string     `json:"message"`
	Fingerprint any        `json:"fingerprint"`
	Tags        any        `json:"tags"`
	Exception   any        `json:"exception"`
	Breadcrumbs any        `json:"breadcrumbs"`
	RawEvent    any        `json:"raw_event"`
	OccurredAt  *time.Time `json:"occurred_at"`
	ReceivedAt  *time.Time `json:"received_at"`
}

// GetCrash handles GET /v1/crashes/:id and returns the full crash record.
func (h *CrashesHandler) GetCrash(c echo.Context) error {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid crash id",
			Code:  http.StatusBadRequest,
		})
	}

	row, err := h.queries.GetCrash(c.Request().Context(), int32(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: "crash not found",
				Code:  http.StatusNotFound,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to retrieve crash",
			Code:  http.StatusInternalServerError,
		})
	}

	out := crashDetail{
		ID:      row.ID,
		EventID: row.EventID,
		Level:   row.Level,
		Message: row.Message,
	}
	if row.DongleID.Valid {
		out.DongleID = row.DongleID.String
	}
	if row.OccurredAt.Valid {
		t := row.OccurredAt.Time
		out.OccurredAt = &t
	}
	if row.ReceivedAt.Valid {
		t := row.ReceivedAt.Time
		out.ReceivedAt = &t
	}
	out.Fingerprint = decodeJSONBOrEmpty(row.Fingerprint, []interface{}{})
	out.Tags = decodeJSONBOrEmpty(row.Tags, map[string]interface{}{})
	out.Exception = decodeJSONBOrEmpty(row.Exception, map[string]interface{}{})
	out.Breadcrumbs = decodeJSONBOrEmpty(row.Breadcrumbs, []interface{}{})
	out.RawEvent = decodeJSONBOrEmpty(row.RawEvent, map[string]interface{}{})

	return c.JSON(http.StatusOK, out)
}

// decodeJSONBOrEmpty parses raw JSONB bytes into a Go value. If the input
// is empty or malformed (which should never happen for a well-behaved
// envelope) the supplied empty value is used so the response shape stays
// stable for the frontend.
func decodeJSONBOrEmpty(raw []byte, empty interface{}) interface{} {
	if len(raw) == 0 {
		return empty
	}
	var out interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return empty
	}
	return out
}

// RegisterRoutes wires up the dashboard read endpoints. The group must
// accept either a session cookie or a device JWT (SessionOrJWT) -- the
// dashboard reads from the operator browser, and ad-hoc CLI lookups can
// hit the same endpoint with a device JWT.
func (h *CrashesHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/crashes", h.ListCrashes)
	g.GET("/crashes/:id", h.GetCrash)
}
