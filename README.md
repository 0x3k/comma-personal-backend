# comma-personal-backend

Personal backend for collecting and reviewing dashcam videos, logs, location data, and trip information from comma.ai devices, compatible with the openpilot/sunnypilot backend API.

## Getting Started

### Prerequisites

- Go 1.22+
- Node.js 20+ and pnpm
- PostgreSQL 16+ with PostGIS extension
- FFmpeg (for video transcoding)

### Installation

```bash
# Backend
go mod download

# Frontend
pnpm install --dir web
```

### Development

```bash
# Backend API server
go run ./cmd/server

# Frontend dev server
pnpm --dir web dev
```

### Testing

```bash
# Backend
go test ./...

# Frontend
pnpm --dir web test
```

## Configuration

Copy `.env.example` to `.env` and set:

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `DATABASE_URL` | yes | `postgres://localhost:5432/comma` | PostgreSQL connection string |
| `STORAGE_PATH` | yes | `./data` | Local filesystem path for video/log files |
| `PORT` | no | `8080` | API server listen port |
| `JWT_SECRET` | yes | - | Token signing for device auth |

## Compatibility

This backend implements the comma.ai API surface used by openpilot and sunnypilot for device registration, route/segment uploads, and data retrieval. Point your device's API endpoint to this server to use it as a self-hosted replacement.
