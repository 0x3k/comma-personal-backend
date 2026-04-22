package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/cereal"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/storage"
)

// SignalsHandler serves the per-route driving-signals timeline used by the
// frontend video player overlay. It parses each segment's qlog through the
// cereal signal extractor the first time it's requested and caches the
// column-oriented JSON next to the qlog so repeat calls are cheap.
type SignalsHandler struct {
	queries *db.Queries
	storage *storage.Storage
}

// NewSignalsHandler constructs a SignalsHandler wired to both the database
// (for the route existence check) and filesystem storage (for qlog reads and
// signals.json cache writes).
func NewSignalsHandler(queries *db.Queries, s *storage.Storage) *SignalsHandler {
	return &SignalsHandler{queries: queries, storage: s}
}

// signalsResponse is the column-oriented JSON payload the frontend consumes.
// Every slice is the same length as Times; entry i across all slices describes
// the same moment. Times are unix milliseconds (derived from the event's
// logMonoTime, which is a monotonic clock origin, not wall-clock -- the
// frontend treats them as a per-route relative timeline).
type signalsResponse struct {
	Times       []int64   `json:"times"`
	SpeedMPS    []float64 `json:"speed_mps"`
	SteeringDeg []float64 `json:"steering_deg"`
	Engaged     []bool    `json:"engaged"`
	Alerts      []string  `json:"alerts"`
}

// segmentSignals is the on-disk cache format. It mirrors signalsResponse but
// carries one segment's worth of data so each segment can be cached
// independently of the rest of the route.
type segmentSignals struct {
	Times       []int64   `json:"times"`
	SpeedMPS    []float64 `json:"speed_mps"`
	SteeringDeg []float64 `json:"steering_deg"`
	Engaged     []bool    `json:"engaged"`
	Alerts      []string  `json:"alerts"`
}

// qlogFilenames lists the possible on-disk names for a segment's qlog, in the
// preferred-first order the resolver walks them. Devices may upload either
// the raw capnp stream or the bz2-wrapped form; the cereal parser handles
// both, but we need to know which filename to open.
var qlogFilenames = []string{"qlog.bz2", "qlog"}

// signalsCacheFilename is the on-disk cache written next to the qlog after
// the first parse. It holds one segment's signalsResponse columns as JSON.
const signalsCacheFilename = "signals.json"

// GetRouteSignals handles GET /v1/routes/:dongle_id/:route_name/signals.
// It concatenates per-segment signals (in segment-number order) into a single
// time-aligned response. Segments with no qlog yet are skipped so partial
// routes still return a useful (possibly shorter) timeline.
func (h *SignalsHandler) GetRouteSignals(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	authDongleID, _ := c.Get(middleware.ContextKeyDongleID).(string)
	if authDongleID != dongleID {
		return c.JSON(http.StatusForbidden, errorResponse{
			Error: "dongle_id does not match authenticated device",
			Code:  http.StatusForbidden,
		})
	}

	ctx := c.Request().Context()

	// Route existence check -- the endpoint returns 404 for unknown routes
	// rather than an empty payload so clients can distinguish "route does
	// not exist" from "route exists but no segments are parsed yet".
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

	segments, err := h.queries.ListSegmentsByRoute(ctx, route.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to retrieve segments",
			Code:  http.StatusInternalServerError,
		})
	}

	resp := signalsResponse{
		Times:       []int64{},
		SpeedMPS:    []float64{},
		SteeringDeg: []float64{},
		Engaged:     []bool{},
		Alerts:      []string{},
	}

	for _, seg := range segments {
		segStr := strconv.Itoa(int(seg.SegmentNumber))
		segSig, err := h.loadSegmentSignals(dongleID, routeName, segStr)
		if err != nil {
			// Surface a 500 only for unexpected I/O / parse errors. A
			// missing qlog is not an error -- loadSegmentSignals returns
			// (nil, nil) for that case.
			log.Printf("signals: segment %s/%s/%s: %v", dongleID, routeName, segStr, err)
			return c.JSON(http.StatusInternalServerError, errorResponse{
				Error: "failed to compute segment signals",
				Code:  http.StatusInternalServerError,
			})
		}
		if segSig == nil {
			continue
		}
		resp.Times = append(resp.Times, segSig.Times...)
		resp.SpeedMPS = append(resp.SpeedMPS, segSig.SpeedMPS...)
		resp.SteeringDeg = append(resp.SteeringDeg, segSig.SteeringDeg...)
		resp.Engaged = append(resp.Engaged, segSig.Engaged...)
		resp.Alerts = append(resp.Alerts, segSig.Alerts...)
	}

	return c.JSON(http.StatusOK, resp)
}

