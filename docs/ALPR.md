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
| `GET` | `/health` | Liveness probe + capability flags (`region`, `supports_attributes`, `engine_loaded`, `attributes_error`) |
| `POST` | `/v1/detect` | Single-frame detection. Accepts multipart form-data on the `image` field, JSON `{"image_b64": "..."}`, or the raw image bytes as the request body. Returns `{"detections": [...], "elapsed_ms": int}`. Server-side timeout: 5 seconds. |

The Go client lives in [`internal/alpr`](../internal/alpr) and is wired
into the deps struct as a lazy accessor (`deps.ALPRClient()`), so
toggling the runtime `alpr_enabled` flag does not require a server
restart.

### Detection response: `vehicle` field

Each detection in the `detections` array carries a `vehicle` field that
is either `null` (the engine did not run the classifier, or it failed
to load) or an object with the second-stage classifier output:

```json
{
  "plate": "ABC123",
  "confidence": 0.91,
  "bbox": {"x": 10, "y": 20, "w": 80, "h": 40},
  "vehicle": {
    "make": "toyota",
    "model": null,
    "year_min": null,
    "year_max": null,
    "color": "silver",
    "body_type": "sedan",
    "confidence": 0.61,
    "signature_key": "toyota|silver|sedan"
  }
}
```

Field semantics:

- **`make`**, **`model`**, **`color`**: lowercased strings, or `null`
  when the per-attribute confidence is below
  `ALPR_ATTRIBUTES_MIN_CONFIDENCE` (default `0.5`). Downstream relies
  on null-vs-present, NOT on absolute confidence values, so the engine
  drops uncertain attributes rather than guessing.
- **`year_min`**, **`year_max`**: integer year range, or `null`. The
  current classifier (`open-image-models`) does not predict year, so
  these are always `null` today; the fields exist so a future
  classifier swap does not require a contract change.
- **`body_type`**: one of `sedan`, `suv`, `truck`, `hatchback`,
  `coupe`, `van`, `wagon`, `motorcycle`, `other`, or `null`. Upstream
  class names that do not map cleanly to this taxonomy become `null`
  (we do not silently bucket unknown shapes into `other`).
- **`confidence`**: aggregate score for the `vehicle` payload, computed
  as the minimum surviving per-attribute confidence (most pessimistic;
  matches "every reported attribute is at least this confident"). May
  be `null` when no attribute survived the gate.
- **`signature_key`**: see "Vehicle signature canonicalization" below.

### Vehicle signature canonicalization

`signature_key` is generated server-side (Python is the authoritative
implementation, mirrored verbatim by the Go client) so canonicalization
rules cannot drift between the engine and any re-derivation in callers.
The rules are:

1. **Lowercase** every component.
2. **Strip** leading and trailing whitespace.
3. **Drop** `null` and empty-string components entirely. The wire
   format never contains double pipes; the count of pipe separators
   equals `count(non-null components) - 1`.
4. **Join** the surviving components in fixed order:
   `make | model | color | body_type`.
5. If every component is `null` or empty, the value is the empty
   string `""`. Downstream consumers (the encounter aggregator, the
   signature fusion heuristic) treat this as a non-grouping detection.

Examples:

| make | model | color | body_type | `signature_key` |
|------|-------|-------|-----------|-----------------|
| `toyota` | `camry` | `silver` | `sedan` | `toyota\|camry\|silver\|sedan` |
| `null` | `null` | `silver` | `sedan` | `silver\|sedan` |
| `toyota` | `null` | `silver` | `sedan` | `toyota\|silver\|sedan` |
| `null` | `null` | `null` | `null` | `""` |

The canonicalization is deterministic for any fixed input, including
when the upstream classifier exhibits jitter in the third decimal of
the per-attribute confidence (the gate at `ALPR_ATTRIBUTES_MIN_CONFIDENCE`
absorbs that jitter cleanly when the labels themselves agree).

### Disabling the attribute classifier

Set `ALPR_ATTRIBUTES_ENABLED=false` (env var, inside the engine
container) to skip the second-stage classifier entirely:

- `vehicle: null` on every detection.
- `/health` reports `supports_attributes: false`.
- The classifier is not loaded, no model weights are pulled, and the
  per-frame latency drops back to the plate-only baseline.

This is the documented compute-savings escape hatch; useful on
under-resourced hardware where the operator only wants plate text and
does not need vehicle grouping.

### Latency budget

The classifier runs once per frame on the same numpy array as the
plate detector (image-level, not per-bbox). The targets below are
based on the spike harness measurements (see
`tools/alpr-spike/bench.py`) plus public benchmarks for
`open-image-models` v0.2.x.

| Hardware | Plate-only mean | Plate + attributes mean | Notes |
|---|---|---|---|
| **CPU x86 (8-core modern)** | ~120-180 ms | **~150-210 ms (target ≤30 ms overhead)** | open-image-models adds ~25-35 ms per frame on CPU; comfortably within the 5s server-side timeout. |
| **GPU (RTX 3060+, CUDA EP)** | ~25-40 ms | **~30-45 ms (target ≤10 ms overhead)** | ONNX Runtime CUDA EP runs the attribute classifier and the plate detector concurrently in the same session pool. |

The targets above are budgets, not commitments: if a future
`open-image-models` release exceeds them, this doc and the
`docker/alpr/pyproject.toml` pin should be updated together. Re-validate
with `tools/alpr-spike/bench.py --engine fast-alpr --include-attributes`
on representative dashcam footage before bumping the pin.

If the classifier is the latency hot spot for your workload, set
`ALPR_ATTRIBUTES_ENABLED=false` (above) -- the production worker plan
in `alpr-detection-worker` only requires vehicle attributes for the
fusion heuristic, not for the primary plate-text path.

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
