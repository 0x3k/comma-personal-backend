package storage

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func mustStore(t *testing.T, s *Storage, dongleID, route, segment, filename, content string) {
	t.Helper()
	err := s.Store(dongleID, route, segment, filename, strings.NewReader(content))
	if err != nil {
		t.Fatalf("Store(%q, %q, %q, %q) failed: %v", dongleID, route, segment, filename, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) failed: %v", path, err)
	}
	return string(data)
}

func TestStore(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	dongleID := "abc123"
	route := "2024-01-15--12-30-00"
	segment := "0"
	filename := "fcamera.hevc"
	content := "fake video data"

	mustStore(t, s, dongleID, route, segment, filename, content)

	wantPath := filepath.Join(base, dongleID, route, segment, filename)
	got := readFile(t, wantPath)
	if got != content {
		t.Errorf("stored file content = %q, want %q", got, content)
	}
}

func TestStoreCreatesDirectories(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	dongleID := "device42"
	route := "2024-06-01--08-00-00"
	segment := "5"
	filename := "qlog"

	// The nested directories do not exist yet.
	dir := filepath.Join(base, dongleID, route, segment)
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("expected directory to not exist before Store")
	}

	mustStore(t, s, dongleID, route, segment, filename, "log data")

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected a directory, got a file")
	}
}

func TestStoreMultipleFiles(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	files := []struct {
		filename string
		content  string
	}{
		{"fcamera.hevc", "front camera"},
		{"ecamera.hevc", "wide camera"},
		{"dcamera.hevc", "driver camera"},
		{"rlog", "raw log"},
		{"qlog", "quick log"},
		{"qcamera.ts", "quick camera"},
	}

	dongleID := "dongle1"
	route := "2024-03-10--14-00-00"
	segment := "0"

	for _, f := range files {
		mustStore(t, s, dongleID, route, segment, f.filename, f.content)
	}

	for _, f := range files {
		path := filepath.Join(base, dongleID, route, segment, f.filename)
		got := readFile(t, path)
		if got != f.content {
			t.Errorf("file %q: content = %q, want %q", f.filename, got, f.content)
		}
	}
}

func TestPath(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	tests := []struct {
		name     string
		dongleID string
		route    string
		segment  string
		filename string
	}{
		{
			name:     "typical segment file",
			dongleID: "abc123",
			route:    "2024-01-15--12-30-00",
			segment:  "0",
			filename: "fcamera.hevc",
		},
		{
			name:     "higher segment number",
			dongleID: "xyz789",
			route:    "2024-07-20--09-15-30",
			segment:  "42",
			filename: "rlog",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Path(tt.dongleID, tt.route, tt.segment, tt.filename)
			want := filepath.Join(base, tt.dongleID, tt.route, tt.segment, tt.filename)
			if got != want {
				t.Errorf("Path() = %q, want %q", got, want)
			}
		})
	}
}

func TestExists(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	dongleID := "dev99"
	route := "2024-05-01--10-00-00"
	segment := "0"
	filename := "rlog"

	// File does not exist yet.
	if s.Exists(dongleID, route, segment, filename) {
		t.Error("Exists() = true before file was stored, want false")
	}

	mustStore(t, s, dongleID, route, segment, filename, "some data")

	if !s.Exists(dongleID, route, segment, filename) {
		t.Error("Exists() = false after file was stored, want true")
	}

	// Non-existent file in the same segment.
	if s.Exists(dongleID, route, segment, "nonexistent.file") {
		t.Error("Exists() = true for non-existent file, want false")
	}
}

func TestListSegments(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	dongleID := "dev01"
	route := "2024-02-20--16-45-00"

	// Create segment directories in non-sorted order.
	segmentNums := []int{3, 0, 7, 1, 12}
	for _, n := range segmentNums {
		dir := filepath.Join(base, dongleID, route, strconv.Itoa(n))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create segment dir %d: %v", n, err)
		}
	}

	got, err := s.ListSegments(dongleID, route)
	if err != nil {
		t.Fatalf("ListSegments() error: %v", err)
	}

	want := []int{0, 1, 3, 7, 12}
	if len(got) != len(want) {
		t.Fatalf("ListSegments() returned %d segments, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListSegments()[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestListSegmentsIgnoresNonNumericDirs(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	dongleID := "dev02"
	route := "2024-04-10--11-00-00"

	routeDir := filepath.Join(base, dongleID, route)

	// Create a mix of numeric segment dirs and non-numeric entries.
	dirs := []string{"0", "1", "2", "notes", ".hidden", "metadata"}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(routeDir, d), 0755); err != nil {
			t.Fatalf("failed to create dir %q: %v", d, err)
		}
	}

	// Also create a regular file (not a directory) with a numeric name.
	if err := os.WriteFile(filepath.Join(routeDir, "99"), []byte("not a dir"), 0644); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}

	got, err := s.ListSegments(dongleID, route)
	if err != nil {
		t.Fatalf("ListSegments() error: %v", err)
	}

	want := []int{0, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("ListSegments() returned %d segments, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListSegments()[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestListSegmentsNonExistentRoute(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	_, err := s.ListSegments("nodevice", "noroute")
	if err == nil {
		t.Error("ListSegments() for non-existent route returned nil error, want error")
	}
}

func TestListSegmentsEmpty(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	dongleID := "dev03"
	route := "2024-08-01--06-00-00"

	// Create empty route directory.
	routeDir := filepath.Join(base, dongleID, route)
	if err := os.MkdirAll(routeDir, 0755); err != nil {
		t.Fatalf("failed to create route dir: %v", err)
	}

	got, err := s.ListSegments(dongleID, route)
	if err != nil {
		t.Fatalf("ListSegments() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("ListSegments() returned %d segments for empty route, want 0", len(got))
	}
}

// failAfterReader returns n bytes of data then fails with the given error.
type failAfterReader struct {
	n   int
	err error
	pos int
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.pos >= r.n {
		return 0, r.err
	}
	remaining := r.n - r.pos
	toWrite := len(p)
	if toWrite > remaining {
		toWrite = remaining
	}
	for i := 0; i < toWrite; i++ {
		p[i] = 'x'
	}
	r.pos += toWrite
	return toWrite, nil
}

var _ io.Reader = (*failAfterReader)(nil)

func TestStoreRemovesPartialFileOnCopyError(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	dongleID := "dev50"
	route := "2024-09-01--12-00-00"
	segment := "0"
	filename := "fcamera.hevc"

	copyErr := errors.New("simulated read failure")
	reader := &failAfterReader{n: 512, err: copyErr}

	err := s.Store(dongleID, route, segment, filename, reader)
	if err == nil {
		t.Fatal("Store() returned nil error, want error from failing reader")
	}
	if !errors.Is(err, copyErr) {
		t.Fatalf("Store() error = %v, want wrapped %v", err, copyErr)
	}

	// The partial file must not remain on disk.
	if s.Exists(dongleID, route, segment, filename) {
		t.Error("partial file still exists after failed Store, want it removed")
	}
}
