package api

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/storage"
)

// UsageReporter is the slice of *storage.Storage that StorageHandler depends
// on. Kept as an interface so tests can stub the walk without touching the
// filesystem.
type UsageReporter interface {
	Usage(ctx context.Context, forceRefresh bool) (*storage.UsageReport, error)
}

// StorageHandler serves disk usage endpoints backed by the local storage
// tree. The heavy Usage() call is memoized so this endpoint is safe to poll.
type StorageHandler struct {
	store UsageReporter
}

// NewStorageHandler returns a StorageHandler wired to the given UsageReporter.
func NewStorageHandler(store UsageReporter) *StorageHandler {
	return &StorageHandler{store: store}
}

// GetUsage handles GET /v1/storage/usage. It returns the cached usage report
// unless the caller passes ?refresh=1, in which case the underlying walk is
// re-run and the cache is updated.
func (h *StorageHandler) GetUsage(c echo.Context) error {
	forceRefresh := isTruthyQuery(c.QueryParam("refresh"))

	report, err := h.store.Usage(c.Request().Context(), forceRefresh)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to compute storage usage",
			Code:  http.StatusInternalServerError,
		})
	}
	return c.JSON(http.StatusOK, report)
}

// RegisterRoutes wires the storage endpoints onto the given Echo group. The
// group is expected to already have JWT auth middleware applied.
func (h *StorageHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/storage/usage", h.GetUsage)
	g.GET("/storage/usage/", h.GetUsage)
}

// isTruthyQuery reports whether a query param value should be treated as
// "true". Accepts "1", "true", "yes" (case-insensitive) which matches how
// operators typically hit the endpoint from a browser.
func isTruthyQuery(v string) bool {
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES", "Yes":
		return true
	}
	return false
}
