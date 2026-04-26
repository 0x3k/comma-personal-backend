package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"comma-personal-backend/internal/storage"
)

// fakeJPEG is a minimal 2-byte marker that stands in for a real JPEG. The
// worker does not inspect bytes; tests only need to confirm the file was
// written (and possibly preserved).
var fakeJPEG = []byte{0xff, 0xd8}

// writeQcamera creates a fake qcamera.ts file in the given segment
// directory, creating intermediate directories as needed.
func writeQcamera(t *testing.T, basePath, dongleID, route, segment string) {
	t.Helper()
	dir := filepath.Join(basePath, dongleID, route, segment)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, QcameraFileName)
	if err := os.WriteFile(path, []byte("fake ts"), 0644); err != nil {
		t.Fatalf("write qcamera: %v", err)
	}
}

// successRunner returns an ffmpegRunner that writes fakeJPEG bytes to the
// output path, simulating a successful ffmpeg invocation.
func successRunner() ffmpegRunner {
	return func(_ context.Context, _, _ /* inputPath */, outputPath string, _ int) error {
		if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
			return err
		}
		return os.WriteFile(outputPath, fakeJPEG, 0644)
	}
}

// failureRunner returns an ffmpegRunner that always errors without
// touching the output path.
func failureRunner(msg string) ffmpegRunner {
	return func(_ context.Context, _, _, _ string, _ int) error {
		return errors.New(msg)
	}
}

func TestThumbnailer_GenerateHappyPath(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)
	th := NewThumbnailer(nil, store)
	th.SetFFmpegRunner(successRunner())

	dongle := "abc123"
	route := "2024-01-15--12-30-00"
	writeQcamera(t, tmp, dongle, route, "0")

	if err := th.generate(context.Background(), dongle, route, "0"); err != nil {
		t.Fatalf("generate() error = %v, want nil", err)
	}

	thumbPath := filepath.Join(tmp, dongle, route, "0", ThumbnailFileName)
	data, err := os.ReadFile(thumbPath)
	if err != nil {
		t.Fatalf("read thumbnail: %v", err)
	}
	if len(data) == 0 {
		t.Error("thumbnail file is empty")
	}

	// The temp file must have been renamed away.
	if _, err := os.Stat(thumbPath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file should have been renamed; err=%v", err)
	}
}

func TestThumbnailer_GenerateMissingQcamera(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)
	th := NewThumbnailer(nil, store)

	// Runner that would normally succeed; should never be called because
	// the worker stats the input first.
	called := false
	th.SetFFmpegRunner(func(_ context.Context, _, _, _ string, _ int) error {
		called = true
		return nil
	})

	err := th.generate(context.Background(), "abc", "route", "0")
	if err == nil {
		t.Fatal("generate() should error when qcamera.ts is missing")
	}
	if called {
		t.Error("ffmpeg runner should not be invoked when input is missing")
	}
}

func TestThumbnailer_ProcessFailureWritesFailureMarker(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)
	th := NewThumbnailer(nil, store)
	th.SetFFmpegRunner(failureRunner("ffmpeg broke"))

	// Freeze time so the marker contents are deterministic and inCooldown
	// checks are predictable.
	frozen := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	th.SetNowFunc(func() time.Time { return frozen })

	dongle := "abc123"
	route := "2024-01-15--12-30-00"
	writeQcamera(t, tmp, dongle, route, "0")

	th.process(context.Background(), thumbnailJob{dongleID: dongle, route: route, segment: "0"})

	markerPath := filepath.Join(tmp, dongle, route, "0", ThumbnailFailedFileName)
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("failure marker missing: %v", err)
	}
	if len(data) == 0 {
		t.Error("failure marker is empty")
	}

	// The route must now be in cooldown, so the next tick skips it.
	if !th.inCooldown(dongle, route, "0") {
		t.Error("inCooldown should be true immediately after a failure")
	}
}

