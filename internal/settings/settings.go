// Package settings provides read/write access to the application's simple
// key/value settings table.
//
// It is intentionally small: callers either ask for the int value of a key
// (GetInt) or persist one (SetInt). The underlying storage is the settings
// SQL table managed via sqlc-generated queries.
//
// A missing key is reported as ErrNotFound so callers can fall back to their
// own defaults (for example, an env var).
package settings

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"comma-personal-backend/internal/db"
)

// Known setting keys. Kept as named constants so handlers and consumers use
// the same string everywhere.
const (
	// KeyRetentionDays is the number of days to keep non-preserved routes
	// before the cleanup worker deletes them. A value of 0 means
	// "never delete".
	KeyRetentionDays = "retention_days"

	// KeyTurnWindowSeconds is the sliding-window duration the turn
	// detector uses to compute (bearing_after - bearing_before). Larger
	// values smooth out GPS jitter at the cost of merging tightly spaced
	// turns; the default of 4s is a good balance for urban routes.
	KeyTurnWindowSeconds = "turn_window_seconds"

	// KeyTurnDeltaDegMin is the minimum absolute heading change (in
	// degrees) required to emit a turn. Larger values suppress sweeping
	// curves; the default of 35 degrees catches typical 90-degree
	// intersections without firing on highway lane changes.
	KeyTurnDeltaDegMin = "turn_delta_deg_min"

	// KeyTurnDedupSeconds is the suppression window after a turn fires
	// during which subsequent above-threshold deltas are ignored. The
	// default of 5s prevents a roundabout from emitting one turn per
	// vertex.
	KeyTurnDedupSeconds = "turn_dedup_seconds"
)

// ErrNotFound is returned when the requested key does not exist in the
// settings table. Callers that have a sensible default (for example the
// RETENTION_DAYS env var) should handle this explicitly.
var ErrNotFound = errors.New("settings: key not found")

// Querier is the subset of db.Queries that this package uses. Extracted as
// an interface so tests can supply a fake without spinning up a database.
type Querier interface {
	GetSetting(ctx context.Context, key string) (db.Setting, error)
	UpsertSetting(ctx context.Context, arg db.UpsertSettingParams) (db.Setting, error)
	InsertSettingIfMissing(ctx context.Context, arg db.InsertSettingIfMissingParams) error
}

// Store reads and writes typed values from the settings table.
type Store struct {
	q Querier
}

// New wraps the given Querier (typically a *db.Queries) in a Store.
func New(q Querier) *Store {
	return &Store{q: q}
}

// Get returns the raw string value for the given key. If the key is not
// present it returns ErrNotFound.
func (s *Store) Get(ctx context.Context, key string) (string, error) {
	row, err := s.q.GetSetting(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("settings: get %q: %w", key, err)
	}
	return row.Value, nil
}

// GetInt returns the integer value for the given key. If the stored value
// is not a valid integer the error wraps strconv.ErrSyntax. If the key is
// not present it returns ErrNotFound.
func (s *Store) GetInt(ctx context.Context, key string) (int, error) {
	raw, err := s.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("settings: value for %q is not an integer: %w", key, err)
	}
	return n, nil
}

// Set writes the given string value for key, creating or replacing the row.
func (s *Store) Set(ctx context.Context, key, value string) error {
	if _, err := s.q.UpsertSetting(ctx, db.UpsertSettingParams{
		Key:   key,
		Value: value,
	}); err != nil {
		return fmt.Errorf("settings: set %q: %w", key, err)
	}
	return nil
}

// SetInt writes the integer value for key.
func (s *Store) SetInt(ctx context.Context, key string, value int) error {
	return s.Set(ctx, key, strconv.Itoa(value))
}

// GetFloat returns the float64 value for the given key. If the stored
// value does not parse cleanly the error wraps strconv.ErrSyntax. If the
// key is not present it returns ErrNotFound.
func (s *Store) GetFloat(ctx context.Context, key string) (float64, error) {
	raw, err := s.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("settings: value for %q is not a float: %w", key, err)
	}
	return f, nil
}

// SetFloat writes the float64 value for key. The value is stored using
// strconv.FormatFloat with 'g' formatting and -1 precision so the round
// trip is lossless.
func (s *Store) SetFloat(ctx context.Context, key string, value float64) error {
	return s.Set(ctx, key, strconv.FormatFloat(value, 'g', -1, 64))
}

// SeedFloatIfMissing inserts value for key only if no row exists yet.
// Used at startup to push an env-var default into the database so a later
// runtime override via the API does not require a restart to take effect.
func (s *Store) SeedFloatIfMissing(ctx context.Context, key string, value float64) error {
	if err := s.q.InsertSettingIfMissing(ctx, db.InsertSettingIfMissingParams{
		Key:   key,
		Value: strconv.FormatFloat(value, 'g', -1, 64),
	}); err != nil {
		return fmt.Errorf("settings: seed %q: %w", key, err)
	}
	return nil
}

// SeedIntIfMissing inserts value for key only if no row exists yet. Used at
// startup to push the env-var default into the database so a later runtime
// override via the API does not require a restart to take effect.
func (s *Store) SeedIntIfMissing(ctx context.Context, key string, value int) error {
	if err := s.q.InsertSettingIfMissing(ctx, db.InsertSettingIfMissingParams{
		Key:   key,
		Value: strconv.Itoa(value),
	}); err != nil {
		return fmt.Errorf("settings: seed %q: %w", key, err)
	}
	return nil
}

// RetentionDays returns the effective retention window in days. The
// settings table takes precedence; if the row is missing or not a valid
// integer, envDefault is used as the fallback. A result of 0 means
// "never delete".
func (s *Store) RetentionDays(ctx context.Context, envDefault int) (int, error) {
	n, err := s.GetInt(ctx, KeyRetentionDays)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return envDefault, nil
		}
		return envDefault, err
	}
	return n, nil
}
