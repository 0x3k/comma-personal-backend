#!/bin/sh
# Launch the FastALPR wrapper service.
#
# We use a single uvicorn worker by design -- the underlying ONNX Runtime
# session is not safe to share across forked workers, and the production
# load (1 fps keyframe sampling) does not warrant per-worker model copies.
# Scale horizontally by running multiple sidecar containers behind a
# round-robin proxy if a single instance saturates.
set -e

exec uv run --no-sync uvicorn app.main:app \
    --host 0.0.0.0 \
    --port "${ALPR_PORT:-8081}" \
    --workers 1 \
    --log-level "${ALPR_LOG_LEVEL:-info}" \
    --no-access-log
