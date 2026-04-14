package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"comma-personal-backend/internal/storage"
)

// writeFakeFFmpeg creates a shell script that mimics FFmpeg behavior.
// mode controls the script's behavior:
//   - "success": creates the expected HLS output files
//   - "fail": exits with a non-zero status
//   - "fail-copy": fails on -c:v copy but succeeds on -c:v libx264
//   - "slow": reads from a fifo that blocks indefinitely (for cancellation testing)
func writeFakeFFmpeg(t *testing.T, dir, mode string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg script requires unix shell")
	}

	script := filepath.Join(dir, "ffmpeg")
	var content string

	// All scripts parse arguments the same way: walk the arg list to find
	// -hls_segment_filename value and the output path (last positional arg).
	argParsePreamble := `#!/bin/sh
OUTPUT=""
SEG_PATTERN=""
CODEC=""
PREV=""
for arg in "$@"; do
    if [ "$PREV" = "-hls_segment_filename" ]; then
        SEG_PATTERN="$arg"
    fi
    if [ "$PREV" = "-c:v" ]; then
        CODEC="$arg"
    fi
    PREV="$arg"
    OUTPUT="$arg"
done
`

	createOutputLogic := `
OUTDIR="$(dirname "$OUTPUT")"
mkdir -p "$OUTDIR"
printf "#EXTM3U\n#EXT-X-VERSION:3\n" > "$OUTPUT"
if [ -n "$SEG_PATTERN" ]; then
    SEG_DIR="$(dirname "$SEG_PATTERN")"
    mkdir -p "$SEG_DIR"
    SEG_FILE="$(echo "$SEG_PATTERN" | sed 's/%03d/000/')"
    printf "fake ts data" > "$SEG_FILE"
fi
`

	switch mode {
	case "success":
		content = argParsePreamble + createOutputLogic + "exit 0\n"
	case "fail":
		content = argParsePreamble + `
echo "Error: invalid input" >&2
exit 1
`
	case "fail-copy":
		content = argParsePreamble + `
if [ "$CODEC" = "copy" ]; then
    echo "Error: copy failed" >&2
    exit 1
fi
` + createOutputLogic + "exit 0\n"
	case "slow":
		// Create a named pipe and try to read from it -- blocks until killed
		content = argParsePreamble + `
FIFO="$(dirname "$0")/block_fifo"
mkfifo "$FIFO" 2>/dev/null || true
cat "$FIFO"
exit 0
`
	}

	if err := os.WriteFile(script, []byte(content), 0755); err != nil {
		t.Fatalf("failed to write fake ffmpeg: %v", err)
	}
	return script
}

// setupTestSegment creates a fake segment directory with camera files.
func setupTestSegment(t *testing.T, basePath, dongleID, route, segment string, cameras []string) {
	t.Helper()
	dir := filepath.Join(basePath, dongleID, route, segment)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create segment dir: %v", err)
	}
	for _, cam := range cameras {
		path := filepath.Join(dir, cam)
		if err := os.WriteFile(path, []byte("fake hevc data"), 0644); err != nil {
			t.Fatalf("failed to write camera file %s: %v", cam, err)
		}
	}
}

func TestTranscodeFile_Success(t *testing.T) {
	tmp := t.TempDir()
	ffmpeg := writeFakeFFmpeg(t, tmp, "success")

	store := storage.New(tmp)
	tr := New(store, 1)
	tr.SetFFmpegPath(ffmpeg)

	inputPath := filepath.Join(tmp, "test.hevc")
	if err := os.WriteFile(inputPath, []byte("fake hevc"), 0644); err != nil {
		t.Fatalf("failed to write input: %v", err)
	}

	outputDir := filepath.Join(tmp, "test_output")
	err := tr.TranscodeFile(context.Background(), inputPath, outputDir)
	if err != nil {
		t.Fatalf("TranscodeFile() returned error: %v", err)
	}

	indexPath := filepath.Join(outputDir, "index.m3u8")
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		t.Errorf("expected index.m3u8 to exist at %s", indexPath)
	}
}

