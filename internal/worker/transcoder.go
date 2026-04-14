package worker

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"comma-personal-backend/internal/storage"
)

// cameraFiles lists the HEVC camera files found in each segment.
var cameraFiles = []string{
	"fcamera.hevc",
	"ecamera.hevc",
	"dcamera.hevc",
}

// Transcoder converts HEVC video files to HLS format using FFmpeg.
// It can process individual files, full segments, or run as a background
// worker that watches for new uploads.
type Transcoder struct {
	storage     *storage.Storage
	concurrency int
	ffmpegPath  string

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	jobs   chan transcodeJob
}

// transcodeJob represents a single segment to be transcoded.
type transcodeJob struct {
	dongleID string
	route    string
	segment  string
}

// New creates a Transcoder backed by the given storage.
// concurrency controls how many FFmpeg processes run in parallel.
func New(store *storage.Storage, concurrency int) *Transcoder {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Transcoder{
		storage:     store,
		concurrency: concurrency,
		ffmpegPath:  "ffmpeg",
		jobs:        make(chan transcodeJob, 100),
	}
}

// SetFFmpegPath overrides the FFmpeg binary path (useful for testing).
func (t *Transcoder) SetFFmpegPath(path string) {
	t.ffmpegPath = path
}

// Start launches the background worker goroutines. It blocks until ctx
// is cancelled or Stop is called. If called while workers are already
// running, the previous workers are stopped first to prevent leaks.
func (t *Transcoder) Start(ctx context.Context) {
	t.mu.Lock()
	if t.cancel != nil {
		t.cancel()
		t.mu.Unlock()
		t.wg.Wait()
		t.mu.Lock()
	}
	ctx, t.cancel = context.WithCancel(ctx)
	t.mu.Unlock()

	for i := 0; i < t.concurrency; i++ {
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.worker(ctx)
		}()
	}
}

// Stop signals all workers to shut down and waits for them to finish.
// It is safe to call multiple times and to call Start again afterwards.
func (t *Transcoder) Stop() {
	t.mu.Lock()
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.mu.Unlock()
	t.wg.Wait()
}

// Enqueue submits a segment for background transcoding. It returns false
// if the job channel is full.
func (t *Transcoder) Enqueue(dongleID, route, segment string) bool {
	select {
	case t.jobs <- transcodeJob{dongleID: dongleID, route: route, segment: segment}:
		return true
	default:
		return false
	}
}

// worker drains the job channel, transcoding each segment until ctx is done.
func (t *Transcoder) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-t.jobs:
			if !ok {
				return
			}
			if err := t.ProcessSegment(ctx, job.dongleID, job.route, job.segment); err != nil {
				log.Printf("failed to transcode segment %s/%s/%s: %v",
					job.dongleID, job.route, job.segment, err)
			}
		}
	}
}

// ProcessSegment transcodes all camera HEVC files in the given segment.
// It skips files that have already been transcoded.
func (t *Transcoder) ProcessSegment(ctx context.Context, dongleID, route, segment string) error {
	var errs []string
	for _, camera := range cameraFiles {
		if !t.storage.Exists(dongleID, route, segment, camera) {
			continue
		}

		// Output directory name is the camera filename without extension
		// e.g. "fcamera.hevc" -> "fcamera"
		base := strings.TrimSuffix(camera, filepath.Ext(camera))
		hlsDir := filepath.Join(base)
		indexFile := filepath.Join(hlsDir, "index.m3u8")

		// Skip if already transcoded
		if t.storage.Exists(dongleID, route, segment, indexFile) {
			continue
		}

		inputPath := t.storage.Path(dongleID, route, segment, camera)
		outputDir := filepath.Join(filepath.Dir(inputPath), base)

		if err := t.TranscodeFile(ctx, inputPath, outputDir); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", camera, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to transcode segment: %s", strings.Join(errs, "; "))
	}
	return nil
}

// TranscodeFile converts a single HEVC file to HLS format.
// It first tries container copy (-c:v copy) for speed, and falls back to
// re-encoding with libx264 if the copy fails.
func (t *Transcoder) TranscodeFile(ctx context.Context, inputPath, outputDir string) error {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	indexPath := filepath.Join(outputDir, "index.m3u8")
	segPattern := filepath.Join(outputDir, "seg_%03d.ts")

	// Try container copy first (fast, no re-encoding)
	err := t.runFFmpeg(ctx, inputPath, indexPath, segPattern, "copy")
	if err == nil {
		return nil
	}

	// Clean up partial output from failed copy attempt
	t.cleanDir(outputDir)

	// Fall back to re-encoding with libx264
	if err := t.runFFmpeg(ctx, inputPath, indexPath, segPattern, "libx264"); err != nil {
		return fmt.Errorf("failed to transcode (re-encode fallback): %w", err)
	}
	return nil
}

// runFFmpeg executes the FFmpeg command with the given video codec.
func (t *Transcoder) runFFmpeg(ctx context.Context, inputPath, indexPath, segPattern, vcodec string) error {
	args := []string{
		"-i", inputPath,
		"-c:v", vcodec,
		"-hls_time", "6",
		"-hls_list_size", "0",
		"-hls_segment_filename", segPattern,
		indexPath,
	}

	cmd := exec.CommandContext(ctx, t.ffmpegPath, args...)
	// Create a new process group so cancellation kills the entire tree
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the process group, not just the leader
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("failed to run ffmpeg: %w", ctx.Err())
		}
		return fmt.Errorf("failed to run ffmpeg (codec=%s): %s: %w", vcodec, string(output), err)
	}
	return nil
}

// cleanDir removes all files in a directory but keeps the directory itself.
func (t *Transcoder) cleanDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		os.Remove(filepath.Join(dir, entry.Name()))
	}
}
