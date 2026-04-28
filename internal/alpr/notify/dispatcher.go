package notify

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
)

// notifyQuerier is the subset of *db.Queries the dispatcher uses. Carved
// out so tests can supply an in-memory fake without spinning up a real
// Postgres. Exposed as an unexported interface so external callers
// (cmd/server, tests in this package) wire *db.Queries directly while
// in-package tests build a fake.
type notifyQuerier interface {
	GetWatchlistByHash(ctx context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error)
	GetLastNotificationSent(ctx context.Context, plateHash []byte) (db.AlprNotificationsSent, error)
	UpsertNotificationSent(ctx context.Context, arg db.UpsertNotificationSentParams) error
}

// PayloadEnricher resolves the in-event payload (which carries only a
// plate hash + severity + route) into the full AlertPayload (decrypted
// plate text, vehicle attributes, evidence). The enricher is supplied
// by the cmd/server wiring layer because it needs the keyring and
// detection / signature queries that this package intentionally does
// not depend on. Returning an error is non-fatal -- the dispatcher
// logs at warn and skips the alert (a future event will retry).
//
// Implementations must be safe for concurrent use; the dispatcher
// invokes the enricher inline before the per-sender fan-out, but a
// fast-path test endpoint may call it from multiple goroutines.
type PayloadEnricher interface {
	Enrich(ctx context.Context, base AlertPayload) (AlertPayload, error)
}

// PayloadEnricherFunc adapts a plain function into PayloadEnricher.
type PayloadEnricherFunc func(ctx context.Context, base AlertPayload) (AlertPayload, error)

// Enrich implements PayloadEnricher.
func (f PayloadEnricherFunc) Enrich(ctx context.Context, base AlertPayload) (AlertPayload, error) {
	return f(ctx, base)
}