func TestTranscodeFile_FFmpegNotFound(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)
	tr := New(store, 1)
	tr.SetFFmpegPath(filepath.Join(tmp, "nonexistent-ffmpeg"))

	inputPath := filepath.Join(tmp, "test.hevc")
	if err := os.WriteFile(inputPath, []byte("fake hevc"), 0644); err != nil {
		t.Fatalf("failed to write input: %v", err)
	}

	outputDir := filepath.Join(tmp, "test_output")
	err := tr.TranscodeFile(context.Background(), inputPath, outputDir)
	if err == nil {
		t.Fatal("TranscodeFile() expected error for missing FFmpeg, got nil")
	}
}

func TestTranscodeFile_FFmpegFails(t *testing.T) {
	tmp := t.TempDir()
	ffmpeg := writeFakeFFmpeg(t, tmp, "fail")

	store := storage.New(tmp)
	tr := New(store, 1)
	tr.SetFFmpegPath(ffmpeg)

	inputPath := filepath.Join(tmp, "test.hevc")
	if err := os.WriteFile(inputPath, []byte("fake hevc"), 0644); err != nil {
		t.Fatalf("failed to write input: %v", err)
	}

	outputDir := filepath.Join(tmp, "test_output")
	err := tr.TranscodeFile(context.Background(), inputPath, outputDir)
	if err == nil {
		t.Fatal("TranscodeFile() expected error for failed FFmpeg, got nil")
	}
}

func TestTranscodeFile_FallbackToReencode(t *testing.T) {
	tmp := t.TempDir()
	ffmpeg := writeFakeFFmpeg(t, tmp, "fail-copy")

	store := storage.New(tmp)
	tr := New(store, 1)
	tr.SetFFmpegPath(ffmpeg)

	inputPath := filepath.Join(tmp, "test.hevc")
	if err := os.WriteFile(inputPath, []byte("fake hevc"), 0644); err != nil {
		t.Fatalf("failed to write input: %v", err)
	}

	outputDir := filepath.Join(tmp, "test_output")
	err := tr.TranscodeFile(context.Background(), inputPath, outputDir)
	if err != nil {
		t.Fatalf("TranscodeFile() should succeed with re-encode fallback, got: %v", err)
	}

	indexPath := filepath.Join(outputDir, "index.m3u8")
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		t.Errorf("expected index.m3u8 to exist at %s after fallback", indexPath)
	}
}

func TestTranscodeFile_ContextCancelled(t *testing.T) {
	tmp := t.TempDir()
	ffmpeg := writeFakeFFmpeg(t, tmp, "slow")

	store := storage.New(tmp)
	tr := New(store, 1)
	tr.SetFFmpegPath(ffmpeg)

	inputPath := filepath.Join(tmp, "test.hevc")
	if err := os.WriteFile(inputPath, []byte("fake hevc"), 0644); err != nil {
		t.Fatalf("failed to write input: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	outputDir := filepath.Join(tmp, "test_output")
	err := tr.TranscodeFile(ctx, inputPath, outputDir)
	if err == nil {
		t.Fatal("TranscodeFile() expected error when context is cancelled, got nil")
	}
}

func TestProcessSegment_TranscodesAllCameras(t *testing.T) {
	tmp := t.TempDir()
	ffmpeg := writeFakeFFmpeg(t, tmp, "success")

	store := storage.New(tmp)
	tr := New(store, 1)
	tr.SetFFmpegPath(ffmpeg)

	dongle := "abc123"
	route := "2024-01-15--12-30-00"
	segment := "0"
	cameras := []string{"fcamera.hevc", "ecamera.hevc", "dcamera.hevc"}
	setupTestSegment(t, tmp, dongle, route, segment, cameras)

	err := tr.ProcessSegment(context.Background(), dongle, route, segment)
	if err != nil {
		t.Fatalf("ProcessSegment() returned error: %v", err)
	}

	for _, cam := range cameras {
		base := cam[:len(cam)-len(filepath.Ext(cam))]
		indexPath := filepath.Join(tmp, dongle, route, segment, base, "index.m3u8")
		if _, err := os.Stat(indexPath); os.IsNotExist(err) {
			t.Errorf("expected HLS output for %s at %s", cam, indexPath)
		}
	}
}

func TestProcessSegment_SkipsMissingCameras(t *testing.T) {
	tmp := t.TempDir()
	ffmpeg := writeFakeFFmpeg(t, tmp, "success")

	store := storage.New(tmp)
	tr := New(store, 1)
	tr.SetFFmpegPath(ffmpeg)

	dongle := "abc123"
	route := "2024-01-15--12-30-00"
	segment := "0"
	setupTestSegment(t, tmp, dongle, route, segment, []string{"fcamera.hevc"})

	err := tr.ProcessSegment(context.Background(), dongle, route, segment)
	if err != nil {
		t.Fatalf("ProcessSegment() returned error: %v", err)
	}

	indexPath := filepath.Join(tmp, dongle, route, segment, "fcamera", "index.m3u8")
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		t.Error("expected HLS output for fcamera")
	}

	for _, cam := range []string{"ecamera", "dcamera"} {
		dir := filepath.Join(tmp, dongle, route, segment, cam)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("expected no output directory for %s", cam)
		}
	}
}