// loadSegmentSignals returns the time-aligned signals for a single segment.
// It first consults the on-disk cache (signals.json); if present and fresher
// than the qlog it returns that directly. Otherwise it parses the qlog,
// writes the cache, and returns the freshly computed result.
//
// A return value of (nil, nil) means the segment has no qlog yet and should
// be skipped. Non-nil errors are reserved for unexpected I/O / parse
// failures.
func (h *SignalsHandler) loadSegmentSignals(dongleID, routeName, segment string) (*segmentSignals, error) {
	qlogPath, qlogInfo, err := h.findQlog(dongleID, routeName, segment)
	if err != nil {
		return nil, err
	}

	cachePath := h.storage.Path(dongleID, routeName, segment, signalsCacheFilename)
	cacheInfo, cacheErr := os.Stat(cachePath)

	// Cache is usable when either:
	//   - the qlog is missing but a previous run already cached the result
	//     (lets us serve consistent output even if the qlog was pruned), or
	//   - the cache exists and is at least as fresh as the qlog.
	switch {
	case cacheErr == nil && qlogInfo == nil:
		return readSegmentSignalsCache(cachePath)
	case cacheErr == nil && qlogInfo != nil && !cacheInfo.ModTime().Before(qlogInfo.ModTime()):
		return readSegmentSignalsCache(cachePath)
	case cacheErr != nil && !errors.Is(cacheErr, os.ErrNotExist):
		return nil, fmt.Errorf("failed to stat signals cache: %w", cacheErr)
	}

	// No usable cache and no qlog to parse -- skip this segment.
	if qlogInfo == nil {
		return nil, nil
	}

	segSig, err := computeSegmentSignals(qlogPath)
	if err != nil {
		return nil, err
	}

	if err := writeSegmentSignalsCache(cachePath, segSig); err != nil {
		// Cache write failure is non-fatal: the response is still correct,
		// the next request will just recompute. Log and continue.
		log.Printf("signals: failed to write cache %s: %v", cachePath, err)
	}

	return segSig, nil
}

// findQlog locates the qlog file on disk for a segment, preferring the
// compressed form. It returns the path alongside its FileInfo, or
// ("", nil, nil) when no qlog exists for this segment.
func (h *SignalsHandler) findQlog(dongleID, routeName, segment string) (string, os.FileInfo, error) {
	for _, name := range qlogFilenames {
		path := h.storage.Path(dongleID, routeName, segment, name)
		info, err := os.Stat(path)
		if err == nil {
			if info.IsDir() {
				continue
			}
			return path, info, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("failed to stat qlog %s: %w", path, err)
		}
	}
	return "", nil, nil
}

// computeSegmentSignals runs the cereal signal extractor over a single qlog
// and returns its column-oriented result in the on-disk cache shape.
func computeSegmentSignals(qlogPath string) (*segmentSignals, error) {
	f, err := os.Open(qlogPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open qlog %s: %w", qlogPath, err)
	}
	defer f.Close()

	ext := &cereal.SignalExtractor{}
	driving, err := ext.ExtractDriving(f)
	if err != nil {
		return nil, fmt.Errorf("failed to parse qlog %s: %w", qlogPath, err)
	}

	n := len(driving.Times)
	out := &segmentSignals{
		Times:       make([]int64, n),
		SpeedMPS:    make([]float64, n),
		SteeringDeg: make([]float64, n),
		Engaged:     make([]bool, n),
		Alerts:      make([]string, n),
	}
	for i := 0; i < n; i++ {
		out.Times[i] = driving.Times[i].UnixMilli()
		out.SpeedMPS[i] = driving.VEgo[i]
		out.SteeringDeg[i] = driving.SteeringAngleDeg[i]
		out.Engaged[i] = driving.Engaged[i]
		out.Alerts[i] = driving.AlertText[i]
	}
	return out, nil
}

// readSegmentSignalsCache loads a segment's signals.json off disk.
func readSegmentSignalsCache(path string) (*segmentSignals, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read signals cache %s: %w", path, err)
	}
	var out segmentSignals
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("failed to decode signals cache %s: %w", path, err)
	}
	return &out, nil
}

// writeSegmentSignalsCache writes a segment's signals.json next to its qlog.
// It creates the parent directory if needed (for the corner case where the
// segment dir was pruned between the qlog-stat and the cache-write) and
// writes atomically via a temp file rename so a crashed write can't leave a
// truncated cache.
func writeSegmentSignalsCache(path string, segSig *segmentSignals) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create signals cache dir: %w", err)
	}

	data, err := json.Marshal(segSig)
	if err != nil {
		return fmt.Errorf("failed to encode signals cache: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), signalsCacheFilename+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to open temp cache file: %w", err)
	}
	tmpPath := tmp.Name()

	// Best-effort cleanup for the tmp file if anything below fails.
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write temp cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp cache file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to rename temp cache file: %w", err)
	}

	// Match the cache mtime to "now" -- the rename set a fresh mtime, so
	// cache-fresh-relative-to-qlog comparisons use a defensible ordering.
	_ = os.Chtimes(path, time.Now(), time.Now())
	success = true
	return nil
}

// RegisterRoutes wires up the signals endpoint on the given Echo group.
// The group should already have JWT auth middleware applied.
func (h *SignalsHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/:dongle_id/:route_name/signals", h.GetRouteSignals)
}
