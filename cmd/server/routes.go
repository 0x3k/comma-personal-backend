package main

import (
	"context"
	"log"
	"net/http"
	"strings"

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
	// Global middleware. Order matters:
	//   logger -> CORS -> rate-limiter -> body-limit -> metrics
	// Logger runs first so every request is visible in the access log,
	// including ones rejected by the rate limiter (429) or body limit
	// (413). CORS runs next so OPTIONS preflights short-circuit (204)
	// before the rate limiter counts them and before metrics observe
	// them. Metrics observe post-admission traffic only.
	e.Use(echomw.RequestLoggerWithConfig(echomw.RequestLoggerConfig{
		LogStatus:   true,
		LogMethod:   true,
		LogURI:      true,
		LogRemoteIP: true,
		LogLatency:  true,
		LogError:    true,
		HandleError: true,
		LogValuesFunc: func(c echo.Context, v echomw.RequestLoggerValues) error {
			if v.Error != nil {
				log.Printf("%s %s %d %s remote=%s err=%v",
					v.Method, v.URI, v.Status, v.Latency, v.RemoteIP, v.Error)
			} else {
				log.Printf("%s %s %d %s remote=%s",
					v.Method, v.URI, v.Status, v.Latency, v.RemoteIP)
			}
			return nil
		},
	}))
	if len(d.cfg.AllowedOrigins) > 0 {
		e.Use(echomw.CORSWithConfig(echomw.CORSConfig{
			AllowOrigins: d.cfg.AllowedOrigins,
			AllowMethods: []string{
				http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch,
				http.MethodPost, http.MethodDelete, http.MethodOptions,
			},
			AllowHeaders: []string{
				echo.HeaderOrigin, echo.HeaderContentType, echo.HeaderAccept,
				echo.HeaderAuthorization,
			},
			AllowCredentials: true,
		}))
	}
	// Global rate limiter at 100 req/s. Excludes /storage/ because HLS
	// streaming is naturally bursty (one playlist + many .ts chunks per
	// segment per camera) and would otherwise starve every other endpoint
	// the dashboard hits during playback. /storage/ is still gated by
	// sessionOrJWT + checkDongleAccess so the exclusion is auth-only,
	// not authorization-only.
	e.Use(echomw.RateLimiterWithConfig(echomw.RateLimiterConfig{
		Skipper: func(c echo.Context) bool {
			return strings.HasPrefix(c.Request().URL.Path, "/storage/")
		},
		Store: echomw.NewRateLimiterMemoryStore(100),
	}))
	// Global 1M body limit for the API surface. /upload/* sets its own
	// (larger) limit later; without this skipper, the global limit fires
	// first, closes the connection mid-stream, and the device sees a TLS
	// EOF instead of a 413.
	e.Use(echomw.BodyLimitWithConfig(echomw.BodyLimitConfig{
		Skipper: func(c echo.Context) bool {
			return strings.HasPrefix(c.Request().URL.Path, "/upload/")
		},
		Limit: "1M",
	}))
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
	routeHandler.RegisterAnnotationReadRoutes(v1Routes)
	api.NewExportHandler(d.queries, d.store).RegisterRoutes(v1Routes)
	api.NewSignalsHandler(d.queries, d.store).RegisterRoutes(v1Routes)
	api.NewThumbnailHandler(d.store).RegisterRoutes(v1Routes)
	api.NewTurnsHandler(d.queries).RegisterRoutes(v1Routes)

	// Authenticated segment file streaming. Mounted at the top-level path
	// /storage/... (not under /v1) because the frontend was already
	// constructing those URLs in MultiCameraPlayer; the handler mirrors
	// the public /v1/share/:token/... routes but enforces sessionOrJWT +
	// checkDongleAccess in place of the signed-token check.
	api.NewStorageFilesHandler(d.store).RegisterRoutes(e, sessionOrJWT)

	// Share link creation is a mutation (mints a signed token) so it rides
	// the session-only group. The public /v1/share/:token endpoints are
	// mounted directly on the top-level Echo instance below -- they must
	// not be gated on the session middleware because the whole point is
	// to let an unauthenticated viewer see a single shared route.
	shareHandler := api.NewShareHandler(d.queries, d.store, d.cfg.SessionSecret)
	v1RoutesSessionOnly := e.Group("/v1/routes", sessionOnly)
	shareHandler.RegisterCreateRoute(v1RoutesSessionOnly)
	shareHandler.RegisterPublicRoutes(e)

	// Route annotation writes (note, starred, tags) ride the session-only
	// group because device JWTs have no business rewriting the operator's
	// user annotations.
	routeHandler.RegisterAnnotationMutationRoutes(v1RoutesSessionOnly)

	// Device-level tag autocomplete lives under /v1/devices/:dongle_id/tags
	// alongside the other device-scoped reads.
	v1Devices := e.Group("/v1/devices", sessionOrJWT)
	routeHandler.RegisterDeviceTagsRoute(v1Devices)

	// On-demand pull of full-resolution route data (full HEVC + full rlog).
	// POST queues an uploadFilesToUrls RPC against the device; GET returns
	// the request row + per-segment progress derived from the segments
	// upload flags so the polling UI does not need a second endpoint.
	api.NewRouteDataRequestHandler(
		d.queries,
		api.NewHubDispatcher(d.hub, d.rpcCaller),
		d.sessionSecret,
	).WithPublicBaseURL(d.cfg.PublicBaseURL).RegisterRoutes(v1Route)

	// Trip stats: per-device lifetime totals + recent trip list on /v1, and
	// per-route aggregated trip detail on /v1/routes.
	tripHandler := api.NewTripHandler(d.queries)
	tripHandler.RegisterTripRoute(v1Routes)

	// Upload URL and file upload -- device-facing, JWT only.
	v14 := e.Group("/v1.4", auth)
	uploadHandler := api.NewUploadHandlerWithMetrics(d.store, d.queries, d.metrics).WithPublicBaseURL(d.cfg.PublicBaseURL)
	v14.GET("/:dongle_id/upload_url/", uploadHandler.GetUploadURL)
	v14.GET("/:dongle_id/upload_url", uploadHandler.GetUploadURL)

	// /upload accepts either a valid device JWT (regular auto-upload path,
	// where the device already has a token to attach) or a backend-signed URL
	// (the on-demand "Get full quality" path -- athenad PUTs the URL it was
	// given via RPC and has nothing to sign with). uploadAuth checks the
	// signature first and only falls through to JWT validation when no
	// signature is present, so a malformed signature fails closed instead of
	// silently downgrading to JWT.
	uploadAuth := uploadAuthOrSignature(d.sessionSecret, auth)
	uploadGroup := e.Group("/upload", uploadAuth, echomw.BodyLimit("100M"))
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

	// ALPR configuration. Routes always register (no env-gating) so the
	// frontend can flip the master flag without a server restart. Reads
	// ride sessionOrJWT because devices can ask "is ALPR enabled?";
	// mutations are session-only because a compromised device must never
	// be able to enable plate recording on itself.
	alprSettingsHandler := api.NewALPRSettingsHandler(d.settings, d.cfg.ALPR)
	alprSettingsHandler.RegisterReadRoutes(v1ConfigRead)
	alprSettingsHandler.RegisterMutationRoutes(v1ConfigWrite)

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

	// Sunnylink params: operator-facing REST surface that proxies the
	// sunnylink-only WS RPC methods (toggleLogUpload, getParams, saveParams).
	// Reads ride the shared read group; writes are session-only because a
	// compromised device should not be able to flip its own log-upload
	// toggle from the server side.
	sunnylinkParamsHandler := api.NewSunnylinkParamsHandler(d.queries, d.hub, d.rpcCaller)
	sunnylinkParamsHandler.RegisterReadRoutes(v1ConfigRead)
	if d.cfg.UIAuthEnabled() {
		sunnylinkParamsHandler.RegisterMutationRoutes(v1ConfigWrite)
	}

	// Sunnylink device-facing endpoints that the device polls every few
	// seconds: roles (sponsor tier) and users (pairing). Bearer JWT auth
	// happens inside the handler against the sunnylink_public_key column,
	// so these are mounted on the bare Echo instance with no group middleware.
	api.NewSunnylinkStateHandler(d.queries).RegisterRoutes(e)

	// Sunnylink resume-queue ack: device hits this immediately after
	// reconnecting its WS so we acknowledge it can drain its uploads.
	// Public, no auth (device passes a JWT but we don't depend on it).
	api.NewSunnylinkResumeHandler().RegisterRoutes(e)

	// Device pairing landing page. The QR on the device's pairing dialog
	// points here; operators scan it on their phone and see a confirmation
	// page. Pairing itself is implicit -- see deviceResponse.IsPaired.
	api.NewPairingHandler(d.queries).RegisterRoutes(e)

	// Sentry envelope relay. Devices repoint their existing
	// sentry_sdk.init(...) DSN at this backend by configuring the DSN
	// hostname; the SDK posts envelopes to /api/<project_id>/envelope/.
	// Authless by design (matches the Sentry relay protocol).
	api.NewSentryRelay(d.queries).RegisterRoutes(e)

	// Crash dashboard reads. Mounted on the shared read group so the
	// operator dashboard (session cookie) and ad-hoc CLI callers (device
	// JWT) both work.
	api.NewCrashesHandler(d.queries).RegisterRoutes(v1ConfigRead)

	// WebSocket for device communication.
	//
	// The handlers map dispatches JSON-RPC requests the device pushes
	// unprompted. forwardLogs and storeStats are the two notifications
	// athenad/sunnylinkd send every ~10 seconds for log + telemetry uploads;
	// without server-side handlers the device retries forever and disk fills
	// up on the device side.
	wsHandlers := map[string]ws.MethodHandler{
		"forwardLogs": ws.MakeForwardLogsHandler(d.store, nil),
		"storeStats":  ws.MakeStoreStatsHandler(d.store, nil),
	}
	ws.NewHandler(d.hub, d.queries, wsHandlers, d.rpcCaller).RegisterRoutes(e)
}

