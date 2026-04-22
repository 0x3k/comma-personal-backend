package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"

	"comma-personal-backend/internal/api"
	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/metrics"
	"comma-personal-backend/internal/storage"
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

	// Authenticated API groups. Every request carries a per-device JWT
	// signed with the private key openpilot generated during pilotauth; the
	// middleware verifies it against the public_key stored for that device.
	auth := middleware.JWTAuthFromDB(queries)

	deviceHandler := api.NewDeviceHandler(queries)

	v11 := e.Group("/v1.1", auth)
	deviceHandler.RegisterRoutes(v11)

	// Dashboard listing of all registered devices. Unauthenticated because the
	// local web UI has no device JWT; see DeviceHandler.ListDevices.
	e.GET("/v1/devices", deviceHandler.ListDevices)

	// Route listing and detail.
	v1Route := e.Group("/v1/route", auth)
	routeHandler := api.NewRouteHandler(queries)
	routeHandler.RegisterRoutes(v1Route)

	// Plural /v1/routes/ path hosts route mutation and export endpoints so
	// they do not collide with /v1/route/:dongle_id.
	v1Routes := e.Group("/v1/routes", auth)
	routeHandler.RegisterPreservedRoute(v1Routes)
	exportHandler := api.NewExportHandler(queries)
	exportHandler.RegisterRoutes(v1Routes)

	// Upload URL and file upload.
	v14 := e.Group("/v1.4", auth)
	uploadHandler := api.NewUploadHandlerWithMetrics(store, queries, m)
	v14.GET("/:dongle_id/upload_url/", uploadHandler.GetUploadURL)
	v14.GET("/:dongle_id/upload_url", uploadHandler.GetUploadURL)

	uploadGroup := e.Group("/upload", auth, echomw.BodyLimit("100M"))
	uploadGroup.PUT("/:dongle_id/*", uploadHandler.UploadFile)

	// Device config parameters.
	hub := ws.NewHubWithMetrics(m)
	rpcCaller := ws.NewRPCCallerWithMetrics(m)

	v1Config := e.Group("/v1", auth)
	configHandler := api.NewConfigHandler(queries, hub, rpcCaller)
	configHandler.RegisterRoutes(v1Config)

	// Storage usage (disk accounting) endpoint. The walk is memoized in the
	// storage package so repeated polling stays cheap.
	v1Storage := e.Group("/v1", auth)
	storageHandler := api.NewStorageHandler(store)
	storageHandler.RegisterRoutes(v1Storage)

	// WebSocket for device communication.
	wsHandler := ws.NewHandler(hub, queries, nil, rpcCaller)
	wsHandler.RegisterRoutes(e)

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
