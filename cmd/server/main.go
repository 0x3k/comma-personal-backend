package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"

	"comma-personal-backend/internal/api"
	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/geocode"
	"comma-personal-backend/internal/metrics"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/storage"
	"comma-personal-backend/internal/worker"
	"comma-personal-backend/internal/ws"
)

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

	// Metrics registry is shared across the process: the HTTP middleware,
	// the transcoder, the RPC caller, and the hub all observe into it, and
	// /metrics exposes it.
	m := metrics.New()

	e := echo.New()

	// Global rate limiter: 20 requests/second per IP.
	e.Use(echomw.RateLimiter(echomw.NewRateLimiterMemoryStore(20)))

	// Default body limit for JSON endpoints (1MB).
	e.Use(echomw.BodyLimit("1M"))

	// HTTP metrics middleware: registered once so every route is observed.
	// It skips /metrics internally to avoid self-observation noise.
	e.Use(metrics.EchoMiddleware(m))

	// Prometheus scrape endpoint. Intentionally unauthenticated per the
	// feature spec -- front with nginx if auth is needed.
	e.GET("/metrics", echo.WrapHandler(m.Handler()))

	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	// Device registration (unauthenticated).
	pilotAuth := api.NewPilotAuthHandler(queries, cfg)
	pilotAuth.RegisterRoutes(e)

	// Web UI authentication. Kept separate from the device-facing JWT auth so
	// operators without a SESSION_SECRET still get device uploads working.
	if cfg.UIAuthEnabled() {
		sessionHandler := api.NewSessionHandler(queries, cfg.SessionSecret)
		sessionHandler.RegisterRoutes(e)

		if err := api.BootstrapAdmin(context.Background(), queries, cfg.AdminUsername, cfg.AdminPassword); err != nil {
			log.Fatalf("failed to bootstrap admin user: %v", err)
		}
	} else {
		log.Printf("warning: SESSION_SECRET is not set; web UI authentication is disabled. Device auth is unaffected.")
	}

	// Authenticated API groups. Device endpoints carry a per-device JWT
	// signed with the private key openpilot generated during pilotauth; the
	// middleware verifies it against the public_key stored for that device.
	// Dashboard endpoints authenticate via a signed session cookie set by
	// POST /v1/session/login; shared read endpoints accept either.
	sessionSecret := []byte(cfg.SessionSecret)
	auth := middleware.JWTAuthFromDB(queries)
	sessionOnly := middleware.SessionRequired(sessionSecret)
	sessionOrJWT := middleware.SessionOrJWT(sessionSecret, queries)

	deviceHandler := api.NewDeviceHandler(queries)

	// /v1.1/devices/:dongle_id is the device self-info endpoint called by
	// openpilot; it stays JWT-only.
	v11 := e.Group("/v1.1", auth)
	deviceHandler.RegisterRoutes(v11)

	// Dashboard listing of all registered devices. Dashboard-facing, so
	// gated on SessionOrJWT.
	e.GET("/v1/devices", deviceHandler.ListDevices, sessionOrJWT)

	// Route listing and detail (dashboard reads; devices may still call).
	v1Route := e.Group("/v1/route", sessionOrJWT)
	routeHandler := api.NewRouteHandler(queries)
	routeHandler.RegisterRoutes(v1Route)

	// Plural /v1/routes/ path hosts route mutation and export endpoints so
	// they do not collide with /v1/route/:dongle_id. These are dashboard
	// operations (preserve flag, export downloads), shared with device
	// JWTs for consistency.
	v1Routes := e.Group("/v1/routes", sessionOrJWT)
	routeHandler.RegisterPreservedRoute(v1Routes)
	exportHandler := api.NewExportHandler(queries, store)
	exportHandler.RegisterRoutes(v1Routes)
	signalsHandler := api.NewSignalsHandler(queries, store)
	signalsHandler.RegisterRoutes(v1Routes)

	// Share link creation is a mutation (mints a signed token) so it rides
	// the session-only group. The public /v1/share/:token endpoints are
	// mounted directly on the top-level Echo instance below -- they must
	// not be gated on the session middleware because the whole point is
	// to let an unauthenticated viewer see a single shared route.
	shareHandler := api.NewShareHandler(queries, store, cfg.SessionSecret)
	v1RoutesSessionOnly := e.Group("/v1/routes", sessionOnly)
	shareHandler.RegisterCreateRoute(v1RoutesSessionOnly)
	shareHandler.RegisterPublicRoutes(e)

	// Trip stats: per-device lifetime totals + recent trip list on /v1, and
	// per-route aggregated trip detail on /v1/routes.
	tripHandler := api.NewTripHandler(queries)
	tripHandler.RegisterTripRoute(v1Routes)

	// Upload URL and file upload -- device-facing, JWT only.
	v14 := e.Group("/v1.4", auth)
	uploadHandler := api.NewUploadHandlerWithMetrics(store, queries, m)
	v14.GET("/:dongle_id/upload_url/", uploadHandler.GetUploadURL)
	v14.GET("/:dongle_id/upload_url", uploadHandler.GetUploadURL)

	uploadGroup := e.Group("/upload", auth, echomw.BodyLimit("100M"))
	uploadGroup.PUT("/:dongle_id/*", uploadHandler.UploadFile)

	// Device config parameters. Reads accept either cookie or JWT; writes
	// require an operator session so a compromised device cannot rewrite
	// its own params.
	hub := ws.NewHubWithMetrics(m)
	rpcCaller := ws.NewRPCCallerWithMetrics(m)

	configHandler := api.NewConfigHandler(queries, hub, rpcCaller)
	v1ConfigRead := e.Group("/v1", sessionOrJWT)
	configHandler.RegisterReadRoutes(v1ConfigRead)
	v1ConfigWrite := e.Group("/v1", sessionOnly)
	configHandler.RegisterMutationRoutes(v1ConfigWrite)

	// Retention and other operator settings. Same split as config params:
	// GETs on sessionOrJWT so the dashboard and devices both work; PUTs on
	// sessionOnly because only operators should change retention policy.
	settingsHandler := api.NewSettingsHandler(settingsStore, cfg.RetentionDays)
	settingsHandler.RegisterReadRoutes(v1ConfigRead)
	settingsHandler.RegisterMutationRoutes(v1ConfigWrite)

	// Per-device trip stats live at /v1/devices/:dongle_id/stats, so they
	// accept either a session cookie or a device JWT via the shared read group.
	tripHandler.RegisterStatsRoute(v1ConfigRead)

	// Live device status panel feeds the web UI; it accepts either a session
	// cookie (browser dashboard) or a device JWT (CLI/ad-hoc) via the shared
	// read group.
	liveHandler := api.NewDeviceLiveHandler(hub, rpcCaller)
	liveHandler.RegisterRoutes(v1ConfigRead)

	// Events "Moments" listing: paginated, filterable events per device.
	// Mounted on the shared read group so the dashboard (session cookie) and
	// ad-hoc device JWT callers both work.
	eventsHandler := api.NewEventsHandler(queries)
	eventsHandler.RegisterRoutes(v1ConfigRead)

	// Storage usage (disk accounting) endpoint. The walk is memoized in the
	// storage package so repeated polling stays cheap. Dashboard-facing.
	v1Storage := e.Group("/v1", sessionOrJWT)
	storageHandler := api.NewStorageHandler(store)
	storageHandler.RegisterRoutes(v1Storage)

	// Upload queue inspection and cancellation. The GET endpoint accepts
	// either a UI session cookie or a device JWT so either side can read it;
	// POST is session-only because only the operator should be cancelling
	// the device's own uploads. Uses the same sessionOrJWT/sessionOnly
	// middleware chain as the other dashboard endpoints.
	uploadQueueHandler := api.NewUploadQueueHandler(hub, rpcCaller)
	uploadQueueHandler.RegisterListRoute(v1ConfigRead)
	if cfg.UIAuthEnabled() {
		uploadQueueHandler.RegisterCancelRoute(v1ConfigWrite)
	}

	// Snapshot endpoint accepts either a session cookie (operator from the
	// web UI) or a device JWT, so it rides the shared read group.
	snapshotHandler := api.NewSnapshotHandler(hub, rpcCaller)
	snapshotHandler.RegisterRoutes(v1ConfigRead)

	// WebSocket for device communication.
	wsHandler := ws.NewHandler(hub, queries, nil, rpcCaller)
	wsHandler.RegisterRoutes(e)

	// Event detector background worker. Opt-out via EVENT_DETECTOR_ENABLED=false
	// (or "0"); any other value enables it. Runs in its own goroutine with the
	// server context so shutdown cancels the poll loop cleanly.
	if envBool("EVENT_DETECTOR_ENABLED", true) {
		detector := worker.NewEventDetector(
			queries,
			store,
			30*time.Second,
			worker.LoadThresholdsFromEnv(),
		)
		go detector.Run(context.Background())
		log.Printf("event detector worker started (thresholds: brake=%.2f m/s^2, min-sec=%.2f)",
			detector.Thresholds.HardBrakeMps2, detector.Thresholds.HardBrakeMinDurationSec)
	} else {
		log.Printf("event detector worker disabled via EVENT_DETECTOR_ENABLED")
	}

	// Background trip aggregator. Defaults on; set TRIP_AGGREGATOR_ENABLED=0
	// (or false/no/off) to skip it, e.g. in constrained test environments.
	if envBool("TRIP_AGGREGATOR_ENABLED", true) {
		aggregator := worker.NewTripAggregator(queries, geocode.NewClient("", ""))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go aggregator.Run(ctx)
		log.Printf("trip aggregator started (poll=%s, finalized_after=%s)",
			aggregator.PollInterval, aggregator.FinalizedAfter)
	} else {
		log.Printf("trip aggregator disabled via TRIP_AGGREGATOR_ENABLED")
	}

	// Cleanup worker: deletes non-preserved routes older than the
	// configured retention window. CLEANUP_ENABLED defaults to true.
	// DELETE_DRY_RUN defaults to true so first-time operators see what
	// would happen before enabling real deletion.
	if envBool("CLEANUP_ENABLED", true) {
		cleanup := &worker.CleanupWorker{
			Queries:          queries,
			Storage:          store,
			Settings:         settingsStore,
			Interval:         worker.DefaultCleanupInterval,
			EnvRetentionDays: cfg.RetentionDays,
			DryRun:           envBool("DELETE_DRY_RUN", true),
		}
		go cleanup.Run(context.Background())
	} else {
		log.Printf("cleanup worker: disabled via CLEANUP_ENABLED=false")
	}

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

// envBool parses a boolean environment variable. Missing or unparseable
// values fall back to defaultValue. Accepted truthy values: "true", "1",
// "yes", "on" (case-insensitive); accepted falsy: "false", "0", "no", "off".
func envBool(name string, defaultValue bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return defaultValue
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		log.Printf("warning: %s=%q is not a valid boolean; using default %v", name, v, defaultValue)
		return defaultValue
	}
}
