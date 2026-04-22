package api

import (
	"bufio"
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

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
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

// ExportHandler serves MP4 downloads built on the fly from a route's HLS
// segments. No re-encoding is performed -- FFmpeg's concat demuxer with
// "-c copy" simply remuxes the .ts segments into an MP4 container and the
// result is streamed straight to the HTTP response.
type ExportHandler struct {
	storage    *storage.Storage
	ffmpegPath string
}

// NewExportHandler creates an ExportHandler backed by the given storage.
func NewExportHandler(s *storage.Storage) *ExportHandler {
	return &ExportHandler{storage: s, ffmpegPath: "ffmpeg"}
}

// SetFFmpegPath overrides the FFmpeg binary used for export (test hook).
func (h *ExportHandler) SetFFmpegPath(path string) {
	h.ffmpegPath = path
}

// ExportMP4 handles GET /v1/routes/:dongle_id/:route_name/export.mp4. It
// collects every HLS .ts segment the transcoder produced for the requested
// camera, feeds them to ffmpeg through a concat-list on stdin, and streams
// the resulting MP4 directly to the client. Cancelling the HTTP request
// cancels the ffmpeg process through exec.CommandContext.
func (h *ExportHandler) ExportMP4(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	authDongleID, _ := c.Get(middleware.ContextKeyDongleID).(string)
	if authDongleID != dongleID {
		return c.JSON(http.StatusForbidden, errorResponse{
			Error: "dongle_id does not match authenticated device",
			Code:  http.StatusForbidden,
		})
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
	// Run ffmpeg in its own process group so the context cancel reliably
	// kills any children (same pattern the transcoder uses).
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

	// Feed the concat list to ffmpeg stdin in a goroutine so we can start
	// streaming stdout to the client right away.
	go func() {
		defer stdin.Close()
		_, _ = io.Copy(stdin, strings.NewReader(concatList))
	}()

	resp := c.Response()
	resp.Header().Set(echo.HeaderContentType, "video/mp4")
	resp.Header().Set(echo.HeaderContentDisposition,
		fmt.Sprintf("attachment; filename=%q", routeName+"-"+camera+".mp4"))
	resp.WriteHeader(http.StatusOK)

	// Stream ffmpeg stdout directly to the HTTP response. Using a buffered
	// reader lets us flush in reasonable chunks without holding the whole
	// output in memory.
	reader := bufio.NewReaderSize(stdout, 64*1024)
	if _, err := io.Copy(resp.Writer, reader); err != nil {
		// Kill ffmpeg if the copy fails (likely the client disconnected).
		_ = cmd.Cancel()
	}

	if err := cmd.Wait(); err != nil {
		// The request context being cancelled means the client went away;
		// that is not an error we need to surface. Any other wait error
		// arrived after headers, so we cannot alter the response status --
		// just capture the ffmpeg stderr for operator debugging.
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
		// The storage layer wraps os errors; fall back to a direct check.
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
// Falls back to the raw filename when no digits are present.
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
	// Left-pad so lexicographic order matches numeric order.
	pad := 12 - len(digits)
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat("0", pad) + string(digits)
}

// buildConcatList renders the concat-demuxer input format that ffmpeg
// reads from stdin. Paths are single-quoted and any embedded single
// quotes are escaped per ffmpeg's documented rules. Each path is
// prefixed with the "file:" URL scheme so ffmpeg uses the file protocol
// rather than trying to resolve the entry relative to the input URL
// (which would prepend "pipe:" when the concat list itself is read
// from stdin).
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

// isSignalKilled reports whether an ExitError was caused by a signal
// (which is how we stop ffmpeg when the client disconnects).
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

// RegisterRoutes wires the export endpoint onto the given Echo group.
// The caller is expected to have applied session-or-JWT auth middleware
// to the group.
func (h *ExportHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/:dongle_id/:route_name/export.mp4", h.ExportMP4)
}