// SendResult is the per-sender outcome reported by Test (and surfaced
// by the operator-facing /v1/alpr/notify/test endpoint).
type SendResult struct {
	Sender string `json:"sender"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// Config bundles the dispatcher's runtime knobs. All fields are read
// once at construction; changing them at runtime requires constructing
// a fresh Dispatcher.
type Config struct {
	// MinSeverity is the threshold at and above which an alert is
	// considered for dispatch. Below this value the dispatcher drops
	// the event silently (it is logged at debug for diagnostics).
	MinSeverity int

	// DedupHours is the per-plate dedup window. 0 disables dedup
	// entirely (every accepted alert is dispatched). Negative values
	// are clamped to 0.
	DedupHours int

	// SendTimeout caps each sender's Send call. Defaults to 10s when
	// zero. The dispatcher gives every sender its own ctx with this
	// deadline so a stuck transport does not block its sibling.
	SendTimeout time.Duration

	// DashboardURL is the absolute base URL the dispatcher uses to
	// construct deep-link URLs (DashboardURL + "/alpr/plates/" +
	// hash_b64). Empty leaves the field empty in the payload.
	DashboardURL string

	// Now is a clock hook for tests. Defaults to time.Now.
	Now func() time.Time
}

// Dispatcher is the live notify pipeline. Construct via New and feed
// it AlertCreated events through Dispatch (or the heuristic-event
// adapter exposed by cmd/server). Test invokes every configured sender
// with a synthetic payload, bypassing the dedup ledger but otherwise
// honouring the regular dispatch path.
type Dispatcher struct {
	queries  notifyQuerier
	senders  []Sender
	enricher PayloadEnricher
	cfg      Config
}

// New builds a Dispatcher. nil senders are filtered out so callers can
// pass [NewEmailSender(...), NewWebhookSender(...)] without nil
// guarding each one. queries / enricher are required (nil queries means
// the dispatcher cannot enforce dedup or whitelist; nil enricher means
// AlertCreated events cannot be turned into AlertPayloads).
func New(queries notifyQuerier, senders []Sender, enricher PayloadEnricher, cfg Config) *Dispatcher {
	filtered := make([]Sender, 0, len(senders))
	for _, s := range senders {
		if s == nil {
			continue
		}
		filtered = append(filtered, s)
	}
	if cfg.SendTimeout == 0 {
		cfg.SendTimeout = 10 * time.Second
	}
	if cfg.DedupHours < 0 {
		cfg.DedupHours = 0
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Dispatcher{
		queries:  queries,
		senders:  filtered,
		enricher: enricher,
		cfg:      cfg,
	}
}

// SenderCount reports how many senders are wired. Useful for the
// no-config no-op test and for the startup log line that announces
// how many channels are active.
func (d *Dispatcher) SenderCount() int { return len(d.senders) }

// SenderNames returns the stable Name() values of every wired sender,
// preserving the order the dispatcher will fan out to. Used by the test
// endpoint's results array and by startup logging.
func (d *Dispatcher) SenderNames() []string {
	out := make([]string, 0, len(d.senders))
	for _, s := range d.senders {
		out = append(out, s.Name())
	}
	return out
}

// Dispatch is the main entry: takes a base AlertPayload (PlateHashB64,
// Severity, Route, DongleID populated), enriches it (decrypted plate +
// vehicle + evidence), and -- subject to the severity / dedup /
// whitelist filters -- fans out to every configured sender.
//
// Returns nil even on per-sender failures; failures are logged at warn
// and surfaced through the dedup ledger update (the ledger only
// advances when at least one sender succeeded, so a fully-failed
// dispatch leaves the next event eligible).
func (d *Dispatcher) Dispatch(ctx context.Context, base AlertPayload, plateHash []byte) error {
	if len(d.senders) == 0 {
		// No senders configured -- not an error. The acceptance
		// criterion explicitly requires this branch to be a no-op.
		return nil
	}
	if base.Severity < d.cfg.MinSeverity {
		log.Printf("alpr notify: severity %d below threshold %d; skipping", base.Severity, d.cfg.MinSeverity)
		return nil
	}
	if d.queries != nil && len(plateHash) > 0 {
		// Whitelist suppression. Defense in depth -- the heuristic
		// already suppresses whitelisted plates, but we double-check
		// here so a future code path that forwards an alert without
		// the heuristic's filter still gets the suppression.
		wl, err := d.queries.GetWatchlistByHash(ctx, plateHash)
		switch {
		case err == nil:
			if wl.Kind == "whitelist" {
				log.Printf("alpr notify: plate is whitelisted; suppressing dispatch")
				return nil
			}
		case errors.Is(err, pgx.ErrNoRows):
			// no row -- proceed.
		default:
			log.Printf("alpr notify: watchlist lookup failed: %v", err)
		}

		// Dedup window. A 0-hour window disables the check.
		if d.cfg.DedupHours > 0 {
			row, err := d.queries.GetLastNotificationSent(ctx, plateHash)
			switch {
			case err == nil:
				if row.LastSentAt.Valid {
					elapsed := d.cfg.Now().Sub(row.LastSentAt.Time)
					window := time.Duration(d.cfg.DedupHours) * time.Hour
					if elapsed < window {
						log.Printf("alpr notify: within dedup window (%s < %s); skipping", elapsed.Truncate(time.Second), window)
						return nil
					}
				}
			case errors.Is(err, pgx.ErrNoRows):
				// never sent -- proceed.
			default:
				log.Printf("alpr notify: dedup lookup failed: %v", err)
			}
		}
	}

	enriched := base
	if d.enricher != nil {
		out, err := d.enricher.Enrich(ctx, base)
		if err != nil {
			log.Printf("alpr notify: enrich payload: %v; skipping", err)
			return nil
		}
		enriched = out
	}
	if enriched.Plate == "" {
		log.Printf("alpr notify: enriched payload has empty plate text; skipping")
		return nil
	}
	enriched.DashboardURL = buildDashboardURL(d.cfg.DashboardURL, enriched.PlateHashB64)

	results := d.fanOut(ctx, enriched)
	anyOK := false
	for _, r := range results {
		if r.OK {
			anyOK = true
		} else {
			log.Printf("alpr notify: sender %s failed: %s", r.Sender, r.Error)
		}
	}
	if anyOK && d.queries != nil && len(plateHash) > 0 {
		err := d.queries.UpsertNotificationSent(ctx, db.UpsertNotificationSentParams{
			PlateHash:  plateHash,
			LastSentAt: pgtype.Timestamptz{Time: d.cfg.Now(), Valid: true},
		})
		if err != nil {
			log.Printf("alpr notify: upsert dedup ledger: %v", err)
		}
	}
	return nil
}

// Test invokes every configured sender with the supplied payload and
// returns the per-sender outcome. The dedup ledger is NOT consulted or
// updated; the synthetic payload is the operator's setup-validation
// tool and must always reach every channel that is currently wired.
//
// The whitelist check is also skipped: a synthetic plate string
// ("TEST-123") would never be on the watchlist anyway, and skipping
// the lookup keeps the test fast and DB-independent.
func (d *Dispatcher) Test(ctx context.Context, payload AlertPayload) []SendResult {
	if len(d.senders) == 0 {
		return nil
	}
	enriched := payload
	enriched.DashboardURL = buildDashboardURL(d.cfg.DashboardURL, payload.PlateHashB64)
	return d.fanOut(ctx, enriched)
}

// fanOut runs every sender in parallel with its own bounded ctx. The
// returned slice is in the same order as Senders() for deterministic
// reporting.
func (d *Dispatcher) fanOut(ctx context.Context, payload AlertPayload) []SendResult {
	results := make([]SendResult, len(d.senders))
	var wg sync.WaitGroup
	for i, s := range d.senders {
		i, s := i, s
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendCtx, cancel := context.WithTimeout(ctx, d.cfg.SendTimeout)
			defer cancel()
			err := s.Send(sendCtx, payload)
			results[i].Sender = s.Name()
			if err != nil {
				results[i].OK = false
				results[i].Error = err.Error()
			} else {
				results[i].OK = true
			}
		}()
	}
	wg.Wait()
	return results
}

// EncodePlateHashB64 encodes a plate-hash byte slice as the same
// base64-URL-no-padding string the rest of the ALPR API uses for
// /v1/plates/:hash_b64. Exposed so cmd/server (which speaks
// heuristic.AlertCreated, not AlertPayload) can fill in the field
// before calling Dispatch.
func EncodePlateHashB64(plateHash []byte) string {
	return base64.RawURLEncoding.EncodeToString(plateHash)
}

// buildDashboardURL composes the deep link to the plate detail page.
// Returns "" when the base URL is empty (the formatters check for "" to
// decide whether to render the link).
func buildDashboardURL(base, hashB64 string) string {
	if base == "" || hashB64 == "" {
		return ""
	}
	return fmt.Sprintf("%s/alpr/plates/%s", base, hashB64)
}
