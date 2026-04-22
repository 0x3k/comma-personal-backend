package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Storage manages dashcam files on the local filesystem.
// Files are organized as basePath/dongleID/route/segment/filename.
type Storage struct {
	basePath string

	// usageInitOnce guards the lazy initialization of usageCache so the
	// first call to Usage() does not race with a concurrent caller.
	usageInitOnce sync.Once
	usageCache    *usageCache
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

// RouteDir returns the absolute on-disk path for a route (the directory
// that contains every segment folder for that route). It does not check
// for existence.
func (s *Storage) RouteDir(dongleID, route string) string {
	return filepath.Join(s.basePath, dongleID, route)
}

// RouteBytes returns the total size in bytes of every regular file beneath
// the route directory. A missing directory reports 0 bytes with no error so
// callers can use this to log "bytes freed" even when files were never
// uploaded. Symlinks and non-regular entries are skipped.
func (s *Storage) RouteBytes(dongleID, route string) (int64, error) {
	dir := s.RouteDir(dongleID, route)
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("failed to walk route directory: %w", err)
	}
	return total, nil
}

// RemoveRoute deletes the route directory and all files/segments beneath it.
// It is idempotent: a missing directory is not an error. The path is
// validated to stay under basePath so a maliciously crafted dongleID or
// route cannot escape the storage root.
func (s *Storage) RemoveRoute(dongleID, route string) error {
	dir := s.RouteDir(dongleID, route)

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("failed to resolve route path: %w", err)
	}
	absBase, err := filepath.Abs(s.basePath)
	if err != nil {
		return fmt.Errorf("failed to resolve base path: %w", err)
	}
	if !strings.HasPrefix(absDir, absBase+string(filepath.Separator)) {
		return fmt.Errorf("path traversal detected")
	}

	if err := os.RemoveAll(absDir); err != nil {
		return fmt.Errorf("failed to remove route directory: %w", err)
	}
	return nil
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
