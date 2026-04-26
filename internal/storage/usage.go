package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultUsageCacheTTL is how long a UsageReport is served from the cache
// before Usage walks the filesystem again.
const DefaultUsageCacheTTL = 60 * time.Second

// DeviceUsage reports the disk consumption attributable to a single device.
type DeviceUsage struct {
	DongleID   string `json:"dongleId"`
	Bytes      int64  `json:"bytes"`
	RouteCount int    `json:"routeCount"`
}

// UsageReport summarizes storage consumption across every device tree under
// basePath, along with filesystem-wide totals reported by statfs.
type UsageReport struct {
	Devices                  []DeviceUsage `json:"devices"`
	TotalBytes               int64         `json:"totalBytes"`
	FilesystemTotalBytes     uint64        `json:"filesystemTotalBytes"`
	FilesystemAvailableBytes uint64        `json:"filesystemAvailableBytes"`
	ComputedAt               time.Time     `json:"computedAt"`
}

// usageCache holds the last UsageReport together with the moment it was
// computed so Storage can decide whether to reuse it.
type usageCache struct {
	mu         sync.Mutex
	report     *UsageReport
	computedAt time.Time
	ttl        time.Duration
}

// Usage walks basePath (one level of dongle directories, then one level of
// route directories per dongle) and reports the bytes stored per device
// alongside the filesystem totals returned by statfs.
//
// Results are memoized for DefaultUsageCacheTTL so this is safe to call on a
// request hot path. Pass forceRefresh=true to bypass the cache and recompute.
// If ctx is cancelled mid-walk, the partial walk is abandoned and ctx.Err()
// is returned.
func (s *Storage) Usage(ctx context.Context, forceRefresh bool) (*UsageReport, error) {
	cache := s.usageCacheInstance()

	cache.mu.Lock()
	defer cache.mu.Unlock()

	if !forceRefresh && cache.report != nil && time.Since(cache.computedAt) < cache.ttl {
		// Return a copy so callers cannot mutate cached state.
		cp := *cache.report
		// Seed the copy with a non-nil empty slice so JSON marshaling emits
		// `[]` instead of `null` when there are no devices yet (consumers
		// like the web Settings page call .length on the result).
		cp.Devices = append([]DeviceUsage{}, cache.report.Devices...)
		return &cp, nil
	}

	report, err := s.computeUsage(ctx)
	if err != nil {
		return nil, err
	}

	cache.report = report
	cache.computedAt = report.ComputedAt
	// Return a copy to keep the cached report immutable from callers.
	cp := *report
	cp.Devices = append([]DeviceUsage{}, report.Devices...)
	return &cp, nil
}

// usageCacheInstance lazily initializes and returns the Storage's shared
// usage cache. The double-checked locking pattern keeps concurrent callers
// from racing on the first Usage call.
func (s *Storage) usageCacheInstance() *usageCache {
	s.usageInitOnce.Do(func() {
		s.usageCache = &usageCache{ttl: DefaultUsageCacheTTL}
	})
	return s.usageCache
}

// computeUsage performs the filesystem walk and statfs call that back Usage.
func (s *Storage) computeUsage(ctx context.Context) (*UsageReport, error) {
	devices, total, err := s.walkDevices(ctx)
	if err != nil {
		return nil, err
	}

	var statfs unix.Statfs_t
	if err := unix.Statfs(s.basePath, &statfs); err != nil {
		return nil, fmt.Errorf("failed to statfs %q: %w", s.basePath, err)
	}

	blockSize := uint64(statfs.Bsize)
	totalBytes := uint64(statfs.Blocks) * blockSize
	availBytes := uint64(statfs.Bavail) * blockSize

	return &UsageReport{
		Devices:                  devices,
		TotalBytes:               total,
		FilesystemTotalBytes:     totalBytes,
		FilesystemAvailableBytes: availBytes,
		ComputedAt:               time.Now().UTC(),
	}, nil
}

// walkDevices enumerates the top-level dongle directories under basePath and
// sums the bytes beneath each one. If basePath does not exist yet (fresh
// deployment), the walk returns an empty slice and zero bytes rather than an
// error.
func (s *Storage) walkDevices(ctx context.Context) ([]DeviceUsage, int64, error) {
	entries, err := os.ReadDir(s.basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("failed to read storage root: %w", err)
	}

	devices := make([]DeviceUsage, 0, len(entries))
	var total int64

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if !entry.IsDir() {
			continue
		}
		dongleID := entry.Name()
		bytes, routes, err := s.walkDevice(ctx, dongleID)
		if err != nil {
			return nil, 0, err
		}
		devices = append(devices, DeviceUsage{
			DongleID:   dongleID,
			Bytes:      bytes,
			RouteCount: routes,
		})
		total += bytes
	}

	sort.Slice(devices, func(i, j int) bool {
		return devices[i].DongleID < devices[j].DongleID
	})
	return devices, total, nil
}

// walkDevice sums all regular-file bytes beneath basePath/dongleID and
// counts how many route-level subdirectories it contains. Route
// directories are the direct children of the dongle directory (routes are
// identified by `YYYY-MM-DD--HH-MM-SS`-style names on disk).
func (s *Storage) walkDevice(ctx context.Context, dongleID string) (int64, int, error) {
	deviceDir := filepath.Join(s.basePath, dongleID)

	routeEntries, err := os.ReadDir(deviceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("failed to read device directory %q: %w", deviceDir, err)
	}

	var bytes int64
	routeCount := 0
	for _, routeEntry := range routeEntries {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		if !routeEntry.IsDir() {
			// Stray files at the device root (if any) still count toward
			// the device's byte total so operators see the real footprint.
			info, err := routeEntry.Info()
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return 0, 0, fmt.Errorf("failed to stat %q: %w", routeEntry.Name(), err)
			}
			bytes += info.Size()
			continue
		}
		routeCount++
		routeBytes, err := walkBytes(ctx, filepath.Join(deviceDir, routeEntry.Name()))
		if err != nil {
			return 0, 0, err
		}
		bytes += routeBytes
	}

	return bytes, routeCount, nil
}

// walkBytes sums the sizes of every regular file rooted at path. Non-regular
// entries (symlinks, sockets, etc.) are skipped. A missing path is treated as
// zero bytes so a race with a concurrent delete does not fail the whole
// report.
func walkBytes(ctx context.Context, path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
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
		return 0, err
	}
	return total, nil
}
