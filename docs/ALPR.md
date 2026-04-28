# ALPR (Automatic License Plate Recognition)

> Status: **opt-in, optional**. Disabled by default. ALPR records license
> plate text from your dashcam footage; treat it as sensitive PII. The
> long-form operator documentation, retention policy, and disclaimer
> wording are owned by a later feature (`alpr-docs`); this document is a
> compact reference for setting up and operating the engine sidecar.

The ALPR feature runs as a separate Docker service (`comma-alpr`) that
the backend talks to over HTTP. The service is gated by the `alpr`
Docker Compose profile so it never starts unless you explicitly opt in.

See [ALPR-ENGINE-DECISION.md](ALPR-ENGINE-DECISION.md) for the rationale
behind the engine choice ([FastALPR](https://github.com/ankandrew/fast-alpr),
MIT-licensed, ONNX-only).

## Quick start

```bash
# Build the sidecar image (locally; no upstream image is published).
make alpr-build

# Start the sidecar. Composes cleanly with the prod stack.
make alpr-up

# (optional) Tail the sidecar logs.
make alpr-logs

# Stop and remove the sidecar.
make alpr-down
```

`make alpr-up` does not depend on `make prod-up`, so you can bring the
engine up alongside a running prod stack or against your local dev
backend without changes. Image tags are `comma-alpr:dev` for the
working-tree build; tag your own production images with the git short
SHA so rollback is straightforward.

## API contract

The sidecar listens on port `8081` inside the Compose network. By
default no host port is published; uncomment the `ports` stanza in
`docker-compose.yml` or pass `-p 8081:8081` if you want to curl it from
the host.

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/health` | Liveness probe + capability flags (`region`, `supports_attributes`, `engine_loaded`) |
| `POST` | `/v1/detect` | Single-frame detection. Accepts multipart form-data on the `image` field, JSON `{"image_b64": "..."}`, or the raw image bytes as the request body. Returns `{"detections": [...], "elapsed_ms": int}`. Server-side timeout: 5 seconds. |

The Go client lives in [`internal/alpr`](../internal/alpr) and is wired
into the deps struct as a lazy accessor (`deps.ALPRClient()`), so
toggling the runtime `alpr_enabled` flag does not require a server
restart.

## GPU activation (optional)

CPU is the default deployment target. The CPU image processes ~3-8
keyframes/sec on a modern x86 CPU which is more than enough for the
production worker's 1 fps sample rate (see the engine decision doc for
the load profile). GPU is only worth the hassle if you intend to backfill
years of footage.

To enable the GPU variant:

1. Edit `docker/alpr/Dockerfile` to switch the base image to
   [`onnxruntime-gpu`](https://hub.docker.com/r/onnxruntime/onnxruntime-gpu)
   or pin a CUDA-enabled Python base. The wrapper service itself is
   unchanged.
2. Uncomment the `deploy.resources.reservations.devices` block in the
   `alpr` service definition in `docker-compose.yml`.
3. Install
   [`nvidia-docker2`](https://github.com/NVIDIA/nvidia-container-toolkit)
   on the host. Without it the container starts but ONNX Runtime falls
   back to the CPU execution provider.
4. Rebuild and restart: `make alpr-build && make alpr-down && make alpr-up`.
5. Verify by hitting `/health` -- the `version` field reports the
   running fast-alpr version, and the engine logs an ONNX Runtime
   provider line at startup that should include `CUDAExecutionProvider`.

## Pin policy

The Dockerfile pins:

- `fast-alpr` and `onnxruntime` Python package versions in
  `docker/alpr/pyproject.toml`
- The detector and OCR model identifiers via the `ALPR_DETECTOR_MODEL`
  and `ALPR_OCR_MODEL` build args (defaults match the engine decision
  document)

Bump these intentionally and re-run the
[`tools/alpr-spike/`](../tools/alpr-spike) harness against your own
footage before rolling the new pin to production. Both axes have direct
accuracy and latency impact.

## See also

- [`docs/ALPR-ENGINE-DECISION.md`](ALPR-ENGINE-DECISION.md) -- engine choice rationale
- [`tools/alpr-spike/`](../tools/alpr-spike) -- benchmark harness for re-validating the engine choice
- [`docker/alpr/`](../docker/alpr) -- sidecar Dockerfile, FastAPI wrapper, dependency pins
- [`internal/alpr`](../internal/alpr) -- Go HTTP client + typed errors
