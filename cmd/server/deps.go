package main

import (
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"comma-personal-backend/internal/alpr"
	alprcrypto "comma-personal-backend/internal/alpr/crypto"
	"comma-personal-backend/internal/alpr/heuristic"
	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/metrics"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/storage"
	"comma-personal-backend/internal/worker"
	"comma-personal-backend/internal/ws"
)

// deps bundles the long-lived dependencies built during bootstrap. It is
// threaded through setupRoutes and startWorkers so those helpers do not
// need to accept a growing list of positional arguments, and so adding a
// new dependency is a one-field edit rather than a signature change.
type deps struct {
	cfg           *config.Config
	pool          *pgxpool.Pool
	queries       *db.Queries
	store         *storage.Storage
	settings      *settings.Store
	metrics       *metrics.Metrics
	hub           *ws.Hub
	rpcCaller     *ws.RPCCaller
	sessionSecret []byte

	// alprClient is built lazily by ALPRClient() so toggling the
	// runtime alpr_enabled flag does NOT require a process restart.
	// We avoid constructing it at startup because the engine sidecar is
	// optional (gated by the `alpr` Docker Compose profile) and a
	// non-running sidecar would otherwise produce log noise on every
	// bootstrap.
	alprClientOnce sync.Once
	alprClient     *alpr.Client

	// alprFrames is the producer/consumer channel that connects the
	// frame-extractor worker (this PR) to the future detection worker.
	// Constructed at startup with capacity ALPR_EXTRACTOR_BUFFER so
	// the channel exists regardless of whether the runtime alpr_enabled
	// flag is on. The detection worker (later wave) ranges over this
	// channel; the extractor closes it on graceful shutdown.
	alprFrames chan worker.ExtractedFrame

	// alprKeyring holds the ALPR plate-text encryption + hash subkeys
	// derived from ALPR_ENCRYPTION_KEY. Loaded at startup by
	// verifyALPRKeyring so the detection worker can encrypt and hash
	// without paying the HKDF derivation per request and without
	// needing to read the env var itself. Nil when ALPR_ENCRYPTION_KEY
	// is unset; in that case the detection worker logs once at startup
	// and idles -- the operator must configure a key before enabling
	// ALPR (the PUT /v1/settings/alpr handler enforces this
	// precondition; the worker's nil-guard is defense-in-depth).
	alprKeyring *alprcrypto.Keyring

	// alprDetectionsComplete carries one event per route once the
	// detection worker has processed every fcamera segment. The
	// encounter aggregator subscribes here so it can collapse a
	// route's per-frame detections into per-encounter rows the moment
	// the detection pass is done -- without polling the database for
	// changes. The channel is buffered so a slow consumer cannot
	// stall the detection worker in the unlikely steady-state burst
	// of many simultaneous route completions.
	alprDetectionsComplete chan worker.RouteAlprDetectionsComplete

	// alprEncountersUpdated carries one event per route after the
	// encounter aggregator has (re)written its plate_encounters rows.
	// The downstream alpr-stalking-heuristic subscribes so it can
	// re-run only on plates whose state actually changed instead of
	// rescanning the entire encounter table on a periodic timer. The
	// channel is buffered so a slow heuristic does not stall the
	// aggregator in the unlikely steady-state burst of many
	// simultaneous route completions; the aggregator falls back to
	// dropping the event with a warn when full.
	alprEncountersUpdated chan worker.EncountersUpdated

	// alprAlertCreated carries one event per genuinely-new (or
	// strictly-upgraded) heuristic alert. Notification subsystems
	// (push, dashboard badge invalidation) subscribe here so the
	// alert UX surfaces promptly without polling. Buffered so a
	// slow notifier does not stall the heuristic worker; the
	// heuristic falls back to dropping the event with a warn when
	// full, and the watchlist row itself is the load-bearing record
	// (the alert is still discoverable on the next list-alerts
	// poll).
	alprAlertCreated chan heuristic.AlertCreated

	// redactionBuilder is the on-demand worker that renders the
	// cached qcamera-redacted HLS variant when a redacted-share link
	// triggers it. Started during workers wiring; the share handler
	// holds a (RedactionTrigger interface) reference to it so 503 +
	// Retry-After responses kick off a build.
	redactionBuilder *worker.RedactionBuilder

	// alprBackfill is the singleton goroutine that drives the opt-in
	// historical-backfill state machine. The API handler holds a
	// reference (via the ALPRBackfillTrigger interface) so a
	// successful POST /v1/alpr/backfill/start can wake the worker
	// without waiting for its idle poll.
	alprBackfill *worker.ALPRBackfill
}

// alprClientTimeout is the per-request budget the ALPR client applies on
// top of the engine's documented 5s server-side limit. Comfortable
// headroom for warmup or slow networks while still bounded so a stuck
// engine does not stall the workers indefinitely.
const alprClientTimeout = 10 * time.Second

// ALPRClient returns the lazily-initialised ALPR engine HTTP client. The
// first call constructs the underlying http.Client; later calls return
// the cached instance. Safe to call from multiple goroutines.
//
// Returns nil only when the deps struct is nil or the configured engine
// URL is empty -- both unreachable in practice (config.LoadALPR seeds a
// default), but the nil-guard keeps callers from segfaulting under
// pathological boot conditions.
func (d *deps) ALPRClient() *alpr.Client {
	if d == nil {
		return nil
	}
	d.alprClientOnce.Do(func() {
		if d.cfg == nil || d.cfg.ALPR == nil || d.cfg.ALPR.EngineURL == "" {
			return
		}
		d.alprClient = alpr.NewClient(d.cfg.ALPR.EngineURL, alprClientTimeout)
	})
	return d.alprClient
}
