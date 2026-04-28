package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/redaction"
	"comma-personal-backend/internal/storage"
)

// rbStubDB implements db.DBTX so the RedactionBuilder's
// ListDetectionsForRoute call resolves to a canned list.
type rbStubDB struct {
	rows []db.ListDetectionsForRouteRow
}

func (m *rbStubDB) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *rbStubDB) Query(_ context.Context, _ string, _ ...interface{}) (pgx.Rows, error) {
	return &rbStubRows{rows: m.rows}, nil
}

func (m *rbStubDB) QueryRow(_ context.Context, _ string, _ ...interface{}) pgx.Row {
	return nil
}

type rbStubRows struct {
	rows []db.ListDetectionsForRouteRow
	idx  int
}

func (r *rbStubRows) Close()                                       {}
func (r *rbStubRows) Err() error                                   { return nil }
func (r *rbStubRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *rbStubRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *rbStubRows) Next() bool                                   { r.idx++; return r.idx <= len(r.rows) }
func (r *rbStubRows) Values() ([]interface{}, error)               { return nil, nil }
func (r *rbStubRows) RawValues() [][]byte                          { return nil }
func (r *rbStubRows) Conn() *pgx.Conn                              { return nil }
func (r *rbStubRows) Scan(dest ...interface{}) error {
	if r.idx == 0 || r.idx > len(r.rows) {
		return nil
	}
	src := r.rows[r.idx-1]
	if d, ok := dest[0].(*int64); ok {
		*d = src.ID
	}
	if d, ok := dest[1].(*string); ok {
		*d = src.DongleID
	}
	if d, ok := dest[2].(*string); ok {
		*d = src.Route
	}
	if d, ok := dest[3].(*int32); ok {
		*d = src.Segment
	}
	if d, ok := dest[4].(*int32); ok {
		*d = src.FrameOffsetMs
	}
	if d, ok := dest[5].(*[]byte); ok {
		*d = src.PlateCiphertext
	}
	if d, ok := dest[6].(*[]byte); ok {
		*d = src.PlateHash
	}
	if d, ok := dest[7].(*[]byte); ok {
		*d = src.Bbox
	}
	if d, ok := dest[8].(*float32); ok {
		*d = src.Confidence
	}
	if d, ok := dest[9].(*bool); ok {
		*d = src.OcrCorrected
	}
	if d, ok := dest[10].(*pgtype.Float8); ok {
		*d = src.GpsLat
	}
	if d, ok := dest[11].(*pgtype.Float8); ok {
		*d = src.GpsLng
	}
	if d, ok := dest[12].(*pgtype.Float4); ok {
		*d = src.GpsHeadingDeg
	}
	if d, ok := dest[13].(*pgtype.Timestamptz); ok {
		*d = src.FrameTs
	}
	if d, ok := dest[14].(*pgtype.Text); ok {
		*d = src.ThumbPath
	}
	if d, ok := dest[15].(*pgtype.Timestamptz); ok {
		*d = src.CreatedAt
	}
	return nil
}

// writeTinyQcameraTS writes a tiny but valid MPEG-TS file via ffmpeg
// so the builder has a real input to work against. Mirrors the
// fixture helper in internal/api/export_test.go.
func writeTinyQcameraTS(t *testing.T, ffmpegPath, outPath string) {
	t.Helper()
	cmd := exec.Command(ffmpegPath,
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-f", "lavfi",
		"-i", "color=black:s=64x32:d=0.5:r=10",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-pix_fmt", "yuv420p",
		"-f", "mpegts",
		outPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create qcamera.ts fixture: %v: %s", err, string(out))
	}
}

func TestRedactionBuilderInvalidateRouteContract(t *testing.T) {
	// Sanity check: redaction.InvalidateRoute is the documented
	// public API the future alpr-manual-correction-api feature will
	// call. This test asserts the path-shape contract by laying out
	// a fake cached variant and proving InvalidateRoute deletes it.
	root := t.TempDir()
	dongle := "abc"
	route := "2024-01-01--12-00-00"
	dir := filepath.Join(root, dongle, route, "0", redaction.RedactedQcameraDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("X"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	deleted, err := redaction.InvalidateRoute(root, dongle, route)
	if err != nil {
		t.Fatalf("InvalidateRoute: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("variant dir still present: %v", err)
	}
}

func TestRedactionBuilderTriggerIsIdempotent(t *testing.T) {
	store := storage.New(t.TempDir())
	b := NewRedactionBuilder(nil, store, 1)
	// Fire two triggers without starting the worker; the second must
	// be a no-op (return false) because the inflight map already has
	// the route.
	if !b.Trigger("d", "r") {
		t.Fatal("first trigger should return true")
	}
	if b.Trigger("d", "r") {
		t.Error("second trigger should be deduped")
	}
}

func TestRedactionBuilderRendersCachedVariant(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available; skipping")
	}

	root := t.TempDir()
	dongle := "abc"
	route := "2024-01-01--12-00-00"
	segDir := filepath.Join(root, dongle, route, "0")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTinyQcameraTS(t, ffmpegPath, filepath.Join(segDir, "qcamera.ts"))

	store := storage.New(root)

	bboxJSON, _ := json.Marshal(map[string]float64{
		"x": 100, "y": 50, "w": 800, "h": 400,
	})
	// Use the actual sqlc Queries with our stub.
	queries := db.New(&rbStubDB{rows: []db.ListDetectionsForRouteRow{
		{ID: 1, DongleID: dongle, Route: route, Segment: 0,
			FrameOffsetMs: 100, Bbox: bboxJSON, Confidence: 0.9,
			FrameTs: pgtype.Timestamptz{Time: time.Now(), Valid: true}},
	}})

	b := NewRedactionBuilder(queries, store, 1)
	b.SetFFmpegPath(ffmpegPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.buildRoute(ctx, dongle, route); err != nil {
		t.Fatalf("buildRoute: %v", err)
	}

	idx := filepath.Join(segDir, redaction.RedactedQcameraDir, "index.m3u8")
	if _, err := os.Stat(idx); err != nil {
		t.Fatalf("redacted index.m3u8 missing: %v", err)
	}
	contents, err := os.ReadFile(idx)
	if err != nil {
		t.Fatalf("read playlist: %v", err)
	}
	if len(contents) == 0 {
		t.Errorf("playlist empty")
	}
}

func TestRedactionBuilderSkipsSegmentsWithoutQcamera(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available; skipping")
	}
	root := t.TempDir()
	dongle := "abc"
	route := "r"
	// Create three segment dirs but only segment 1 has qcamera.ts.
	for _, seg := range []string{"0", "1", "2"} {
		dir := filepath.Join(root, dongle, route, seg)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	writeTinyQcameraTS(t, ffmpegPath, filepath.Join(root, dongle, route, "1", "qcamera.ts"))

	store := storage.New(root)
	queries := db.New(&rbStubDB{}) // no detections
	b := NewRedactionBuilder(queries, store, 1)
	b.SetFFmpegPath(ffmpegPath)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.buildRoute(ctx, dongle, route); err != nil {
		t.Fatalf("buildRoute: %v", err)
	}
	// Only segment 1 should have a redacted variant.
	for seg, want := range map[string]bool{"0": false, "1": true, "2": false} {
		dir := filepath.Join(root, dongle, route, seg, redaction.RedactedQcameraDir)
		_, err := os.Stat(dir)
		exists := err == nil
		if exists != want {
			t.Errorf("seg %s variant exists=%v, want %v", seg, exists, want)
		}
	}
}