func TestProcessSegment_SkipsAlreadyTranscoded(t *testing.T) {
	tmp := t.TempDir()
	// Use fail mode -- if it tries to transcode, it will error
	ffmpeg := writeFakeFFmpeg(t, tmp, "fail")

	store := storage.New(tmp)
	tr := New(store, 1)
	tr.SetFFmpegPath(ffmpeg)

	dongle := "abc123"
	route := "2024-01-15--12-30-00"
	segment := "0"
	setupTestSegment(t, tmp, dongle, route, segment, []string{"fcamera.hevc"})

	// Pre-create HLS output so it looks already transcoded
	hlsDir := filepath.Join(tmp, dongle, route, segment, "fcamera")
	if err := os.MkdirAll(hlsDir, 0755); err != nil {
		t.Fatalf("failed to create HLS dir: %v", err)
	}
	indexPath := filepath.Join(hlsDir, "index.m3u8")
	if err := os.WriteFile(indexPath, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatalf("failed to write index: %v", err)
	}

	err := tr.ProcessSegment(context.Background(), dongle, route, segment)
	if err != nil {
		t.Fatalf("ProcessSegment() should skip already-transcoded files, got: %v", err)
	}
}

func TestNew_MinConcurrency(t *testing.T) {
	store := storage.New(t.TempDir())
	tr := New(store, 0)
	if tr.concurrency != 1 {
		t.Errorf("New(store, 0) concurrency = %d, want 1", tr.concurrency)
	}

	tr = New(store, -5)
	if tr.concurrency != 1 {
		t.Errorf("New(store, -5) concurrency = %d, want 1", tr.concurrency)
	}
}

func TestEnqueue_FullChannel(t *testing.T) {
	store := storage.New(t.TempDir())
	tr := New(store, 1)
	for i := 0; i < 100; i++ {
		if !tr.Enqueue("d", "r", "s") {
			t.Fatalf("Enqueue failed at i=%d, expected success", i)
		}
	}
	if tr.Enqueue("d", "r", "s") {
		t.Error("Enqueue should return false when job channel is full")
	}
}

func TestStartStop(t *testing.T) {
	tmp := t.TempDir()
	ffmpeg := writeFakeFFmpeg(t, tmp, "success")

	store := storage.New(tmp)
	tr := New(store, 2)
	tr.SetFFmpegPath(ffmpeg)

	dongle := "abc123"
	route := "2024-01-15--12-30-00"
	segment := "0"
	setupTestSegment(t, tmp, dongle, route, segment, []string{"fcamera.hevc"})

	ctx := context.Background()
	tr.Start(ctx)

	tr.Enqueue(dongle, route, segment)

	// Give workers time to process
	time.Sleep(500 * time.Millisecond)

	tr.Stop()

	indexPath := filepath.Join(tmp, dongle, route, segment, "fcamera", "index.m3u8")
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		t.Error("expected HLS output after background processing")
	}
}

func TestStop_Idempotent(t *testing.T) {
	store := storage.New(t.TempDir())
	tr := New(store, 1)
	tr.Start(context.Background())

	// Calling Stop multiple times should not panic
	tr.Stop()
	tr.Stop()
	tr.Stop()
}
