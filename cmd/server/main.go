package main

import (
	"log"
	"net/http"

	"github.com/joho/godotenv"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/config"
)

func main() {
	// Load .env file if present; ignore error if file does not exist.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load configuration: %v", err)
	}

	e := echo.New()

	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	log.Printf("starting server on :%s", cfg.Port)
	if err := e.Start(":" + cfg.Port); err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
}
