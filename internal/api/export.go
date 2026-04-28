package api

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/redaction"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/storage"
)

// cameraToHLSDir maps the public camera codes used on the export endpoint
// (f=front, e=wide/road, d=driver) to the HLS output directory names that
// the transcoder writes under each segment.
var cameraToHLSDir = map[string]string{
	"f": "fcamera",
	"e": "ecamera",
	"d": "dcamera",
}

// ExportHandler serves downloadable representations of a route: the GPS
// track as GPX (pulled from PostGIS) and a streamed MP4 built on the fly
// from the HLS segments on disk. No re-encoding is performed for the
// unredacted MP4 path -- FFmpeg's concat demuxer with "-c copy" remuxes
// .ts into MP4. The plate-redaction path (?redact_plates=true) requires
// re-encoding because the boxblur+overlay filter touches video pixels.
type ExportHandler struct {
	queries    *db.Queries
	storage    *storage.Storage
	settings   *settings.Store
	ffmpegPath string
}

// NewExportHandler creates an ExportHandler wired to both the database
// queries (for the GPX geometry lookup) and the filesystem storage (for
// the MP4 HLS segment walk). Either may be nil if the corresponding
// endpoint is not expected to be exercised. settings may be nil; in
// that case the redact_plates query parameter is treated as a no-op
// (alpr_enabled is read as false).
func NewExportHandler(queries *db.Queries, s *storage.Storage) *ExportHandler {
	return &ExportHandler{queries: queries, storage: s, ffmpegPath: "ffmpeg"}
}

// WithSettings wires a settings.Store into the export handler so it can
// read the alpr_enabled flag. Returns the handler for fluent chaining.
// Kept as a separate setter (rather than a constructor parameter) to
// avoid an API break for existing callers; the legacy NewExportHandler
// signature still works on existing tests.
func (h *ExportHandler) WithSettings(s *settings.Store) *ExportHandler {
	h.settings = s
	return h
}

// SetFFmpegPath overrides the FFmpeg binary used for MP4 export (test hook).
func (h *ExportHandler) SetFFmpegPath(path string) {
	h.ffmpegPath = path
}

// gpxFile is the top-level <gpx> element of a GPX 1.1 document.
type gpxFile struct {
	XMLName xml.Name `xml:"gpx"`
	Version string   `xml:"version,attr"`
	Creator string   `xml:"creator,attr"`
	Xmlns   string   `xml:"xmlns,attr"`
	Tracks  []gpxTrk `xml:"trk"`
}

type gpxTrk struct {
	Name     string      `xml:"name,omitempty"`
	Segments []gpxTrkseg `xml:"trkseg"`
}

type gpxTrkseg struct {
	Points []gpxTrkpt `xml:"trkpt"`
}

type gpxTrkpt struct {
	Lat float64 `xml:"lat,attr"`
	Lon float64 `xml:"lon,attr"`
}

// ExportRouteGPX handles GET /v1/routes/:dongle_id/:route_name/export.gpx.
func (h *ExportHandler) ExportRouteGPX(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	ctx := c.Request().Context()

	wkt, err := h.queries.GetRouteGeometryWKT(ctx, db.GetRouteGeometryWKTParams{
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
			Error: "failed to retrieve route geometry",
			Code:  http.StatusInternalServerError,
		})
	}

	if !wkt.Valid || strings.TrimSpace(wkt.String) == "" {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("route %s has no geometry", routeName),
			Code:  http.StatusNotFound,
		})
	}

	points, err := parseLineStringWKT(wkt.String)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: fmt.Sprintf("failed to parse route geometry: %s", err.Error()),
			Code:  http.StatusInternalServerError,
		})
	}
	if len(points) == 0 {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("route %s has no geometry", routeName),
			Code:  http.StatusNotFound,
		})
	}

	doc := gpxFile{
		Version: "1.1",
		Creator: "comma-personal-backend",
		Xmlns:   "http://www.topografix.com/GPX/1/1",
		Tracks: []gpxTrk{{
			Name:     routeName,
			Segments: []gpxTrkseg{{Points: points}},
		}},
	}

	body, err := xml.Marshal(doc)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to encode GPX document",
			Code:  http.StatusInternalServerError,
		})
	}

	payload := []byte(xml.Header)
	payload = append(payload, body...)

	c.Response().Header().Set(echo.HeaderContentType, "application/gpx+xml")
	c.Response().Header().Set(echo.HeaderContentDisposition,
		fmt.Sprintf(`attachment; filename="%s.gpx"`, routeName))
	return c.Blob(http.StatusOK, "application/gpx+xml", payload)
}

