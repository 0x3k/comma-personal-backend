package main

import (
	"context"
	"log"
	"net/http"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"

	"comma-personal-backend/internal/api"
	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/metrics"
	"comma-personal-backend/internal/ws"
)

// setupRoutes wires every HTTP handler onto e. New route registrations
// belong here, grouped with their siblings -- NOT in main.go. This keeps
// the bootstrap minimal and lets parallel feature branches add handlers
// without always colliding on main.go.
func setupRoutes(e *echo.Echo, d *deps) {
	// Global middleware. Order matters: rate-limiter -> body-limit ->
	// metrics, so metrics observe post-admission traffic only.
	e.Use(echomw.RateLimiter(echomw.NewRateLimiterMemoryStore(20)))
	e.Use(echomw.BodyLimit("1M"))
	e.Use(metrics.EchoMiddleware(d.metrics))

	// Prometheus scrape endpoint. Intentionally unauthenticated per the
	// feature spec -- front with nginx if auth is needed.
	e.GET("/metrics", echo.WrapHandler(d.metrics.Handler()))

	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	// Device registration (unauthenticated).
	api.NewPilotAuthHandler(d.queries, d.cfg).RegisterRoutes(e)

	// Web UI authentication. Kept separate from the device-facing JWT auth
	// so operators without a SESSION_SECRET still get device uploads working.
	if d.cfg.UIAuthEnabled() {
		api.NewSessionHandler(d.queries, d.cfg.SessionSecret).RegisterRoutes(e)
		if err := api.BootstrapAdmin(context.Background(), d.queries, d.cfg.AdminUsername, d.cfg.AdminPassword); err != nil {
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
	auth := middleware.JWTAuthFromDB(d.queries)
	sessionOnly := middleware.SessionRequired(d.sessionSecret)
	sessionOrJWT := middleware.SessionOrJWT(d.sessionSecret, d.queries)

	deviceHandler := api.NewDeviceHandler(d.queries)

	// /v1.1/devices/:dongle_id is the device self-info endpoint called by
	// openpilot; it stays JWT-only.
	v11 := e.Group("/v1.1", auth)
	deviceHandler.RegisterRoutes(v11)

	// Dashboard listing of all registered devices. Dashboard-facing, so
	// gated on SessionOrJWT.
	e.GET("/v1/devices", deviceHandler.ListDevices, sessionOrJWT)

	// Route listing and detail (dashboard reads; devices may still call).
	v1Route := e.Group("/v1/route", sessionOrJWT)
	routeHandler := api.NewRouteHandler(d.queries)
	routeHandler.RegisterRoutes(v1Route)

	// Plural /v1/routes/ path hosts route mutation and export endpoints so
	// they do not collide with /v1/route/:dongle_id. These are dashboard
	// operations (preserve flag, export downloads), shared with device
	// JWTs for consistency.
	v1Routes := e.Group("/v1/routes", sessionOrJWT)
	routeHandler.RegisterPreservedRoute(v1Routes)
	api.NewExportHandler(d.queries, d.store).RegisterRoutes(v1Routes)
	api.NewSignalsHandler(d.queries, d.store).RegisterRoutes(v1Routes)

	// Share link creation is a mutation (mints a signed token) so it rides
	// the session-only group. The public /v1/share/:token endpoints are
	// mounted directly on the top-level Echo instance below -- they must
	// not be gated on the session middleware because the whole point is
	// to let an unauthenticated viewer see a single shared route.
	shareHandler := api.NewShareHandler(d.queries, d.store, d.cfg.SessionSecret)
	v1RoutesSessionOnly := e.Group("/v1/routes", sessionOnly)
	shareHandler.RegisterCreateRoute(v1RoutesSessionOnly)
	shareHandler.RegisterPublicRoutes(e)

	// Trip stats: per-device lifetime totals + recent trip list on /v1, and
	// per-route aggregated trip detail on /v1/routes.
	tripHandler := api.NewTripHandler(d.queries)
	tripHandler.RegisterTripRoute(v1Routes)

	// Upload URL and file upload -- device-facing, JWT only.
	v14 := e.Group("/v1.4", auth)
	uploadHandler := api.NewUploadHandlerWithMetrics(d.store, d.queries, d.metrics)
	v14.GET("/:dongle_id/upload_url/", uploadHandler.GetUploadURL)
	v14.GET("/:dongle_id/upload_url", uploadHandler.GetUploadURL)

	uploadGroup := e.Group("/upload", auth, echomw.BodyLimit("100M"))
	uploadGroup.PUT("/:dongle_id/*", uploadHandler.UploadFile)

	// Device config parameters. Reads accept either cookie or JWT; writes
	// require an operator session so a compromised device cannot rewrite
	// its own params.
	configHandler := api.NewConfigHandler(d.queries, d.hub, d.rpcCaller)
	v1ConfigRead := e.Group("/v1", sessionOrJWT)
	configHandler.RegisterReadRoutes(v1ConfigRead)
	v1ConfigWrite := e.Group("/v1", sessionOnly)
	configHandler.RegisterMutationRoutes(v1ConfigWrite)

	// Retention and other operator settings. Same split as config params:
	// GETs on sessionOrJWT so the dashboard and devices both work; PUTs on
	// sessionOnly because only operators should change retention policy.
	settingsHandler := api.NewSettingsHandler(d.settings, d.cfg.RetentionDays)
	settingsHandler.RegisterReadRoutes(v1ConfigRead)
	settingsHandler.RegisterMutationRoutes(v1ConfigWrite)

	// Per-device trip stats live at /v1/devices/:dongle_id/stats, so they
	// accept either a session cookie or a device JWT via the shared read group.
	tripHandler.RegisterStatsRoute(v1ConfigRead)

	// Live device status panel feeds the web UI; it accepts either a session
	// cookie (browser dashboard) or a device JWT (CLI/ad-hoc) via the shared
	// read group.
	api.NewDeviceLiveHandler(d.hub, d.rpcCaller).RegisterRoutes(v1ConfigRead)

	// Events "Moments" listing: paginated, filterable events per device.
	// Mounted on the shared read group so the dashboard (session cookie) and
	// ad-hoc device JWT callers both work.
	api.NewEventsHandler(d.queries).RegisterRoutes(v1ConfigRead)

	// Storage usage (disk accounting) endpoint. The walk is memoized in the
	// storage package so repeated polling stays cheap. Dashboard-facing.
	v1Storage := e.Group("/v1", sessionOrJWT)
	api.NewStorageHandler(d.store).RegisterRoutes(v1Storage)

	// Upload queue inspection and cancellation. The GET endpoint accepts
	// either a UI session cookie or a device JWT so either side can read it;
	// POST is session-only because only the operator should be cancelling
	// the device's own uploads.
	uploadQueueHandler := api.NewUploadQueueHandler(d.hub, d.rpcCaller)
	uploadQueueHandler.RegisterListRoute(v1ConfigRead)
	if d.cfg.UIAuthEnabled() {
		uploadQueueHandler.RegisterCancelRoute(v1ConfigWrite)
	}

	// Snapshot endpoint accepts either a session cookie (operator from the
	// web UI) or a device JWT, so it rides the shared read group.
	api.NewSnapshotHandler(d.hub, d.rpcCaller).RegisterRoutes(v1ConfigRead)

	// WebSocket for device communication.
	ws.NewHandler(d.hub, d.queries, nil, d.rpcCaller).RegisterRoutes(e)
}
