package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fabricatedTree is a helper that creates a populated STORAGE_PATH with a
// couple of devices, each containing multiple routes and segments. Returns
// a map of dongleID -> {bytes, routes} describing the fabricated layout.
type fabricatedStats struct {
	bytes  int64
	routes int
}

func fabricateDeviceTree(t *testing.T, base string) map[string]fabricatedStats {
	t.Helper()

	// Device "alpha": two routes, each with two segments holding two files.
	// Device "beta":  one route with one segment holding one file.
	layout := []struct {
		dongle, route, segment, filename, content string
	}{
		{"alpha", "2024-01-01--10-00-00", "0", "rlog", "alpha-r1-s0-rlog-data"},
		{"alpha", "2024-01-01--10-00-00", "0", "qlog", "alpha-qlog"},
		{"alpha", "2024-01-01--10-00-00", "1", "rlog", "alpha-rlog-seg1"},
		{"alpha", "2024-01-02--09-30-00", "0", "fcamera.hevc", "alpha-fcam"},
		{"alpha", "2024-01-02--09-30-00", "0", "dcamera.hevc", "alpha-dcam-bytes"},
		{"alpha", "2024-01-02--09-30-00", "2", "qcamera.ts", "alpha-qcam-ts"},
		{"beta", "2024-05-05--14-15-00", "0", "rlog", "beta-rlog-data"},
	}

	stats := map[string]fabricatedStats{}
	routesSeen := map[string]map[string]struct{}{}

	for _, f := range layout {
		dir := filepath.Join(base, f.dongle, f.route, f.segment)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("failed to create %q: %v", dir, err)
		}
		path := filepath.Join(dir, f.filename)
		if err := os.WriteFile(path, []byte(f.content), 0o644); err != nil {
			t.Fatalf("failed to write %q: %v", path, err)
		}

		s := stats[f.dongle]
		s.bytes += int64(len(f.content))
		stats[f.dongle] = s

		if routesSeen[f.dongle] == nil {
			routesSeen[f.dongle] = map[string]struct{}{}
		}
		routesSeen[f.dongle][f.route] = struct{}{}
	}

	for dongle, routes := range routesSeen {
		s := stats[dongle]
		s.routes = len(routes)
		stats[dongle] = s
	}
	return stats
}

func TestUsageWalksDeviceTree(t *testing.T) {
	base := t.TempDir()
	s := New(base)
	want := fabricateDeviceTree(t, base)

	report, err := s.Usage(context.Background(), false)
	if err != nil {
		t.Fatalf("Usage() error: %v", err)
	}

	if len(report.Devices) != len(want) {
		t.Fatalf("Devices len = %d, want %d: %+v", len(report.Devices), len(want), report.Devices)
	}

	// Devices are returned sorted alphabetically so alpha < beta.
	for i, d := range report.Devices {
		if i > 0 && report.Devices[i-1].DongleID >= d.DongleID {
			t.Errorf("devices not sorted: %q came after %q", d.DongleID, report.Devices[i-1].DongleID)
		}
		w, ok := want[d.DongleID]
		if !ok {
			t.Errorf("unexpected device %q in report", d.DongleID)
			continue
		}
		if d.Bytes != w.bytes {
			t.Errorf("%s bytes = %d, want %d", d.DongleID, d.Bytes, w.bytes)
		}
		if d.RouteCount != w.routes {
			t.Errorf("%s routes = %d, want %d", d.DongleID, d.RouteCount, w.routes)
		}
	}

	var wantTotal int64
	for _, v := range want {
		wantTotal += v.bytes
	}
	if report.TotalBytes != wantTotal {
		t.Errorf("TotalBytes = %d, want %d", report.TotalBytes, wantTotal)
	}

	if report.FilesystemTotalBytes == 0 {
		t.Error("FilesystemTotalBytes = 0, want > 0 from statfs")
	}
	if report.FilesystemAvailableBytes == 0 {
		t.Error("FilesystemAvailableBytes = 0, want > 0 from statfs")
	}
	if report.FilesystemAvailableBytes > report.FilesystemTotalBytes {
		t.Errorf("FilesystemAvailableBytes (%d) > FilesystemTotalBytes (%d)",
			report.FilesystemAvailableBytes, report.FilesystemTotalBytes)
	}
	if report.ComputedAt.IsZero() {
		t.Error("ComputedAt is zero, want a set time")
	}
}

func TestUsageEmptyStorageRoot(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	report, err := s.Usage(context.Background(), false)
	if err != nil {
		t.Fatalf("Usage() on empty tree: %v", err)
	}
	if len(report.Devices) != 0 {
		t.Errorf("len(Devices) = %d, want 0", len(report.Devices))
	}
	if report.TotalBytes != 0 {
		t.Errorf("TotalBytes = %d, want 0", report.TotalBytes)
	}
}

func TestUsageMissingStorageRoot(t *testing.T) {
	// Storage path does not exist (fresh deploy). Usage should not error.
	base := filepath.Join(t.TempDir(), "does-not-exist-yet")
	s := New(base)

	report, err := s.Usage(context.Background(), false)
	if err == nil {
		// statfs on the nonexistent path *will* fail; that is acceptable.
		if len(report.Devices) != 0 {
			t.Errorf("expected 0 devices for missing base, got %d", len(report.Devices))
		}
		return
	}
	if !strings.Contains(err.Error(), "statfs") {
		t.Fatalf("expected statfs error for missing base, got: %v", err)
	}
}

