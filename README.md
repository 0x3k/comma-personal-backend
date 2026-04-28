# comma-personal-backend

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Next.js](https://img.shields.io/badge/Next.js-15-000000?logo=nextdotjs&logoColor=white)](https://nextjs.org)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-PostGIS-4169E1?logo=postgresql&logoColor=white)](https://www.postgresql.org)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

> Self-hosted backend for [comma.ai](https://comma.ai) devices running [openpilot](https://github.com/commaai/openpilot) or [sunnypilot](https://github.com/sunnypilot/sunnypilot). Collects dashcam video, driving logs, GPS tracks, and device telemetry -- then lets you review everything through a built-in web UI.

**No account required. No data leaves your network.**

## What it does

- **Drop-in API replacement** -- mirrors the official comma backend endpoints (`/v2/pilotauth/`, `/v1.4/upload_url/`, `/v1/route/`, `/v1.1/devices/`, etc.) so openpilot and sunnypilot connect without any client-side patches
- **Video ingestion and transcoding** -- receives HEVC uploads from three cameras (road, driver, wide) and transcodes them to HLS for browser playback, with automatic fallback from container copy to libx264 re-encoding
- **Live device communication** -- persistent WebSocket channel per device for real-time JSON-RPC: push config changes, request file uploads and snapshots, set nav destinations, query network/SIM/thermal status, inspect and cancel the device upload queue
- **Device configuration** -- read and write device parameters through the API or the web UI, with changes pushed to the device in real time over WebSocket
- **GPS track storage** -- stores route geometry as PostGIS LineStrings for spatial queries and map display
- **Trip aggregation** -- background worker computes per-route distance, duration, and speed stats from the GPS geometry; reverse-geocodes start/end addresses via Nominatim
- **Event detection** -- background worker parses qlogs and extracts "moments" (hard brakes, disengagements, FCW alerts, warnings) with tunable thresholds
- **Log signal overlay** -- the video player renders a canvas timeline of speed, steering, and engagement state pulled from parsed qlogs, with click-to-seek
- **Web dashboard** -- home page with lifetime stats and recent drives, route browser with multi-camera HLS playback, Leaflet/OpenStreetMap GPS tracks, virtualized log viewer, moments/events listing, upload-queue monitor, device control center, and storage/retention settings
- **Shareable read-only links** -- signed, time-limited tokens grant unauthenticated access to a single route's video and map with no write access and no leakage of other routes
- **Retention and cleanup** -- per-route preserve flag, configurable retention window, and a cleanup worker that deletes old non-preserved routes (dry-run by default for safety)
- **Observability** -- Prometheus `/metrics` endpoint for request rate and latency, upload throughput, transcode timing, RPC latency, connected device count, and worker health, plus an importable Grafana dashboard

## Architecture

```
comma device                              this server
  athenad  ---- WebSocket (JSON-RPC) ----> ws/hub
  uploader ---- PUT /upload/... ---------> storage layer --> FFmpeg transcoder
                                           PostgreSQL + PostGIS
                                                |
                                           Next.js web UI
```

**Backend** (Go): Echo HTTP framework, pgx + sqlc for type-safe PostgreSQL access, gorilla/websocket for persistent device connections, FFmpeg for HEVC-to-HLS transcoding.

**Frontend** (TypeScript): Next.js + React, hls.js for multi-camera HLS playback, Leaflet + OpenStreetMap for GPS track maps, TanStack Table + Virtual for virtualized log viewing.

```
cmd/server/           -- entrypoint
internal/
  api/                -- HTTP handlers (device, route, upload, config, pilotauth, signals, stats, events, live, snapshot, upload-queue, share, session login, storage, settings)
  api/middleware/     -- JWT auth + session/cookie middleware (SessionRequired, SessionOrJWT)
  cereal/             -- cereal capnp log parser + driving-signal extractor
  config/             -- environment/config loader
  db/                 -- sqlc-generated queries
  geocode/            -- Nominatim reverse-geocoding client
  metrics/            -- Prometheus collectors
  sessioncookie/      -- signed session cookie HMAC helpers
  settings/           -- runtime settings store (retention, etc.)
  share/              -- signed share-link token helpers
  storage/            -- filesystem storage layer
  worker/             -- background jobs (HEVC->HLS transcode, cleanup, trip aggregator, event detector)
  ws/                 -- WebSocket hub, JSON-RPC client/server, device RPC calls
sql/
  migrations/         -- PostgreSQL migrations
  queries/            -- sqlc query files
web/
  src/app/            -- Next.js pages (dashboard home, routes, devices, moments, share, login, settings)
  src/components/     -- video player + signal timeline, map, log viewer, snapshot button, share button, device status panel, UI primitives
docs/
  grafana-dashboard.json  -- importable Grafana dashboard
```

## Prerequisites

- Go 1.22+
- Node.js 18+ and pnpm
- PostgreSQL 14+ with PostGIS
- FFmpeg (for video transcoding)

## Setup

**1. Create the database**

```bash
createdb comma
psql comma -c "CREATE EXTENSION IF NOT EXISTS postgis;"
```

**2. Run migrations**

Apply every migration in order:

```bash
for m in sql/migrations/*.up.sql; do psql comma < "$m"; done
```

**3. Configure environment**

Copy the example env file and fill in your values:

```bash
cp .env.example .env
# Edit .env with your DATABASE_URL
```

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | yes | -- | PostgreSQL connection string |
| `STORAGE_PATH` | no | `./data` | Directory for uploaded video/log files |
| `PORT` | no | `8080` | API server listen port |
| `ALLOWED_SERIALS` | no | -- | Comma-separated allowlist of device serials permitted to register (all allowed if unset). The dongle ID is assigned server-side, so restriction is by hardware serial. |
| `CORS_ALLOWED_ORIGINS` | no | -- | Comma-separated list of origins permitted by the CORS middleware. Required when the frontend is served from a different origin than the API (e.g. the docker prod stack puts the frontend on `:80` and the API on `:7070`). Cannot be `*` -- the API uses cookie-based session auth so credentialed CORS needs explicit origins. Leave unset for same-origin-only deployments. |
| `SESSION_SECRET` | no | -- | Required to enable the web UI login. Used as the HMAC key for signed session cookies. If unset, UI auth is disabled (a warning is logged) -- device auth still works. |
| `ADMIN_USERNAME` | no | -- | When both ADMIN_USERNAME and ADMIN_PASSWORD are set, the server bootstraps (or updates) this user row in `ui_users` on startup so you can log into the dashboard with env-configured credentials. |
| `ADMIN_PASSWORD` | no | -- | Plaintext admin password; stored hashed with bcrypt (cost 12). See ADMIN_USERNAME. |
| `RETENTION_DAYS` | no | `0` | Default retention window (in days) for non-preserved routes before the cleanup worker deletes them; `0` means never delete. Seeds the `retention_days` row in the `settings` table on first boot; overridable at runtime via `PUT /v1/settings/retention`. |
| `CLEANUP_ENABLED` | no | `true` | When `true`, runs the background cleanup worker that deletes non-preserved routes older than the effective retention window. Set to `false` to disable the worker (e.g. in replica deployments). |
| `DELETE_DRY_RUN` | no | `true` | When `true` (the default, for safety), the cleanup worker logs what it would delete but does not touch the filesystem or the database. Set to `false` to enable real deletion. |

**4. Start the backend**

```bash
go run ./cmd/server
```

**5. Start the frontend**

```bash
pnpm install --dir web
pnpm --dir web dev
```

The API server runs on `:8080` and the frontend dev server on `:3000` by default.

## Optional services

**ALPR (license plate recognition)** is opt-in. It runs as a separate
Docker container (`comma-alpr`) gated by the `alpr` Compose profile, so
bare `docker compose up` and `make prod-up` do not start it. Bring it up
with `make alpr-up` only if you accept the privacy and legal trade-offs
of recording plate text. See [docs/ALPR.md](docs/ALPR.md) for the engine
sidecar setup, GPU activation, and pin policy.

## Pointing your device at this server

Set two environment variables on your comma device before openpilot starts:

```bash
export API_HOST="https://your-server.example.com"
export ATHENA_HOST="wss://your-server.example.com"
```

See [docs/DEVICE-SETUP.md](docs/DEVICE-SETUP.md) for the full walkthrough -- authentication flow, upload mechanics, WebSocket path rewriting, and troubleshooting.

## Docker quickstart

For a containerized stack -- Postgres + PostGIS, the Go backend, and the Next.js frontend all together -- with no local Go/Node toolchain required beyond Docker:

```bash
cp .env.example .env
# Edit .env if you want to override ports or set SESSION_SECRET / admin creds.
make prod-up
```

Service URLs with the default `.env`:

| Service | URL | Container port |
|---------|-----|----------------|
| Frontend | http://localhost | `:3000` |
| Backend  | http://localhost:7070 | `:8080` |
| Postgres | `localhost:5432` | `:5432` |

Overridable via `.env`: `BACKEND_PORT`, `FRONTEND_PORT`, `NEXT_PUBLIC_API_URL`, `CORS_ALLOWED_ORIGINS`, plus every backend env var from the table above.

Two cross-cutting things to know about the Docker stack:

- **`NEXT_PUBLIC_API_URL` is baked into the frontend image at build time** (Next.js requirement). If you change `BACKEND_PORT`, you also need to update `NEXT_PUBLIC_API_URL` to the matching browser-reachable URL and rebuild: `make prod-build && make prod-up`. The variable must be reachable from the user's **browser**, not from inside the frontend container.
- **CORS is required** because the frontend (`:80`) and backend (`:7070`) are on different origins. The compose file defaults `CORS_ALLOWED_ORIGINS` to `http://localhost`; override in `.env` if your frontend lives elsewhere. `*` is rejected because the API uses cookie-based session auth.
- **Avoid host port `7000` on macOS.** macOS's AirPlay Receiver binds `*:7000` and silently intercepts all traffic on that port before Docker sees it (you'll get `Server: AirTunes/...` responses). The default `BACKEND_PORT` is `7070` for this reason; if you change it, steer clear of `5000` and `7000`.

Other targets: `make prod-down` (stop), `make prod-build` (rebuild images), `make prod-logs` (tail all services).

On Apple Silicon, the stack uses [`imresamu/postgis`](https://hub.docker.com/r/imresamu/postgis) (multi-arch) instead of the official `postgis/postgis` (amd64-only), so Postgres runs natively without Rosetta emulation.

## Production deployment

The local dev setup and the Docker quickstart above are fine for testing or a single-machine deployment behind your home network. For an always-on server that receives uploads from your car over the public internet, see [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) -- covers TLS with Caddy/nginx, systemd services, storage planning, and database backups.

## API

Endpoints are grouped by auth mode:
- **Device JWT** -- issued by `/v2/pilotauth/`, required for device-facing endpoints
- **Session cookie** -- issued by `/v1/session/login`, required for dashboard mutations
- **Session-or-JWT** -- either works, used by read endpoints shared between the UI and the device
- **Public** -- no auth (health, session login, share-link consumption, `/metrics`)

### Authentication
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST | `/v2/pilotauth/` | public | Device registration, returns a signed JWT |
| POST | `/v1/session/login` | public | Dashboard login (username + password). Sets an HttpOnly session cookie. Enabled only when `SESSION_SECRET` is set. Rate limited to 5 attempts / 15 min per IP. |
| POST | `/v1/session/logout` | session | Dashboard logout. Clears the session cookie. |

### Devices
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/v1/devices` | session-or-JWT | List registered devices |
| GET | `/v1.1/devices/:dongle_id/` | JWT | Device info |
| GET | `/v1/devices/:dongle_id/params` | session-or-JWT | List device config params |
| PUT | `/v1/devices/:dongle_id/params/:key` | session | Set a config param (pushed via WebSocket if connected) |
| DELETE | `/v1/devices/:dongle_id/params/:key` | session | Delete a config param |
| GET | `/v1/devices/:dongle_id/live` | session-or-JWT | Live status (network, metered, SIM, free disk, thermal) with a 5-minute cache when the device is offline |
| GET | `/v1/devices/:dongle_id/stats` | session-or-JWT | Lifetime totals + recent trips (`?limit=` `?offset=`) |
| GET | `/v1/devices/:dongle_id/events` | session-or-JWT | Detected events/moments (`?type=` `?severity=` `?limit=` `?offset=`) |
| POST | `/v1/devices/:dongle_id/snapshot` | session-or-JWT | Request an on-demand JPEG snapshot from both cameras (rate limited to 1 per 5s per device) |
| GET | `/v1/devices/:dongle_id/upload-queue` | session-or-JWT | List the device's current upload queue |
| POST | `/v1/devices/:dongle_id/upload-queue/cancel` | session | Cancel queued uploads by ID |

### Routes and uploads
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/v1/route/:dongle_id` | session-or-JWT | List routes (paginated, `?limit=` `?offset=`) |
| GET | `/v1/route/:dongle_id/:route_name` | session-or-JWT | Route detail with full segment list |
| GET | `/v1/routes/:dongle_id/:route_name/signals` | JWT | Parsed log signals (speed, steering, engagement, alerts) for the signal timeline; results are cached to `signals.json` next to each qlog |
| GET | `/v1/routes/:dongle_id/:route_name/trip` | JWT | Aggregated trip row for the route (404 if not yet aggregated) |
| GET | `/v1/routes/:dongle_id/:route_name/export.gpx` | session-or-JWT | Download the GPS track as GPX |
| GET | `/v1/routes/:dongle_id/:route_name/export.mp4` | session-or-JWT | Download a concatenated MP4 of the route's HLS video |
| POST | `/v1/routes/:dongle_id/:route_name/share` | session | Create a signed, time-limited public share link (body: `{"expires_in_hours": int (default 72, max 720)}`; 501 if `SESSION_SECRET` is unset) |
| GET | `/v1.4/:dongle_id/upload_url/` | JWT | Get self-hosted upload URL for a segment file |
| PUT | `/upload/:dongle_id/*` | JWT | Upload a segment file (up to 100 MB) |

### Share (public, token-gated)
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/v1/share/:token` | token | Route metadata, segment list, geometry, and media base URL for a shared route (410 Gone on expiry, 401 on tampered token) |
| GET | `/v1/share/:token/segments/:seg/:file` | token | Stream a whitelisted HLS file (`qcamera.ts`, `index.m3u8`) scoped to the share token's route |

### Storage and settings
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/v1/storage/usage` | session-or-JWT | Disk usage breakdown: total, per-device, filesystem available |
| GET | `/v1/settings/retention` | session-or-JWT | Read current retention window in days (`0` means never delete) |
| PUT | `/v1/settings/retention` | session | Update retention window (body: `{"retention_days": int}`, must be `>= 0`) |

### WebSocket
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/ws/v2/:dongle_id` | JWT | Persistent device channel (JSON-RPC over WebSocket) |

Supported RPC methods (server-initiated calls to the device): `uploadFileToUrl`, `uploadFilesToUrls`, `getNetworkType`, `getNetworkMetered`, `getNetworks`, `getSimInfo`, `getMessage`, `setNavDestination`, `setParam`, `deleteParam`, `listDataDirectory`, `takeSnapshot`, `listUploadQueue`, `cancelUpload`, `setRouteViewed`, `getVersion`, `getPublicKey`, `getSshAuthorizedKeys`, `getGithubUsername`.

### Health and observability
| Method | Path | Auth | Description |
|--------|------|------|-------------|
| GET | `/health` | public | Returns `{"status": "ok"}` |
| GET | `/metrics` | public | Prometheus metrics (HTTP, upload, transcode, RPC, worker, WebSocket). See `docs/DEPLOYMENT.md` for the Grafana import flow. |

## Data layout on disk

```
$STORAGE_PATH/
  <dongle_id>/
    <route_name>/
      <segment_number>/
        fcamera.hevc          # road camera (raw HEVC)
        ecamera.hevc          # wide camera
        dcamera.hevc          # driver camera
        qcamera.ts            # low-res preview
        rlog                  # full log
        qlog                  # compressed log
        fcamera/index.m3u8    # HLS (generated by transcoder)
        ecamera/index.m3u8
        dcamera/index.m3u8
```

## Development

```bash
go test ./...              # backend tests
pnpm --dir web test        # frontend tests
go vet ./...               # lint
pnpm --dir web lint        # frontend lint
pnpm --dir web type-check  # TypeScript type check
go build -o server ./cmd/server  # production binary
```
