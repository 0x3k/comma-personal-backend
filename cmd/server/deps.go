package main

import (
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
	queries       *db.Queries
	store         *storage.Storage
	settings      *settings.Store
	metrics       *metrics.Metrics
	hub           *ws.Hub
	rpcCaller     *ws.RPCCaller
	sessionSecret []byte
}
