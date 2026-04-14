# comma-personal-backend

A self-hosted backend for [comma.ai](https://comma.ai) devices running [openpilot](https://github.com/commaai/openpilot) or [sunnypilot](https://github.com/sunnypilot/sunnypilot). Collects dashcam video, driving logs, GPS tracks, and device telemetry -- then lets you review everything through a built-in web UI.

Devices connect to this server instead of comma's cloud. No account required, no data leaves your network.

## What it does

- **Drop-in API replacement** -- mirrors the official comma backend endpoints (`/v2/pilotauth/`, `/v1.4/upload_url/`, `/v1/route/`, `/v1.1/devices/`, etc.) so openpilot and sunnypilot connect without any client-side patches
- **Video ingestion and transcoding** -- receives HEVC uploads from three cameras (road, driver, wide) and transcodes them to HLS for browser playback, with automatic fallback from container copy to libx264 re-encoding
- **Live device communication** -- persistent WebSocket channel per device for real-time JSON-RPC: push config changes, request file uploads, set nav destinations, query network/SIM status
- **Device configuration** -- read and write device parameters through the API or the web UI, with changes pushed to the device in real time over WebSocket
- **GPS track storage** -- stores route geometry as PostGIS LineStrings for spatial queries and map display
- **Web dashboard** -- browse routes, play back multi-camera video with hls.js, view GPS tracks on Leaflet/OpenStreetMap, inspect upload status per segment, and manage device settings

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
  api/                -- HTTP handlers (device, route, upload, config, pilotauth)
  api/middleware/     -- JWT auth
  db/                 -- sqlc-generated queries + migrations
  storage/            -- filesystem storage layer
  worker/             -- background video transcoding
  ws/                 -- WebSocket hub, JSON-RPC client/server
web/
  src/app/            -- Next.js pages (dashboard, routes, devices, settings)
  src/components/     -- video player, map, log viewer, UI primitives
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

```bash
psql comma < sql/migrations/001_init.up.sql
psql comma < sql/migrations/002_device_params.up.sql
```

**3. Configure environment**

Copy the example env file and fill in your values:

```bash
cp .env.example .env
# Edit .env with your DATABASE_URL and JWT_SECRET
```

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | yes | -- | PostgreSQL connection string |
| `JWT_SECRET` | yes | -- | Secret key for signing device auth tokens |
| `STORAGE_PATH` | no | `./data` | Directory for uploaded video/log files |
| `PORT` | no | `8080` | API server listen port |
| `ALLOWED_DONGLE_IDS` | no | -- | Comma-separated allowlist of dongle IDs permitted to register (all allowed if unset) |

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

## Pointing your device at this server

Set two environment variables on your comma device before openpilot starts:

```bash
export API_HOST="https://your-server.example.com"
export ATHENA_HOST="wss://your-server.example.com"
```

See [docs/DEVICE-SETUP.md](docs/DEVICE-SETUP.md) for the full walkthrough -- authentication flow, upload mechanics, WebSocket path rewriting, and troubleshooting.

## Production deployment

The local dev setup above is fine for testing. For an always-on server that receives uploads from your car, see [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) -- covers TLS with Caddy/nginx, systemd services, storage planning, and database backups.

## API

### Authentication
| Method | Path | Description |
|--------|------|-------------|
| POST | `/v2/pilotauth/` | Device registration, returns a signed JWT |

### Devices
| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1.1/devices/:dongle_id/` | Device info |
| GET | `/v1/devices/:dongle_id/params` | List device config params |
| PUT | `/v1/devices/:dongle_id/params/:key` | Set a config param (pushed via WebSocket if connected) |
| DELETE | `/v1/devices/:dongle_id/params/:key` | Delete a config param |

### Routes and uploads
| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/route/:dongle_id` | List routes (paginated, `?limit=` `?offset=`) |
| GET | `/v1/route/:dongle_id/:route_name` | Route detail with full segment list |
| GET | `/v1.4/:dongle_id/upload_url/` | Get self-hosted upload URL for a segment file |
| PUT | `/upload/:dongle_id/*` | Upload a segment file (up to 100 MB) |

### WebSocket
| Method | Path | Description |
|--------|------|-------------|
| GET | `/ws/v2/:dongle_id` | Persistent device channel (JSON-RPC over WebSocket) |

Supported RPC methods: `uploadFileToUrl`, `getNetworkType`, `getSimInfo`, `setNavDestination`, `setParam`, `deleteParam`.

### Health
| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | Returns `{"status": "ok"}` |

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
