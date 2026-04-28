# ALPR engine sidecar -- FastALPR (MIT, YOLOv9-t + PaddleOCR ONNX) wrapped
# in a small FastAPI service. See docs/ALPR-ENGINE-DECISION.md for the
# rationale behind the engine choice and docs/ALPR.md for operator-facing
# documentation including the GPU variant.
#
# Build context is the project root so this Dockerfile can be referenced
# from docker-compose.yml as `build: { context: ., dockerfile: docker/alpr/Dockerfile }`.
# Tag locally as `comma-alpr:dev`.
#
# CPU is the default deployment target. A CUDA variant is documented in
# docs/ALPR.md; activating it requires swapping the base image and
# uncommenting the device_requests stanza in docker-compose.yml.

FROM python:3.12-slim-bookworm

# Pin uv version explicitly so reproducibility does not drift across
# rebuilds. uv handles the entire Python tooling surface (env creation,
# dependency resolution, install) -- never invoke `python` / `pip` / `venv`
# / `pipx` directly inside this image.
ARG UV_VERSION=0.5.7

# System packages required by ONNX Runtime, PaddleOCR, and the
# image-decoding paths inside FastALPR.
#   libgl1, libglib2.0-0:  OpenCV runtime deps used by image preprocessing
#   libgomp1:              OpenMP runtime used by ONNX Runtime
#   curl, ca-certificates: model fetch + healthcheck
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        libgl1 \
        libglib2.0-0 \
        libgomp1 \
    && rm -rf /var/lib/apt/lists/*

# Install uv. Pinned by version so the install layer is cached and
# reproducible across rebuilds.
RUN curl -LsSf "https://astral.sh/uv/${UV_VERSION}/install.sh" | sh \
    && mv /root/.local/bin/uv /usr/local/bin/uv \
    && mv /root/.local/bin/uvx /usr/local/bin/uvx

WORKDIR /app

# Copy dependency manifests first so the heavy install layer is cached
# when only the wrapper source changes.
COPY docker/alpr/pyproject.toml docker/alpr/uv.lock* /app/

# `uv sync` resolves and installs into a project-local .venv. The
# pyproject pins fast-alpr / onnxruntime / fastapi / uvicorn to specific
# versions so model + runtime ABI stays consistent between rebuilds.
# --no-dev keeps the image lean (no test/lint deps).
ENV UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never
RUN uv sync --no-dev --frozen 2>/dev/null \
    || uv sync --no-dev

# Copy the wrapper service. Source files are kept tiny so this layer
# rebuilds cheaply during iteration.
COPY docker/alpr/app /app/app
COPY docker/alpr/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

# Pre-pull the FastALPR ONNX models at build time so the first request
# does not pay the download cost. The pin policy is documented in
# docs/ALPR.md -- bump the model strings here only when the operator has
# validated the new weights against their footage. The pre-pull script
# tolerates network failures so the image still builds in air-gapped
# environments; in that case the first /v1/detect call pulls them.
ARG ALPR_DETECTOR_MODEL=yolo-v9-t-384-license-plate-end2end
ARG ALPR_OCR_MODEL=global-plates-mobile-vit-v2-model
ENV ALPR_DETECTOR_MODEL=${ALPR_DETECTOR_MODEL} \
    ALPR_OCR_MODEL=${ALPR_OCR_MODEL}
RUN uv run --no-sync python -c "import os; \
from fast_alpr import ALPR; \
ALPR(detector_model=os.environ['ALPR_DETECTOR_MODEL'], ocr_model=os.environ['ALPR_OCR_MODEL'])" \
    || echo "warning: pre-pull failed (offline build?); models will fetch on first request"

# Pre-pull the open-image-models vehicle attribute classifier weights
# (alpr-vehicle-attributes-engine). Same air-gapped tolerance as above:
# the wrapper retries the import on first /v1/detect when ALPR_ATTRIBUTES_ENABLED
# is true. Set ALPR_ATTRIBUTES_ENABLED=false at run time to skip the
# classifier entirely (latency drops to plate-only).
ARG ALPR_ATTRIBUTES_ENABLED=true
ENV ALPR_ATTRIBUTES_ENABLED=${ALPR_ATTRIBUTES_ENABLED}
RUN uv run --no-sync python -c "from app.attributes import warmup; warmup()" \
    || echo "warning: open-image-models pre-pull failed (offline build?); classifier loads on first request"

EXPOSE 8081

# Run as a non-root user. Done last so the model cache directory written
# by the pre-pull above is owned by root and readable by `app` (the
# fast_alpr cache lives under /root/.cache by default; the wrapper
# service points HF_HOME / FAST_ALPR_CACHE under /app/.cache for the
# `app` user instead).
RUN useradd --create-home --shell /bin/sh app \
    && mkdir -p /app/.cache \
    && chown -R app:app /app/.cache
USER app
ENV HF_HOME=/app/.cache/huggingface \
    XDG_CACHE_HOME=/app/.cache

ENTRYPOINT ["/app/entrypoint.sh"]
