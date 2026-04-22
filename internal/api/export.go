package api

import (
	"bufio"
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
// from the HLS segments on disk. No re-encoding is performed for the MP4
// path -- FFmpeg's concat demuxer with "-c copy" remuxes .ts into MP4.
type ExportHandler struct {
	queries    *db.Queries
	storage    *storage.Storage
	ffmpegPath string
}

// NewExportHandler creates an ExportHandler wired to both the database
// queries (for the GPX geometry lookup) and the filesystem storage (for
// the MP4 HLS segment walk). Either may be nil if the corresponding
// endpoint is not expected to be exercised.
func NewExportHandler(queries *db.Queries, s *storage.Storage) *ExportHandler {
	return &ExportHandler{queries: queries, storage: s, ffmpegPath: "ffmpeg"}
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
	cmd := exec.CommandContext(ctx, h.ffmpegPath,
		"-hide_banner",
		"-loglevel", "error",
		"-f", "concat",
		"-safe", "0",
		"-protocol_whitelist", "file,pipe",
		"-i", "pipe:0",
		"-c", "copy",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4",
		"pipe:1",
	)
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
	resp.Header().Set(echo.HeaderContentDisposition,
		fmt.Sprintf("attachment; filename=%q", routeName+"-"+camera+".mp4"))
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
