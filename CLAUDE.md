# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository.


## Project Overview

**Name**: comma-personal-backend
**Language**: Go, TypeScript
**Purpose**: Personal backend for collecting and reviewing dashcam videos, logs, location data, and trip information from comma.ai devices, compatible with the openpilot/sunnypilot backend API

## Build & Dev Commands

```bash
# Install dependencies
go mod download          # backend
pnpm install --dir web   # frontend

# Development
go run ./cmd/server                # backend (API server)
pnpm --dir web dev                 # frontend (Next.js dev server)

# Build
go build -o server ./cmd/server    # backend binary
pnpm --dir web build               # frontend production build

# Lint
go vet ./...                       # backend
golangci-lint run                  # backend (extended)
pnpm --dir web lint                # frontend

# Type check
pnpm --dir web type-check          # frontend (tsc --noEmit)

# Test
go test ./...                      # backend
pnpm --dir web test                # frontend
```

## Architecture

### Backend (Go)
- **Framework**: Echo (HTTP/2, middleware, route groups)
- **Database**: PostgreSQL + PostGIS (spatial queries for trip/map data)
- **DB layer**: pgx (driver) + sqlc (type-safe codegen from SQL)
- **Video processing**: FFmpeg for HEVC-to-HLS transcoding
- **Storage**: Local filesystem at `STORAGE_PATH`

```
cmd/server/          -- main entrypoint
internal/
  api/               -- HTTP handlers (Echo route groups)
  api/middleware/     -- auth, logging, error handling
  db/                -- sqlc-generated code + migrations
  storage/           -- filesystem storage layer
  worker/            -- background jobs (video transcoding, log parsing)
  models/            -- domain types (device, route, segment)
sql/
  migrations/        -- PostgreSQL migrations
  queries/           -- sqlc query files
```

### Frontend (TypeScript)
- **Framework**: React + Next.js
- **Video**: hls.js for HLS playback of dashcam streams
- **Maps**: Leaflet + OpenStreetMap (trip location display)
- **Data tables/logs**: TanStack Table + TanStack Virtual (virtual scrolling)

```
web/
  src/
    app/             -- Next.js app router pages
    components/      -- shared UI components
    components/video/ -- multi-camera video player
    components/map/   -- trip map with Leaflet
    components/logs/  -- log viewer with virtual scroll
    lib/             -- API client, types, utilities
```

### Data Model
- **Device**: identified by `dongle_id`
- **Route**: `dongle_id|YYYY-MM-DD--HH-MM-SS` (ignition to power-down)
- **Segment**: `route--N` (1-minute chunk), contains: `rlog`, `qlog`, `fcamera.hevc`, `ecamera.hevc`, `dcamera.hevc`, `qcamera.ts`
- Files on disk: `<STORAGE_PATH>/<dongle_id>/<route>/<segment>/`

### Reference Repos (client-side)
- **openpilot**: `../../openpilot/` -- upstream comma.ai client, reference for API endpoints and data formats
- **sunnypilot**: `../sunnypilot/` -- sunnypilot fork, reference for any additional API usage or extensions

These repos contain the device-side code (athenad, uploader, API client) that this backend must be compatible with. Consult them for endpoint contracts, auth flow, and file upload formats.

## Environment Variables

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `DATABASE_URL` | yes | `postgres://localhost:5432/comma` | PostgreSQL connection string |
| `STORAGE_PATH` | yes | `./data` | Local filesystem path for video/log files |
| `PORT` | no | `8080` | API server listen port |
| `COMMA_DONGLE_ID` | no | - | Restrict to specific device (if multi-device) |
| `ALLOWED_SERIALS` | no | - | Comma-separated allowlist of hardware serials permitted to register via pilotauth. Dongle IDs are assigned server-side, so restriction is keyed on the device's serial. If unset, all devices are allowed. |
| `SESSION_SECRET` | no | - | HMAC key for signed web UI session cookies. If unset, UI login endpoints are disabled (a warning is logged on startup). Device-facing JWT auth is unaffected. |
| `ADMIN_USERNAME` | no | - | Together with `ADMIN_PASSWORD`, bootstraps/updates a single admin row in `ui_users` on startup. Only used when `SESSION_SECRET` is set. |
| `ADMIN_PASSWORD` | no | - | Plaintext password for the bootstrap admin user. Hashed with bcrypt (cost 12) before storage. Only used when `SESSION_SECRET` is set. |

## Key Patterns

- **Comma API compatibility**: mirror official API paths (`/v1/devices/`, `/v1/route/`, `/v1.4/`, etc.) so openpilot/sunnypilot connect without client-side changes
- **Route/segment naming**: follows comma convention `dongle_id|YYYY-MM-DD--HH-MM-SS--N`
- **Database queries**: all queries go through sqlc-generated code, no raw SQL strings in handlers
- **Error responses**: consistent JSON envelope `{"error": "...", "code": ...}`
- **File storage layout**: `<STORAGE_PATH>/<dongle_id>/<route>/<segment>/<filename>`
- **Reference the client repos** (`../../openpilot/`, `../sunnypilot/`) when verifying endpoint contracts or data formats

## Code Conventions