// ExportMP4 handles GET /v1/routes/:dongle_id/:route_name/export.mp4.
//
// Supported query parameters:
//   - camera: "f" (default), "e", or "d" -- selects the per-camera HLS
//     directory whose .ts segments will be concatenated.
//   - redact_plates: "true" / "false" (default false). When true, every
//     detected license-plate bbox (from plate_detections) is blurred in
//     the output via an ffmpeg crop+blur+overlay filter chain, gated
//     by per-detection enable windows of HoldWindowMs (default 200ms).
//     This requires re-encoding because the filter writes pixels; the
//     unredacted path stays on `-c copy` for speed. When alpr_enabled
//     is false at the runtime settings layer the redaction request is
//     treated as a no-op (no detections to apply, fall back to the
//     copy path) and a debug log is emitted.
func (h *ExportHandler) ExportMP4(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	camera := c.QueryParam("camera")
	if camera == "" {
		camera = "f"
	}
	hlsDir, ok := cameraToHLSDir[camera]
	if !ok {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("invalid camera %q: must be one of f, e, d", camera),
			Code:  http.StatusBadRequest,
		})
	}

	redactRequested := parseBoolQuery(c.QueryParam("redact_plates"))

	tsFiles, err := h.collectTSFiles(dongleID, routeName, hlsDir)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to enumerate HLS segments",
			Code:  http.StatusInternalServerError,
		})
	}
	if len(tsFiles) == 0 {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("no HLS segments found for route %s camera %s", routeName, camera),
			Code:  http.StatusNotFound,
		})
	}

	concatList := buildConcatList(tsFiles)
	ctx := c.Request().Context()

	// Decide whether redaction will actually run: only when the caller
	// asked for it, alpr_enabled is true at the settings layer, and
	// the camera supports it (the detector only runs against fcamera,
	// so a redact_plates=true request against ecamera/dcamera has no
	// detections to apply -- treat as a no-op rather than an error).
	doRedact := redactRequested && camera == "f" && h.alprEnabled(ctx)
	if redactRequested && !doRedact {
		log.Printf("export.mp4: redact_plates=true but no-op (alpr disabled or non-fcamera camera=%s)", camera)
	}

	var filter string
	if doRedact {
		dets, err := h.loadRouteDetections(ctx, dongleID, routeName, tsFiles)
		if err != nil {
			log.Printf("export.mp4: failed to load detections for redaction: %v", err)
			// Fall back to unredacted rather than failing the export.
			doRedact = false
		} else if len(dets) > 0 {
			filter = redaction.BuildBoxblurFilter(dets, redaction.FilterOptions{
				InputLabel:  "0:v",
				OutputLabel: "vout",
			})
		} else {
			// No detections for this route -- redact_plates=true is a
			// no-op; stay on the copy path.
			doRedact = false
		}
	}

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-f", "concat",
		"-safe", "0",
		"-protocol_whitelist", "file,pipe",
		"-i", "pipe:0",
	}
	if doRedact && filter != "" {
		// Re-encode with the boxblur+overlay chain. We use libx264 with
		// ultrafast preset because the redaction path has a per-request
		// budget (the user is waiting for the download); the unredacted
		// path stays on -c copy for the same reason.
		args = append(args,
			"-filter_complex", filter,
			"-map", "[vout]",
			"-map", "0:a?",
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-crf", "23",
			"-c:a", "copy",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-f", "mp4",
			"pipe:1",
		)
	} else {
		args = append(args,
			"-c", "copy",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-f", "mp4",
			"pipe:1",
		)
	}

	cmd := exec.CommandContext(ctx, h.ffmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to open ffmpeg stdin",
			Code:  http.StatusInternalServerError,
		})
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to open ffmpeg stdout",
			Code:  http.StatusInternalServerError,
		})
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to start ffmpeg",
			Code:  http.StatusInternalServerError,
		})
	}

	go func() {
		defer stdin.Close()
		_, _ = io.Copy(stdin, strings.NewReader(concatList))
	}()

	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, "video/mp4")
	filename := routeName + "-" + camera + ".mp4"
	if doRedact && filter != "" {
		filename = routeName + "-" + camera + "-redacted.mp4"
	}
	resp.Header().Set(echo.HeaderContentDisposition,
		fmt.Sprintf("attachment; filename=%q", filename))
	resp.WriteHeader(http.StatusOK)

	reader := bufio.NewReaderSize(stdout, 64*1024)
	if _, err := io.Copy(resp.Writer, reader); err != nil {
		_ = cmd.Cancel()
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() == nil && !isSignalKilled(err) {
			log.Printf("ffmpeg export failed: %v: %s", err, stderrBuf.String())
			return nil
		}
	}
	return nil
}

