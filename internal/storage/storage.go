package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Storage manages dashcam files on the local filesystem.
// Files are organized as basePath/dongleID/route/segment/filename.
type Storage struct {
	basePath string
}

// New creates a Storage rooted at the given base path.
func New(basePath string) *Storage {
	return &Storage{basePath: basePath}
}

// Store writes data from the reader to the correct path on disk,
// creating any necessary directories along the way.
func (s *Storage) Store(dongleID, route, segment, filename string, data io.Reader) error {
	dir := filepath.Join(s.basePath, dongleID, route, segment)

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("failed to resolve path: %w", err)
	}
	absBase, err := filepath.Abs(s.basePath)
	if err != nil {
		return fmt.Errorf("failed to resolve base path: %w", err)
	}
	if !strings.HasPrefix(absDir, absBase+string(filepath.Separator)) && absDir != absBase {
		return fmt.Errorf("path traversal detected")
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directories: %w", err)
	}

	path := filepath.Join(dir, filename)
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}

	_, copyErr := io.Copy(f, data)
	closeErr := f.Close()

	if copyErr != nil {
		os.Remove(path)
		return fmt.Errorf("failed to write file: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(path)
		return fmt.Errorf("failed to close file: %w", closeErr)
	}

	return nil
}

// Path returns the absolute file path for the given file components.
func (s *Storage) Path(dongleID, route, segment, filename string) string {
	return filepath.Join(s.basePath, dongleID, route, segment, filename)
}

// Exists reports whether the specified file exists on disk.
func (s *Storage) Exists(dongleID, route, segment, filename string) bool {
	path := filepath.Join(s.basePath, dongleID, route, segment, filename)
	_, err := os.Stat(path)
	return err == nil
}

// ListSegments returns a sorted list of segment numbers for the given
// dongle and route. Each segment is a subdirectory whose name is a
// non-negative integer.
func (s *Storage) ListSegments(dongleID, route string) ([]int, error) {
	dir := filepath.Join(s.basePath, dongleID, route)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read route directory: %w", err)
	}

	var segments []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		n, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		segments = append(segments, n)
	}

	sort.Ints(segments)
	return segments, nil
}