func TestThumbnailer_FailedRouteSkippedUntilCooldownExpires(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)
	th := NewThumbnailer(nil, store)

	callCount := 0
	th.SetFFmpegRunner(func(_ context.Context, _, _, _ string, _ int) error {
		callCount++
		return errors.New("ffmpeg broke")
	})
	th.SetRetryAfter(10 * time.Minute)

	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	th.SetNowFunc(func() time.Time { return now })

	dongle := "abc123"
	route := "2024-01-15--12-30-00"
	writeQcamera(t, tmp, dongle, route, "0")

	// First failure populates the marker.
	th.process(context.Background(), thumbnailJob{dongleID: dongle, route: route, segment: "0"})
	if callCount != 1 {
		t.Fatalf("first pass: expected 1 ffmpeg call, got %d", callCount)
	}
	if !th.inCooldown(dongle, route, "0") {
		t.Fatal("after failure: expected cooldown to be true")
	}

	// Advance the clock but stay within the retry window: still skipped.
	now = now.Add(5 * time.Minute)
	if !th.inCooldown(dongle, route, "0") {
		t.Error("within cooldown window: expected inCooldown to stay true")
	}

	// Jump past the retry window: cooldown should lift.
	now = now.Add(6 * time.Minute)
	if th.inCooldown(dongle, route, "0") {
		t.Error("past cooldown window: expected inCooldown to be false")
	}
}

func TestThumbnailer_SuccessClearsStaleFailureMarker(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)
	th := NewThumbnailer(nil, store)
	th.SetFFmpegRunner(successRunner())

	dongle := "abc123"
	route := "2024-01-15--12-30-00"
	writeQcamera(t, tmp, dongle, route, "0")

	// Pre-seed a stale failure marker.
	markerPath := filepath.Join(tmp, dongle, route, "0", ThumbnailFailedFileName)
	if err := os.WriteFile(markerPath, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0644); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	th.process(context.Background(), thumbnailJob{dongleID: dongle, route: route, segment: "0"})

	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("failure marker should be cleared after success; err=%v", err)
	}
	thumbPath := filepath.Join(tmp, dongle, route, "0", ThumbnailFileName)
	if _, err := os.Stat(thumbPath); err != nil {
		t.Errorf("thumbnail should exist after success; err=%v", err)
	}
}

func TestThumbnailer_SelectSegmentPrefersZero(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)
	th := NewThumbnailer(nil, store)

	dongle := "abc123"
	route := "2024-01-15--12-30-00"
	writeQcamera(t, tmp, dongle, route, "0")
	writeQcamera(t, tmp, dongle, route, "3")

	seg, ok := th.SelectSegment(dongle, route)
	if !ok {
		t.Fatal("SelectSegment returned false with segment 0 present")
	}
	if seg != "0" {
		t.Errorf("SelectSegment = %q, want %q", seg, "0")
	}
}

func TestThumbnailer_SelectSegmentFallbackToLowest(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)
	th := NewThumbnailer(nil, store)

	dongle := "abc123"
	route := "2024-01-15--12-30-00"

	// Segment 0 directory exists but without qcamera.ts (simulating a
	// segment whose qcamera has not uploaded yet). Segment 2 has it.
	dir0 := filepath.Join(tmp, dongle, route, "0")
	if err := os.MkdirAll(dir0, 0755); err != nil {
		t.Fatalf("mkdir seg 0: %v", err)
	}
	writeQcamera(t, tmp, dongle, route, "2")
	writeQcamera(t, tmp, dongle, route, "5")

	seg, ok := th.SelectSegment(dongle, route)
	if !ok {
		t.Fatal("SelectSegment returned false despite segment 2 having qcamera")
	}
	if seg != "2" {
		t.Errorf("SelectSegment = %q, want lowest-numbered segment with qcamera (%q)", seg, "2")
	}
}

func TestThumbnailer_SelectSegmentNoUsable(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)
	th := NewThumbnailer(nil, store)

	if _, ok := th.SelectSegment("abc", "route"); ok {
		t.Error("SelectSegment should return false when route directory is missing")
	}

	// Create the route dir with a segment that has no qcamera.ts.
	dir := filepath.Join(tmp, "abc", "route", "0")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, ok := th.SelectSegment("abc", "route"); ok {
		t.Error("SelectSegment should return false when no segment has qcamera.ts")
	}
}

func TestThumbnailer_EnqueueFullQueue(t *testing.T) {
	store := storage.New(t.TempDir())
	th := NewThumbnailer(nil, store)

	// Drain queue with different routes so each entry is unique in the
	// inflight set.
	for i := 0; i < DefaultThumbnailQueueDepth; i++ {
		route := "route-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		if !th.Enqueue("d", route, "0") {
			t.Fatalf("Enqueue failed at i=%d", i)
		}
	}
	if th.Enqueue("d", "one-more", "0") {
		t.Error("Enqueue should fail when channel is full")
	}
}

