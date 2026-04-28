package main

import (
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"comma-personal-backend/internal/alpr"
	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/metrics"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/storage"
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
