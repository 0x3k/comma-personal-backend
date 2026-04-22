.PHONY: help install install-backend install-frontend \
       dev dev-backend dev-frontend \
       build build-backend build-frontend \
       test test-backend test-frontend \
       lint lint-backend lint-frontend type-check \
       sqlc db-migrate clean \
       db-up db-down db-logs \
       prod-up prod-down prod-build prod-logs

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Install
# ---------------------------------------------------------------------------

install: install-backend install-frontend ## Install all dependencies

install-backend: ## Install Go dependencies + air (hot reload)
	go mod download
	@command -v air >/dev/null 2>&1 || { echo "Installing air for hot reload..."; go install github.com/air-verse/air@latest; }

install-frontend: ## Install frontend dependencies
	pnpm install --dir web

# ---------------------------------------------------------------------------
# Development (hot reload)
# ---------------------------------------------------------------------------

dev: ## Run backend (hot reload) + frontend concurrently
	@command -v air >/dev/null 2>&1 || { echo "air not found. Run 'make install' first."; exit 1; }
	@trap 'kill 0' EXIT; \
		air & \
		pnpm --dir web dev & \
		wait

dev-backend: ## Run backend only with hot reload
	@command -v air >/dev/null 2>&1 || { echo "air not found. Run 'make install' first."; exit 1; }
	air

dev-frontend: ## Run frontend dev server only
	pnpm --dir web dev

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

build: build-backend build-frontend ## Build everything for production

build-backend: ## Build Go binary
	go build -o server ./cmd/server

build-frontend: ## Build frontend for production
	pnpm --dir web build

# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------

test: test-backend test-frontend ## Run all tests

test-backend: ## Run Go tests
	go test ./...

test-frontend: ## Run frontend tests
	pnpm --dir web test

# ---------------------------------------------------------------------------
# Lint / Type-check
# ---------------------------------------------------------------------------

lint: lint-backend lint-frontend ## Run all linters

lint-backend: ## Run Go linters (go vet + golangci-lint if available)
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || true

lint-frontend: ## Run frontend linter
	pnpm --dir web lint

type-check: ## Run TypeScript type checking
	pnpm --dir web type-check

# ---------------------------------------------------------------------------
# Code generation / Database
# ---------------------------------------------------------------------------

sqlc: ## Regenerate sqlc database code
	sqlc generate

db-migrate: ## Run database migrations (requires golang-migrate)
	@command -v migrate >/dev/null 2>&1 || { echo "golang-migrate not found. Install: brew install golang-migrate"; exit 1; }
	migrate -path sql/migrations -database "$$DATABASE_URL" up

# ---------------------------------------------------------------------------
# Docker: dev database
# ---------------------------------------------------------------------------

db-up: ## Start Postgres+PostGIS in Docker (dev)
	docker compose up -d postgres

db-down: ## Stop Postgres container
	docker compose down

db-logs: ## Tail Postgres logs
	docker compose logs -f postgres

# ---------------------------------------------------------------------------
# Docker: prod (full stack in containers)
# ---------------------------------------------------------------------------

prod-build: ## Build backend + frontend images
	docker compose --profile prod build

prod-up: ## Start full stack (postgres + backend + frontend)
	docker compose --profile prod up -d

prod-down: ## Stop full stack
	docker compose --profile prod down

prod-logs: ## Tail logs from all prod services
	docker compose --profile prod logs -f

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------

clean: ## Remove build artifacts
	rm -f server
	rm -rf web/.next web/out
