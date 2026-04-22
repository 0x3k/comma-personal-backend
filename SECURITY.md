# Security Policy

## Scope

This project is a self-hosted backend that receives data from comma.ai devices. It handles device authentication (JWT), file uploads, WebSocket connections, and stores driving data on the local filesystem and in PostgreSQL.

## Supported versions

Security fixes are applied to the latest release on `main`. There are no long-term support branches.

| Branch | Supported |
|--------|-----------|
| `main` | Yes |
| Other branches | No |

## Reporting a vulnerability

If you discover a security vulnerability, please report it privately. **Do not open a public GitHub issue.**

Email: **security@thereisnospoon.org**

Include:
- Description of the vulnerability
- Steps to reproduce or a proof of concept
- Impact assessment (what an attacker could do)
- Any suggested fix, if you have one

You should receive an acknowledgment within 48 hours. We will work with you to understand the issue and coordinate a fix before any public disclosure.

## Security considerations for self-hosters

This backend is designed to run on a private network or behind a reverse proxy. Keep the following in mind:

### Authentication

- Device authentication uses JWTs signed with `JWT_SECRET`. Use a strong, random secret (at least 32 bytes).
- The `ALLOWED_SERIALS` environment variable restricts which devices can register (keyed on the hardware serial, since the dongle_id is server-assigned). Set this if your server is exposed to the internet.
- There is no user authentication for the web UI -- it is intended for single-user, trusted-network deployments. Put it behind a VPN or reverse proxy with authentication if exposing to the internet.

### Network exposure

- The API server should sit behind a TLS-terminating reverse proxy (Caddy, nginx, etc.) in production. See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).
- The WebSocket endpoint (`/ws/v2/:dongle_id`) maintains persistent connections to devices. Ensure your proxy supports WebSocket upgrades.

### File uploads

- Uploads are bounded to 100 MB per file.
- Uploaded files are stored at `STORAGE_PATH` on the local filesystem. Ensure this directory has appropriate permissions.
- Filenames are validated against expected segment file patterns to prevent path traversal.

### Database

- Use a strong password for your PostgreSQL connection.
- Run PostgreSQL on localhost or a private network -- do not expose it to the internet.

## Known limitations

- The web UI has no authentication layer. It trusts the network boundary for access control.
- Device JWTs do not expire by default. Token rotation is the operator's responsibility.
