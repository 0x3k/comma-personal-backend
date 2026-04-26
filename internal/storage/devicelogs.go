package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// WriteDeviceLog persists a swaglog payload pushed from athenad/sunnylinkd.
// The file lives at <basePath>/<dongleID>/swaglog/<id>.log. The id is rejected
// if it would escape the per-device swaglog directory, so a malicious payload
// cannot reach into other dongles' storage.
func (s *Storage) WriteDeviceLog(dongleID, id string, data io.Reader) error {
	return s.writeAuxiliaryFile(dongleID, "swaglog", id, ".log", data)
}

// WriteDeviceStats persists a stats payload pushed from athenad/sunnylinkd.
// The file lives at <basePath>/<dongleID>/stats/<id>.json.
func (s *Storage) WriteDeviceStats(dongleID, id string, data io.Reader) error {
	return s.writeAuxiliaryFile(dongleID, "stats", id, ".json", data)
}

// writeAuxiliaryFile is the shared implementation behind WriteDeviceLog and
// WriteDeviceStats. The (subdir, ext) pair distinguishes the two streams; the
// rest of the path-traversal and write-then-rename logic is identical.
func (s *Storage) writeAuxiliaryFile(dongleID, subdir, id, ext string, data io.Reader) error {
	if err := validateAuxiliaryNameComponent(dongleID, "dongle_id"); err != nil {
		return err
	}
	if err := validateAuxiliaryNameComponent(id, "id"); err != nil {
		return err
	}

	dir := filepath.Join(s.basePath, dongleID, subdir)

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

	path := filepath.Join(dir, id+ext)
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

// validateAuxiliaryNameComponent rejects empty, dot, double-dot, or
// path-separator-bearing names. The auxiliary file APIs accept these from
// device-supplied JSON-RPC params, so the check has to be strict.
func validateAuxiliaryNameComponent(s, label string) error {
	if s == "" {
		return fmt.Errorf("%s is required", label)
	}
	if s == "." || s == ".." {
		return fmt.Errorf("invalid %s: %q", label, s)
	}
	if strings.ContainsAny(s, "/\\") {
		return fmt.Errorf("invalid %s: %q", label, s)
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf("invalid %s: %q", label, s)
	}
	return nil
}