// uploadAuthOrSignature returns middleware that authenticates an /upload PUT
// via either an HMAC-signed URL (sig+exp query params) or a device JWT.
// Signed URLs are how the on-demand "Get full quality" path tells athenad to
// upload files: athenad PUTs the URL it was handed via RPC and has no JWT
// to attach, so the URL itself carries authorisation. JWT remains the
// auto-upload path: the regular uploader pulls a URL via /v1.4/.../upload_url/
// and reflects the JWT it used there onto its PUT.
//
// uploadSecret may be empty -- in which case all signature attempts fail
// closed and the middleware degrades to JWT-only auth, matching legacy
// behaviour. A signature that is *present* but invalid (bad sig, expired,
// or path mismatch) is rejected with 401 instead of falling through to JWT;
// otherwise an attacker could probe with garbage signatures and silently
// retry as JWT.
func uploadAuthOrSignature(uploadSecret []byte, jwtMW echo.MiddlewareFunc) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		jwtChain := jwtMW(next)
		return func(c echo.Context) error {
			expParam := c.QueryParam("exp")
			sigParam := c.QueryParam("sig")
			if expParam != "" || sigParam != "" {
				if len(uploadSecret) == 0 {
					return c.JSON(http.StatusUnauthorized, map[string]any{
						"error": "upload signing is not configured",
						"code":  http.StatusUnauthorized,
					})
				}
				if !api.VerifyUploadSignature(uploadSecret, c.Request().URL.Path, expParam, sigParam) {
					return c.JSON(http.StatusUnauthorized, map[string]any{
						"error": "invalid or expired upload signature",
						"code":  http.StatusUnauthorized,
					})
				}
				c.Set(middleware.ContextKeyDongleID, c.Param("dongle_id"))
				return next(c)
			}
			return jwtChain(c)
		}
	}
}
