#!/bin/sh
set -e

if [ -z "$DATABASE_URL" ]; then
    echo "entrypoint: DATABASE_URL is not set" >&2
    exit 1
fi

echo "entrypoint: running migrations..."
migrate -path /app/sql/migrations -database "$DATABASE_URL" up

echo "entrypoint: starting server..."
exec /app/server "$@"
