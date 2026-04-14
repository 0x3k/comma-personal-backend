# Device Setup

How to point an openpilot or sunnypilot device at this backend.

## Overview

Comma devices (comma 3, 3X) connect to two services:

1. **API host** -- REST API for device registration, upload URLs, route queries, and config params
2. **Athena host** -- persistent WebSocket for real-time communication (file upload requests, config pushes, nav destinations)

By default these point at `https://api.commadotai.com` and `wss://athena.comma.ai`. You override them with environment variables on the device.

## Prerequisites

Before configuring the device, make sure the backend is running and reachable over HTTPS. Openpilot enforces TLS -- it will not connect to plain HTTP or self-signed certificates. See [DEPLOYMENT.md](DEPLOYMENT.md) for setting up TLS with a reverse proxy.

## Configuration

SSH into your comma device and set these environment variables before openpilot starts:

```bash
export API_HOST="https://your-server.example.com"
export ATHENA_HOST="wss://your-server.example.com"
```

To make this persistent across reboots, add the exports to `/data/community/env.sh` (sunnypilot) or a startup script that runs before openpilot launches.

### What each variable controls

| Variable | Default | Used by |
|----------|---------|---------|
| `API_HOST` | `https://api.commadotai.com` | `common/api.py` -- all REST API calls (registration, upload URLs, route listing) |
| `ATHENA_HOST` | `wss://athena.comma.ai` | `system/athena/athenad.py` -- WebSocket connection for remote control |

### Endpoint mapping

The device expects these paths on your server:

| Device calls | Backend handler | Purpose |
|--------------|-----------------|---------|
| `POST /v2/pilotauth/` | `api/pilotauth.go` | First-boot registration, returns JWT |
| `GET /v1.4/:dongle_id/upload_url/?path=...` | `api/upload.go` | Get URL to PUT a segment file |
| `PUT /upload/:dongle_id/...` | `api/upload.go` | Actual file upload (URL from above) |
| `GET /v1.1/devices/:dongle_id/` | `api/device.go` | Device info |
| `GET /v1/route/:dongle_id` | `api/route.go` | Route listing |
| `GET /ws/v2/:dongle_id` | `ws/handler.go` | Athena WebSocket |

### WebSocket path

Openpilot's athenad connects to `ATHENA_HOST/ws/v2/<dongle_id>`. The backend listens on both `/ws/v2/:dongle_id` and `/ws/:dongle_id`, so no path rewriting is needed.

## Authentication flow

1. On first boot (or when the device has no cached token), athenad calls `POST /v2/pilotauth/` with the device's `dongle_id`, `public_key`, and `serial`
2. The backend upserts the device record and returns a signed JWT (`access_token`)
3. The device caches this token and includes it in all subsequent requests as `Authorization: JWT <token>` (REST) or `cookie: jwt=<token>` (WebSocket)
4. Tokens are valid for 90 days

If you set `ALLOWED_DONGLE_IDS` on the backend, only listed devices can register. Unrecognized devices get a 403.

## Upload flow

1. The device's uploader calls `GET /v1.4/<dongle_id>/upload_url/?path=<route>--<segment>/<filename>`
2. The backend parses the path and returns a JSON response: `{"url": "https://your-server.example.com/upload/<dongle_id>/<route>/<segment>/<filename>"}`
3. The device PUTs the file body to that URL
4. The backend writes the file to `STORAGE_PATH/<dongle_id>/<route>/<segment>/<filename>` and updates the database

Files uploaded per segment:
- `fcamera.hevc` -- road-facing camera (HEVC, ~10-30 MB/min)
- `ecamera.hevc` -- wide-angle camera
- `dcamera.hevc` -- driver-facing camera
- `qcamera.ts` -- low-res preview
- `rlog` / `rlog.bz2` -- full telemetry log
- `qlog` / `qlog.bz2` -- compressed telemetry log

Expect roughly 50-100 MB per minute of driving across all cameras and logs.

## Verifying the connection

After configuring the device and rebooting:

1. Check the backend logs for a `POST /v2/pilotauth/` request from the device's dongle ID
2. Verify the device appears in the web UI under Devices
3. After a drive, segments should start appearing under Routes within a few minutes
4. Check `GET /health` from the device's network to confirm basic reachability

## Troubleshooting

**Device won't register**
- Confirm the server is reachable from the device's network (cellular or Wi-Fi)
- Check that TLS certificates are valid (not self-signed, not expired)
- If using `ALLOWED_DONGLE_IDS`, verify the device's dongle ID is in the list
- Check backend logs for 4xx/5xx responses on `/v2/pilotauth/`

**Files not uploading**
- The upload body limit is 100 MB per request. HEVC files from longer segments may exceed this if segment boundaries are unusual
- Check that `STORAGE_PATH` exists, is writable, and has sufficient disk space
- Look for `warning: failed to track upload in database` in server logs -- file was saved but DB tracking failed

**WebSocket not connecting**
- Verify the `/ws/v2/` path rewrite is in place (see above)
- Check that your reverse proxy passes `Upgrade` and `Connection` headers for WebSocket
- The backend enforces one connection per device -- a new connection closes the old one