// collectTSFiles walks each segment of a route and returns every
// HLS .ts file under the requested camera's directory, ordered first
// by segment number and then by the numeric suffix of the .ts filename.
func (h *ExportHandler) collectTSFiles(dongleID, routeName, hlsDir string) ([]string, error) {
	segments, err := h.storage.ListSegments(dongleID, routeName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if _, statErr := os.Stat(h.storage.Path(dongleID, routeName, "", "")); os.IsNotExist(statErr) {
			return nil, nil
		}
		return nil, err
	}

	var tsFiles []string
	for _, segNum := range segments {
		segStr := strconv.Itoa(segNum)
		dir := filepath.Join(h.storage.Path(dongleID, routeName, segStr, ""), hlsDir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("failed to read hls dir %s: %w", dir, err)
		}

		var segTS []string
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".ts") {
				continue
			}
			segTS = append(segTS, filepath.Join(dir, name))
		}

		sort.Slice(segTS, func(i, j int) bool {
			return tsOrderKey(segTS[i]) < tsOrderKey(segTS[j])
		})
		tsFiles = append(tsFiles, segTS...)
	}

	return tsFiles, nil
}

// tsOrderKey extracts the numeric sequence from a filename like
// "seg_012.ts" so segments sort numerically rather than lexicographically.
func tsOrderKey(path string) string {
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, ".ts")
	digits := make([]byte, 0, len(name))
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] < '0' || name[i] > '9' {
			break
		}
		digits = append([]byte{name[i]}, digits...)
	}
	if len(digits) == 0 {
		return name
	}
	pad := 12 - len(digits)
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat("0", pad) + string(digits)
}

// buildConcatList renders the concat-demuxer input format that ffmpeg reads
// from stdin.
func buildConcatList(paths []string) string {
	var b strings.Builder
	b.WriteString("ffconcat version 1.0\n")
	for _, p := range paths {
		escaped := strings.ReplaceAll(p, `'`, `'\''`)
		b.WriteString("file 'file:")
		b.WriteString(escaped)
		b.WriteString("'\n")
	}
	return b.String()
}

// isSignalKilled reports whether an ExitError was caused by a signal.
func isSignalKilled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return status.Signaled()
}

// RegisterRoutes wires up the export endpoints on the given Echo group.
func (h *ExportHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/:dongle_id/:route_name/export.gpx", h.ExportRouteGPX)
	g.GET("/:dongle_id/:route_name/export.mp4", h.ExportMP4)
}

