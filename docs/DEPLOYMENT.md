# Deployment

How to run comma-personal-backend in production. The local dev setup (`go run ./cmd/server`) is fine for testing, but a real deployment needs TLS, process supervision, and a plan for storage growth.

## Architecture

```
internet --> reverse proxy (Caddy/nginx) --> Go API server (:8080)
                 |                                |
                 |                           PostgreSQL + PostGIS
                 |                                |
                 +-- static files (Next.js) ---> STORAGE_PATH/
```

The reverse proxy handles TLS termination and serves the frontend. The Go server handles API requests, WebSocket connections, and file uploads. FFmpeg runs as a subprocess for video transcoding.

## 1. Database

### Install PostgreSQL with PostGIS

```bash
# Debian/Ubuntu
sudo apt install postgresql postgis

# macOS (Homebrew)
brew install postgresql postgis
```

### Create the database

```bash
sudo -u postgres createuser --createdb comma
createdb -U comma comma
psql -U comma comma -c "CREATE EXTENSION IF NOT EXISTS postgis;"
psql -U comma comma < sql/migrations/001_init.up.sql
psql -U comma comma < sql/migrations/002_device_params.up.sql
```

## 2. Build the backend

```bash
go build -o server ./cmd/server
```

This produces a single static binary. Copy it to your server along with the `sql/` directory (for future migrations).

## 3. Build the frontend

```bash
pnpm install --dir web
pnpm --dir web build
```

The output is in `web/.next/`. You can serve it with `pnpm --dir web start` or export it as static files, depending on your setup. For simplicity, running `pnpm --dir web start` behind the reverse proxy works well.

## 4. Environment

Create `/etc/comma-backend/env`:

```bash
DATABASE_URL=postgres://comma:password@localhost:5432/comma
JWT_SECRET=<generate with: openssl rand -hex 32>
STORAGE_PATH=/var/lib/comma/data
PORT=8080
# ALLOWED_DONGLE_IDS=abc123,def456
```

Create the storage directory:

```bash
sudo mkdir -p /var/lib/comma/data
sudo chown comma:comma /var/lib/comma/data
```

## 5. Systemd service

Create `/etc/systemd/system/comma-backend.service`:

```ini
[Unit]
Description=comma personal backend
After=network.target postgresql.service
Requires=postgresql.service

[Service]
Type=simple
User=comma
Group=comma
EnvironmentFile=/etc/comma-backend/env
ExecStart=/usr/local/bin/comma-server
Restart=on-failure
RestartSec=5

# Hardening
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/comma/data
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

```bash
sudo cp server /usr/local/bin/comma-server
sudo systemctl daemon-reload
sudo systemctl enable --now comma-backend
```

Optionally create a second unit for the frontend:

```ini
[Unit]
Description=comma frontend
After=network.target

[Service]
Type=simple
User=comma
Group=comma
WorkingDirectory=/opt/comma-backend/web
ExecStart=/usr/bin/pnpm start
Environment=PORT=3000
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

## 6. Reverse proxy with TLS

Comma devices require valid HTTPS certificates. Self-signed certs will not work -- openpilot's HTTP client and WebSocket library both verify certificates against the system trust store.

### Option A: Caddy (recommended)

Caddy handles TLS certificates automatically via Let's Encrypt.

`/etc/caddy/Caddyfile`:

```
comma.yourdomain.com {
    # API and uploads
    handle /v1/* {
        reverse_proxy localhost:8080
    }
    handle /v1.1/* {
        reverse_proxy localhost:8080
    }
    handle /v1.4/* {
        reverse_proxy localhost:8080
    }
    handle /v2/* {
        reverse_proxy localhost:8080
    }
    handle /upload/* {
        reverse_proxy localhost:8080
    }
    handle /health {
        reverse_proxy localhost:8080
    }
    handle /storage/* {
        reverse_proxy localhost:8080
    }

    # WebSocket
    handle /ws/* {
        reverse_proxy localhost:8080
    }

    # Frontend (everything else)
    handle {
        reverse_proxy localhost:3000
    }
}
```

### Option B: nginx

```nginx
server {
    listen 443 ssl http2;
    server_name comma.yourdomain.com;

    ssl_certificate /etc/letsencrypt/live/comma.yourdomain.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/comma.yourdomain.com/privkey.pem;

    client_max_body_size 150m;

    # API routes
    location ~ ^/(v1|v1\.1|v1\.4|v2|upload|health|storage)(/|$) {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # Allow large uploads
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }

    # WebSocket
    location /ws/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_read_timeout 86400s;
    }

    # Frontend
    location / {
        proxy_pass http://127.0.0.1:3000;
        proxy_set_header Host $host;
    }
}
```

## 7. Storage planning

Each minute of driving produces roughly 50-100 MB across all cameras and logs. A typical 30-minute commute generates 1.5-3 GB. Plan accordingly:

| Driving per day | Raw storage per month | With transcoded HLS |
|-----------------|----------------------|---------------------|
| 30 min | ~50-90 GB | ~75-135 GB |
| 1 hour | ~100-180 GB | ~150-270 GB |
| 2 hours | ~200-360 GB | ~300-540 GB |

HLS transcoding (container copy mode) adds roughly 50% overhead because the same video data is re-segmented into `.ts` chunks alongside the original `.hevc` files. If you re-encode with libx264 (fallback mode), the transcoded output may be smaller but CPU usage is significantly higher.

Consider:
- Mounting a dedicated disk or partition at `STORAGE_PATH`
- Setting up a cron job to delete old raw HEVC files after HLS transcoding is complete
- Monitoring disk usage with standard tools (`df`, `du`, alerting)

## 8. Database backups

The database is small (metadata only -- actual files are on disk) but losing it means losing the mapping between routes, segments, and upload status.

```bash
# Daily backup via cron
0 3 * * * pg_dump -U comma comma | gzip > /var/backups/comma-$(date +\%Y\%m\%d).sql.gz

# Restore
gunzip < /var/backups/comma-20240315.sql.gz | psql -U comma comma
```

Back up `STORAGE_PATH` separately if you want full disaster recovery. rsync to a second disk or remote host works well for the file storage.

## 9. FFmpeg

The transcoder shells out to `ffmpeg`. Make sure it is installed and on the PATH for the service user:

```bash
# Debian/Ubuntu
sudo apt install ffmpeg

# macOS (Homebrew)
brew install ffmpeg

# Verify
ffmpeg -version
```

The transcoder first tries container copy (`-c:v copy`) which is fast and CPU-light. If the HEVC stream has encoding issues, it falls back to re-encoding with `libx264`, which is CPU-intensive. On a typical home server, 2-4 concurrent transcode workers is a reasonable default.

## 10. Firewall

Only port 443 (HTTPS) needs to be exposed to the internet. PostgreSQL (5432) and the Go server (8080) should only listen on localhost.

```bash
# ufw example
sudo ufw allow 443/tcp
sudo ufw allow 80/tcp   # for Let's Encrypt HTTP-01 challenge
sudo ufw enable
```

If your server is behind a home router, set up port forwarding for 443 and use dynamic DNS if you don't have a static IP.
