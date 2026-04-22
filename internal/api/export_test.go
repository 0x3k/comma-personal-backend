package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/storage"
)

const (
	testExportDongleID = "abc123"
	testExportRoute    = "2024-03-15--12-30-00"
)

// newExportRequest builds an Echo context targeting the export endpoint
// with the given camera, auth dongle, and handler storage layout.
func newExportRequest(t *testing.T, dongleID, routeName, camera, authDongle string) (*httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	e := echo.New()
	target := "/v1/routes/" + dongleID + "/" + routeName + "/export.mp4"
	if camera != "" {
		target += "?camera=" + camera
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues(dongleID, routeName)
	c.Set(middleware.ContextKeyDongleID, authDongle)
	return rec, c
}

// writeTinyTS writes a tiny but valid single-stream MPEG-TS file using
// ffmpeg. The resulting .ts files can be concatenated and remuxed to MP4
// by the export handler. Returns the ffmpeg path or skips the test if
// ffmpeg is not available in $PATH (CI environments without ffmpeg
// cannot exercise the happy path).
func writeTinyTS(t *testing.T, ffmpegPath, outPath string) {
	t.Helper()
	cmd := exec.Command(ffmpegPath,
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-f", "lavfi",
		"-i", "color=black:s=16x16:d=0.3:r=10",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-pix_fmt", "yuv420p",
		"-f", "mpegts",
		outPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to create test ts fixture: %v: %s", err, string(out))
	}
}

// setupHLSRoute lays out storage so it mirrors what the transcoder
// produces for a route: segment directories containing per-camera HLS
// folders with .ts files.
func setupHLSRoute(t *testing.T, store *storage.Storage, ffmpegPath, dongleID, routeName, camera string, segmentCount int) {
	t.Helper()
	hlsDir, ok := cameraToHLSDir[camera]
	if !ok {
		t.Fatalf("unknown camera %q", camera)
	}
	for seg := 0; seg < segmentCount; seg++ {
		segStr := strconv.Itoa(seg)
		dir := filepath.Join(store.Path(dongleID, routeName, segStr, ""), hlsDir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create hls dir: %v", err)
		}
		writeTinyTS(t, ffmpegPath, filepath.Join(dir, "seg_000.ts"))
	}
}

// newExportStorage builds a Storage rooted at a temporary directory.
func newExportStorage(t *testing.T) *storage.Storage {
	t.Helper()
	dir := t.TempDir()
	return storage.New(dir)
}

func TestExportMP4_HappyPath(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		// The happy path requires a real ffmpeg binary to both generate
		// fixture .ts files and remux them into MP4. Skip cleanly when
		// the binary is missing (e.g. minimal CI images).
		t.Skipf("ffmpeg not found in PATH: %v", err)
	}

	store := newExportStorage(t)
	setupHLSRoute(t, store, ffmpegPath, testExportDongleID, testExportRoute, "f", 2)

	handler := NewExportHandler(store)

	rec, c := newExportRequest(t, testExportDongleID, testExportRoute, "", testExportDongleID)
	if err := handler.ExportMP4(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	ct := rec.Header().Get(echo.HeaderContentType)
	if ct != "video/mp4" {
		t.Errorf("content-type = %q, want video/mp4", ct)
	}

	cd := rec.Header().Get(echo.HeaderContentDisposition)
	wantDisp := `attachment; filename="` + testExportRoute + `-f.mp4"`
	if cd != wantDisp {
		t.Errorf("content-disposition = %q, want %q", cd, wantDisp)
	}

	body := rec.Body.Bytes()
	if len(body) < 8 {
		t.Fatalf("response too short (%d bytes) to contain an ftyp box", len(body))
	}

	// The MP4 ISO base media format begins with a size (4 bytes) followed
	// by the four-byte 'ftyp' box type. Verify 'ftyp' appears in the
	// first 64 bytes of the streamed response.
	head := body
	if len(head) > 64 {
		head = head[:64]
	}
	if !bytes.Contains(head, []byte("ftyp")) {
		t.Errorf("expected 'ftyp' box in first 64 bytes of response; got hex %x", head)
	}
}

func TestExportMP4_MissingHLS(t *testing.T) {
	// Force the handler to use /bin/true as ffmpeg so the test never
	// depends on a real ffmpeg binary -- we expect a 404 before ffmpeg
	// is ever invoked.
	store := newExportStorage(t)
	handler := NewExportHandler(store)
	handler.SetFFmpegPath(stubSuccessPath(t))

	rec, c := newExportRequest(t, testExportDongleID, testExportRoute, "f", testExportDongleID)
	if err := handler.ExportMP4(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no HLS segments") {
		t.Errorf("error body = %q, want substring %q", rec.Body.String(), "no HLS segments")
	}
}

func TestExportMP4_MissingHLS_ExistingRouteDifferentCamera(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg not found in PATH: %v", err)
	}
	store := newExportStorage(t)
	// Populate the route with only the front camera; requesting driver
	// should return 404 even though the route directory exists.
	setupHLSRoute(t, store, ffmpegPath, testExportDongleID, testExportRoute, "f", 1)

	handler := NewExportHandler(store)
	handler.SetFFmpegPath(stubSuccessPath(t))

	rec, c := newExportRequest(t, testExportDongleID, testExportRoute, "d", testExportDongleID)
	if err := handler.ExportMP4(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestExportMP4_BadCamera(t *testing.T) {
	store := newExportStorage(t)
	handler := NewExportHandler(store)

	rec, c := newExportRequest(t, testExportDongleID, testExportRoute, "x", testExportDongleID)
	if err := handler.ExportMP4(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid camera") {
		t.Errorf("error body = %q, want 'invalid camera' substring", rec.Body.String())
	}
}

func TestExportMP4_DongleMismatch(t *testing.T) {
	store := newExportStorage(t)
	handler := NewExportHandler(store)

	rec, c := newExportRequest(t, testExportDongleID, testExportRoute, "f", "other-device")
	if err := handler.ExportMP4(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

func TestExportMP4_CancellationKillsFfmpeg(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cancellation test requires a POSIX shell for the stub ffmpeg")
	}
	store := newExportStorage(t)
	// Create a minimal HLS layout so the handler gets past the 404 check
	// and actually spawns the ffmpeg stub.
	dir := filepath.Join(store.Path(testExportDongleID, testExportRoute, "0", ""), "fcamera")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create hls dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seg_000.ts"), []byte("fake"), 0644); err != nil {
		t.Fatalf("failed to write fake ts: %v", err)
	}

	// The stub sleeps long enough that we are guaranteed to cancel it
	// before it exits on its own. It also writes its PID so we can
	// confirm the process was actually reaped after cancellation.
	pidFile := filepath.Join(t.TempDir(), "pid")
	stub := writeSlowFfmpegStub(t, pidFile)

	handler := NewExportHandler(store)
	handler.SetFFmpegPath(stub)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		"/v1/routes/"+testExportDongleID+"/"+testExportRoute+"/export.mp4?camera=f", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues(testExportDongleID, testExportRoute)
	c.Set(middleware.ContextKeyDongleID, testExportDongleID)

	done := make(chan error, 1)
	go func() {
		done <- handler.ExportMP4(c)
	}()

	// Poll for the stub to write its pid so we know ffmpeg actually
	// started before we cancel. If the pid file never appears the test
	// fails with a clear error below.
	pidDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(pidDeadline) {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Cancel the request context; the handler must kill ffmpeg and
	// return promptly.
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after request cancellation")
	}

	// Verify the stub process is no longer running. We read the PID the
	// stub wrote on startup, then confirm the kernel has reaped it by
	// sending signal 0 and expecting an ESRCH.
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("failed to read pid file: %v", err)
	}
	pidStr := strings.TrimSpace(string(pidBytes))
	if pidStr == "" {
		t.Fatal("pid file empty -- stub did not start")
	}

	alive := true
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pidStr) {
			alive = false
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if alive {
		t.Fatalf("ffmpeg stub (pid %s) still alive after cancellation", pidStr)
	}
}

func TestExportHandler_RegisterRoutes(t *testing.T) {
	store := newExportStorage(t)
	handler := NewExportHandler(store)

	e := echo.New()
	g := e.Group("/v1/routes")
	handler.RegisterRoutes(g)

	want := "/v1/routes/:dongle_id/:route_name/export.mp4"
	found := false
	for _, r := range e.Routes() {
		if r.Method == http.MethodGet && r.Path == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected route GET %s to be registered", want)
	}
}

func TestBuildConcatList(t *testing.T) {
	got := buildConcatList([]string{"/a/b/seg_000.ts", "/a/b/seg_001.ts"})
	want := "ffconcat version 1.0\nfile 'file:/a/b/seg_000.ts'\nfile 'file:/a/b/seg_001.ts'\n"
	if got != want {
		t.Errorf("buildConcatList = %q, want %q", got, want)
	}

	// Single quote in path must be escaped.
	got = buildConcatList([]string{"/tmp/weird's/seg.ts"})
	if !strings.Contains(got, `file 'file:/tmp/weird'\''s/seg.ts'`) {
		t.Errorf("expected escaped single quote in concat list, got %q", got)
	}
}

func TestTSOrderKey(t *testing.T) {
	in := []string{"/x/seg_010.ts", "/x/seg_002.ts", "/x/seg_000.ts"}
	keys := make([]string, len(in))
	for i, p := range in {
		keys[i] = tsOrderKey(p)
	}
	// Confirm lexicographic order on keys matches numeric order on paths.
	if !(keys[2] < keys[1] && keys[1] < keys[0]) {
		t.Errorf("tsOrderKey produced non-numeric ordering: %v", keys)
	}
}

// writeSlowFfmpegStub writes a shell script that records its PID to
// pidFile, sleeps, and returns exit 0. Used to verify cancellation
// actually kills the child process.
func writeSlowFfmpegStub(t *testing.T, pidFile string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub ffmpeg requires a unix shell")
	}
	script := filepath.Join(t.TempDir(), "ffmpeg_stub.sh")
	// The stub records its own PID to pidFile, drains stdin so the
	// handler never blocks while writing the concat list, and sleeps
	// longer than any reasonable test timeout so cancellation -- not
	// natural exit -- is what ends the process.
	content := "#!/bin/sh\n" +
		"echo $$ > " + pidFile + "\n" +
		"cat > /dev/null &\n" +
		"sleep 30\n"
	if err := os.WriteFile(script, []byte(content), 0755); err != nil {
		t.Fatalf("failed to write stub ffmpeg: %v", err)
	}
	return script
}

// stubSuccessPath writes a no-op ffmpeg shim used when we only need the
// path to be valid (the tests that use it never actually invoke ffmpeg).
func stubSuccessPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return "true"
	}
	script := filepath.Join(t.TempDir(), "ffmpeg_noop.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("failed to write noop ffmpeg: %v", err)
	}
	return script
}

// processAlive probes whether a PID still exists by sending signal 0.
// Returns false once the kernel has reaped the process.
func processAlive(pidStr string) bool {
	pid := 0
	for _, c := range pidStr {
		if c < '0' || c > '9' {
			break
		}
		pid = pid*10 + int(c-'0')
	}
	if pid == 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}
