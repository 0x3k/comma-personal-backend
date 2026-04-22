# Contributing

Thanks for your interest in contributing to comma-personal-backend. This document covers the basics for getting a development environment running and submitting changes.

## Getting started

1. Fork the repo and clone your fork
2. Install prerequisites: Go 1.22+, Node.js 18+, pnpm, PostgreSQL 14+ with PostGIS, FFmpeg
3. Create the database and run migrations:

```bash
createdb comma
psql comma -c "CREATE EXTENSION IF NOT EXISTS postgis;"
psql comma < sql/migrations/001_init.up.sql
psql comma < sql/migrations/002_device_params.up.sql
```

4. Copy `.env.example` to `.env` and fill in `DATABASE_URL`
5. Start the backend and frontend:

```bash
go run ./cmd/server
pnpm install --dir web
pnpm --dir web dev
```

## Making changes

- Create a feature branch from `main`
- Keep commits focused -- one logical change per commit
- Write clear commit messages that explain *why*, not just *what*

## Code quality

Run these before submitting a PR:

```bash
# Backend
go test ./...
go vet ./...

# Frontend
pnpm --dir web lint
pnpm --dir web type-check
pnpm --dir web test
```

All checks must pass. PRs with failing tests or lint errors will not be merged.

## Backend conventions

- HTTP handlers live in `internal/api/` and are grouped by domain (devices, routes, uploads, etc.)
- Database queries go through sqlc -- edit the SQL in `sql/queries/`, then regenerate with `sqlc generate`
- No raw SQL strings in handler code
- Error responses use a consistent JSON envelope: `{"error": "...", "code": ...}`

## Frontend conventions

- Pages use the Next.js app router (`web/src/app/`)
- Shared components go in `web/src/components/`
- API types and client code live in `web/src/lib/`

## Pull requests

- Open a PR against `main`
- Describe what the change does and why
- Link any related issues
- Keep PRs small and reviewable -- split large changes into a series of PRs when possible

## Reporting bugs

Open a GitHub issue with:
- What you expected to happen
- What actually happened
- Steps to reproduce
- Your environment (OS, Go version, Node version, PostgreSQL version)

## Security issues

If you find a security vulnerability, **do not open a public issue**. See [SECURITY.md](SECURITY.md) for responsible disclosure instructions.

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](LICENSE).
