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

	e := echo.New()

	// Global rate limiter: 20 requests/second per IP.
	e.Use(echomw.RateLimiter(echomw.NewRateLimiterMemoryStore(20)))

	// Default body limit for JSON endpoints (1MB).
	e.Use(echomw.BodyLimit("1M"))

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

	v11 := e.Group("/v1.1", auth)
	deviceHandler := api.NewDeviceHandler(queries)
	deviceHandler.RegisterRoutes(v11)

	// Route listing and detail.
	v1Route := e.Group("/v1/route", auth)
	routeHandler := api.NewRouteHandler(queries)
	routeHandler.RegisterRoutes(v1Route)

	// Upload URL and file upload.
	v14 := e.Group("/v1.4", auth)
	uploadHandler := api.NewUploadHandler(store, queries)
	v14.GET("/:dongle_id/upload_url/", uploadHandler.GetUploadURL)
	v14.GET("/:dongle_id/upload_url", uploadHandler.GetUploadURL)

	uploadGroup := e.Group("/upload", auth, echomw.BodyLimit("100M"))
	uploadGroup.PUT("/:dongle_id/*", uploadHandler.UploadFile)

	// Device config parameters.
	hub := ws.NewHub()
	rpcCaller := ws.NewRPCCaller()

	v1Config := e.Group("/v1", auth)
	configHandler := api.NewConfigHandler(queries, hub, rpcCaller)
	configHandler.RegisterRoutes(v1Config)

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