func TestUsageCachesResults(t *testing.T) {
	base := t.TempDir()
	s := New(base)
	fabricateDeviceTree(t, base)

	first, err := s.Usage(context.Background(), false)
	if err != nil {
		t.Fatalf("first Usage() error: %v", err)
	}

	// Add a new file after the first call. Because we just computed, the
	// cache should still hold the original value and hide this change.
	dir := filepath.Join(base, "alpha", "2024-01-01--10-00-00", "0")
	newFile := filepath.Join(dir, "extra")
	if err := os.WriteFile(newFile, []byte("extra-bytes-added-later"), 0o644); err != nil {
		t.Fatalf("failed to add late file: %v", err)
	}

	second, err := s.Usage(context.Background(), false)
	if err != nil {
		t.Fatalf("second Usage() error: %v", err)
	}
	if second.TotalBytes != first.TotalBytes {
		t.Errorf("TotalBytes changed without forceRefresh: got %d, want cached %d",
			second.TotalBytes, first.TotalBytes)
	}
	if !second.ComputedAt.Equal(first.ComputedAt) {
		t.Errorf("ComputedAt changed on cached read: %v -> %v", first.ComputedAt, second.ComputedAt)
	}
}

func TestUsageForceRefreshBypassesCache(t *testing.T) {
	base := t.TempDir()
	s := New(base)
	fabricateDeviceTree(t, base)

	first, err := s.Usage(context.Background(), false)
	if err != nil {
		t.Fatalf("first Usage() error: %v", err)
	}

	dir := filepath.Join(base, "alpha", "2024-01-01--10-00-00", "0")
	newFile := filepath.Join(dir, "extra")
	extra := []byte("extra-bytes-added-later")
	if err := os.WriteFile(newFile, extra, 0o644); err != nil {
		t.Fatalf("failed to add late file: %v", err)
	}

	fresh, err := s.Usage(context.Background(), true)
	if err != nil {
		t.Fatalf("forceRefresh Usage() error: %v", err)
	}
	wantDelta := int64(len(extra))
	if fresh.TotalBytes-first.TotalBytes != wantDelta {
		t.Errorf("fresh TotalBytes = %d, want first(%d) + %d", fresh.TotalBytes, first.TotalBytes, wantDelta)
	}
	if !fresh.ComputedAt.After(first.ComputedAt) && !fresh.ComputedAt.Equal(first.ComputedAt) {
		// Time monotonicity: fresh must not be earlier than first.
		t.Errorf("fresh ComputedAt (%v) earlier than first (%v)", fresh.ComputedAt, first.ComputedAt)
	}
}

func TestUsageCacheExpires(t *testing.T) {
	base := t.TempDir()
	s := New(base)
	fabricateDeviceTree(t, base)

	// Shrink the cache TTL to make the test fast, then prime the cache.
	cache := s.usageCacheInstance()
	cache.mu.Lock()
	cache.ttl = 1 * time.Millisecond
	cache.mu.Unlock()

	first, err := s.Usage(context.Background(), false)
	if err != nil {
		t.Fatalf("first Usage() error: %v", err)
	}

	extra := []byte("expired-cache-extra")
	dir := filepath.Join(base, "alpha", "2024-01-01--10-00-00", "0")
	if err := os.WriteFile(filepath.Join(dir, "extra"), extra, 0o644); err != nil {
		t.Fatalf("failed to add late file: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	expired, err := s.Usage(context.Background(), false)
	if err != nil {
		t.Fatalf("post-expiry Usage() error: %v", err)
	}
	if expired.TotalBytes-first.TotalBytes != int64(len(extra)) {
		t.Errorf("expected cache to expire; TotalBytes %d -> %d, delta = %d, want %d",
			first.TotalBytes, expired.TotalBytes, expired.TotalBytes-first.TotalBytes, len(extra))
	}
}

func TestUsageIgnoresNonDirectoriesAtRoot(t *testing.T) {
	base := t.TempDir()
	s := New(base)

	// Create a legitimate device + a stray top-level file.
	deviceDir := filepath.Join(base, "alpha", "2024-01-01--10-00-00", "0")
	if err := os.MkdirAll(deviceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deviceDir, "rlog"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "README.txt"), []byte("top-level note"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	report, err := s.Usage(context.Background(), false)
	if err != nil {
		t.Fatalf("Usage() error: %v", err)
	}
	if len(report.Devices) != 1 {
		t.Fatalf("expected 1 device (stray file at root ignored), got %d", len(report.Devices))
	}
	if report.Devices[0].DongleID != "alpha" {
		t.Errorf("DongleID = %q, want alpha", report.Devices[0].DongleID)
	}
	if report.Devices[0].RouteCount != 1 {
		t.Errorf("RouteCount = %d, want 1", report.Devices[0].RouteCount)
	}
}

func TestUsageContextCancellation(t *testing.T) {
	base := t.TempDir()
	s := New(base)
	fabricateDeviceTree(t, base)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Usage(ctx, true)
	if err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
}