func TestThumbnailer_StartStopLifecycle(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)
	th := NewThumbnailer(nil, store)
	th.SetFFmpegRunner(successRunner())
	// Scan queries are nil, so the scanner is a no-op. That lets us test
	// Start/Stop without spinning up a database.
	th.SetScanInterval(1 * time.Hour)

	ctx := context.Background()
	th.Start(ctx)

	dongle := "abc"
	route := "r1"
	writeQcamera(t, tmp, dongle, route, "0")

	if !th.Enqueue(dongle, route, "0") {
		t.Fatal("Enqueue returned false on empty queue")
	}

	// Give the generator time to run.
	deadline := time.Now().Add(2 * time.Second)
	thumbPath := filepath.Join(tmp, dongle, route, "0", ThumbnailFileName)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(thumbPath); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	th.Stop()

	if _, err := os.Stat(thumbPath); err != nil {
		t.Fatalf("thumbnail not produced by worker: %v", err)
	}

	// Second Stop must not panic.
	th.Stop()
}

func TestThumbnailer_StopStartStop(t *testing.T) {
	store := storage.New(t.TempDir())
	th := NewThumbnailer(nil, store)
	th.SetScanInterval(1 * time.Hour)
	ctx := context.Background()

	th.Start(ctx)
	th.Stop()
	th.Start(ctx)
	th.Stop()
}

func TestThumbnailer_ScanOnceSkipsExisting(t *testing.T) {
	// scanOnce depends on queries; with nil queries it is a no-op, which
	// is exactly the behaviour we want to verify: the worker does not
	// crash in a minimal (db-less) setup.
	tmp := t.TempDir()
	store := storage.New(tmp)
	th := NewThumbnailer(nil, store)
	th.scanOnce(context.Background())
}

// TestDefaultFFmpegRunner_PassesExplicitMuxerFlag is a regression test for
// the .tmp-extension muxer-inference bug: when the worker writes to
// "thumbnail.jpg.tmp", ffmpeg cannot infer the output format from the
// extension and fails with "Unable to choose an output format". The fix is
// an explicit `-f mjpeg` flag before the output path. This test stubs
// ffmpeg with a shell script that records its argv, then asserts the flag
// is present and that the runner succeeds against a .tmp output path.
func TestDefaultFFmpegRunner_PassesExplicitMuxerFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script ffmpeg stub is unix-only")
	}
	tmp := t.TempDir()

	// Fake ffmpeg: write its argv to a log file, then touch the output path
	// (last positional argument) so the renaming step in generate() works
	// when this script is wired in via SetFFmpegPath.
	argLog := filepath.Join(tmp, "ffmpeg-args.log")
	scriptPath := filepath.Join(tmp, "ffmpeg")
	script := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$@\" > " + argLog + "\n" +
		"out=\"${@: -1}\"\n" +
		"printf 'fake jpeg' > \"$out\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}

	inputPath := filepath.Join(tmp, "qcamera.ts")
	if err := os.WriteFile(inputPath, []byte("fake ts"), 0644); err != nil {
		t.Fatalf("write input: %v", err)
	}
	// Use the same .tmp suffix the worker uses, so the test fails for the
	// same reason production failed before the fix.
	outputPath := filepath.Join(tmp, "thumbnail.jpg.tmp")

	if err := defaultFFmpegRunner(context.Background(), scriptPath, inputPath, outputPath, 320); err != nil {
		t.Fatalf("defaultFFmpegRunner returned error: %v", err)
	}

	logged, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	args := strings.Split(strings.TrimRight(string(logged), "\n"), "\n")

	// -f mjpeg must appear as adjacent args.
	foundMuxer := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-f" && args[i+1] == "mjpeg" {
			foundMuxer = true
			break
		}
	}
	if !foundMuxer {
		t.Errorf("expected `-f mjpeg` in ffmpeg args, got: %v", args)
	}

	// The output path must be the last positional arg (after the muxer
	// flag) so ffmpeg parses `-f mjpeg` as applying to the output.
	if len(args) == 0 || args[len(args)-1] != outputPath {
		t.Errorf("expected output path %q to be last arg, got: %v", outputPath, args)
	}
}

func TestThumbnailer_InCooldownBadMarker(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)
	th := NewThumbnailer(nil, store)

	dongle := "abc"
	route := "route"
	segment := "0"
	dir := filepath.Join(tmp, dongle, route, segment)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ThumbnailFailedFileName), []byte("not a timestamp"), 0644); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if th.inCooldown(dongle, route, segment) {
		t.Error("inCooldown should be false when marker is unparseable")
	}
}