// parseLineStringWKT extracts an ordered list of (lat, lon) points from a
// PostGIS-rendered WKT string such as "LINESTRING(-122.4 37.7, -122.41 37.71)".
func parseLineStringWKT(wkt string) ([]gpxTrkpt, error) {
	s := strings.TrimSpace(wkt)
	upper := strings.ToUpper(s)
	if !strings.HasPrefix(upper, "LINESTRING") {
		return nil, fmt.Errorf("expected LINESTRING geometry, got %q", truncate(s, 32))
	}
	rest := strings.TrimSpace(s[len("LINESTRING"):])

	if strings.EqualFold(rest, "EMPTY") {
		return []gpxTrkpt{}, nil
	}

	if !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
		return nil, fmt.Errorf("malformed LINESTRING body: %q", truncate(rest, 32))
	}
	body := strings.TrimSpace(rest[1 : len(rest)-1])
	if body == "" {
		return []gpxTrkpt{}, nil
	}

	parts := strings.Split(body, ",")
	points := make([]gpxTrkpt, 0, len(parts))
	for _, raw := range parts {
		coord := strings.Fields(strings.TrimSpace(raw))
		if len(coord) < 2 {
			return nil, fmt.Errorf("malformed coordinate pair: %q", raw)
		}
		lon, err := strconv.ParseFloat(coord[0], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid longitude %q: %w", coord[0], err)
		}
		lat, err := strconv.ParseFloat(coord[1], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid latitude %q: %w", coord[1], err)
		}
		points = append(points, gpxTrkpt{Lat: lat, Lon: lon})
	}
	return points, nil
}

// truncate trims s to at most n runes, appending an ellipsis when truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// parseBoolQuery is a forgiving parser for "?redact_plates=..." style
// query parameters. Only literal "true" / "1" are treated as true; an
// empty string and any other value defaults to false. Case-insensitive
// so the frontend can send "True" without surprises.
func parseBoolQuery(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// alprEnabled reports whether the runtime alpr_enabled flag is true.
// A nil settings store, a missing row, or an unparseable value all
// return false so the redaction path stays a strict opt-in.
func (h *ExportHandler) alprEnabled(ctx context.Context) bool {
	if h.settings == nil {
		return false
	}
	v, err := h.settings.BoolOr(ctx, settings.KeyALPREnabled, false)
	if err != nil {
		return false
	}
	return v
}

// loadRouteDetections fetches every plate detection for the route and
// converts them to redaction.Detection by mapping each detection's
// (segment, frame_offset_ms) to a cumulative time on the concatenated
// MP4 timeline. Detections whose bbox blob is malformed are dropped
// silently; a single bad row should not poison the whole export.
//
// The mapping assumes each segment's HLS files concatenate end-to-end
// (the standard openpilot 60s-per-segment layout). Per-segment .ts
// chunk durations are derived by ffprobing each segment's first file
// is overkill; we use the canonical 60s per segment and let the
// per-detection enable-window margin (HoldWindowMs) absorb any minor
// drift. tsFiles is unused here but kept as a parameter so a future
// improvement can switch to a true ffprobe-based mapping without an
// API churn.
func (h *ExportHandler) loadRouteDetections(ctx context.Context, dongleID, routeName string, _ []string) ([]redaction.Detection, error) {
	if h.queries == nil {
		return nil, nil
	}
	rows, err := h.queries.ListDetectionsForRoute(ctx, db.ListDetectionsForRouteParams{
		DongleID: dongleID,
		Route:    routeName,
	})
	if err != nil {
		return nil, fmt.Errorf("list detections: %w", err)
	}
	out := make([]redaction.Detection, 0, len(rows))
	for _, r := range rows {
		bbox, derr := redaction.DecodeBbox(r.Bbox)
		if derr != nil {
			continue
		}
		// Cumulative time on the concatenated MP4 = segment*60s +
		// frame_offset_ms/1000. The route's segment 0 starts at t=0.
		t := float64(r.Segment)*segmentDurationSec + float64(r.FrameOffsetMs)/1000.0
		out = append(out, redaction.Detection{
			TimeSec: t,
			Bbox:    bbox,
		})
	}
	return out, nil
}

// segmentDurationSec is the canonical openpilot/sunnypilot segment
// duration. Used by the export pipeline to map per-segment detection
// times into a route-cumulative timestamp without per-segment ffprobe.
const segmentDurationSec = 60.0
