package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/metrics"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/storage"
	"comma-personal-backend/internal/worker"
	"comma-personal-backend/internal/ws"
)

// main is the bootstrap only. Route registration lives in routes.go and
// background goroutines live in workers.go -- add new handlers and workers
// there, not here.
func main() {
	// Load .env file if present; ignore error if file does not exist.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load configuration: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer pool.Close()

	queries := db.New(pool)
	store := storage.New(cfg.StoragePath)

	// Settings store for operator-configurable runtime values. Seed the
	// retention_days row from the env var on first boot so later API
	// overrides do not require a restart to take effect.
	settingsStore := settings.New(queries)
	if err := settingsStore.SeedIntIfMissing(context.Background(), settings.KeyRetentionDays, cfg.RetentionDays); err != nil {
		log.Printf("warning: failed to seed retention_days setting: %v", err)
	}

	// ALPR runtime settings: same seeding rationale as retention_days. The
	// master flag (alpr_enabled) is left unseeded so its absence is
	// indistinguishable from an explicit `false`, which is the safe default.
	// If the operator has flipped it on previously the existing row sticks
	// across restarts. logALPRStartup emits a single info line summarising
	// the merged settings only when ALPR is currently enabled, so the
	// expected default (off) produces no log noise.
	alprKeyring := verifyALPRKeyring(cfg.ALPR)
	seedALPRDefaults(settingsStore, cfg.ALPR)
	logALPRStartup(settingsStore, cfg.ALPR)

	// Metrics registry is shared across the process: the HTTP middleware,
	// the transcoder, the RPC caller, and the hub all observe into it, and
	// /metrics exposes it.
	m := metrics.New()

	// Redaction builder is constructed up-front (before setupRoutes)
	// so the share handler can be wired with a Trigger reference. It
	// stays unstarted here; startWorkers calls Start (or skips it
	// entirely when REDACTION_BUILDER_ENABLED=false). When the env
	// flag is off we leave d.redactionBuilder nil so the share handler
	// degrades gracefully -- viewers of redact_plates=true tokens
	// receive a 503 with no follow-up build, which is the correct
	// behaviour when ALPR is fully disabled.
	concurrency := envInt("REDACTION_BUILDER_CONCURRENCY", 1)
	redactionBuilder := worker.NewRedactionBuilder(queries, store, concurrency)

	// ALPR notify dispatcher: constructed up-front so setupRoutes can
	// register the operator-facing test endpoint and startWorkers can
	// spawn the AlertCreated subscriber goroutine. The dispatcher is
	// non-nil even when no ALPR_NOTIFY_* env vars are configured -- in
	// that case it has zero senders and Dispatch / Test are no-ops, so
	// the test endpoint reports "configured: false" cleanly.
	alprNotifyDispatcher := buildALPRNotifyDispatcher(cfg.ALPR, queries, alprKeyring)

	d := &deps{
		cfg:              cfg,
		pool:             pool,
		queries:          queries,
		store:            store,
		settings:         settingsStore,
		metrics:          m,
		hub:              ws.NewHubWithMetrics(m),
		rpcCaller:        ws.NewRPCCallerWithMetrics(m),
		sessionSecret:    []byte(cfg.SessionSecret),
		alprKeyring:      alprKeyring,
		redactionBuilder: redactionBuilder,
		alprNotify:       alprNotifyDispatcher,
	}

	e := echo.New()
	setupRoutes(e, d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startWorkers(ctx, d)

	s := &http.Server{
		Addr:              ":" + cfg.Port,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      5 * time.Minute, // allows time for large uploads
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("starting server on :%s", cfg.Port)
	if err := e.StartServer(s); err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
}
