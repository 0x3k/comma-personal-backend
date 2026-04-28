"""HTTP wrapper around FastALPR exposing the contract documented in
.projd/progress/alpr-engine-service.json:

  GET  /health        -> {ok, model, version, region, supports_attributes}
  POST /v1/detect     -> {detections: [{plate, confidence, bbox, ...}]}

The Go backend (internal/alpr.Client) is the only intended caller. The
FastALPR engine itself is a Python library, not an HTTP service, which is
why this thin wrapper exists. See docs/ALPR-ENGINE-DECISION.md.

A single ALPR engine instance is created at process start and reused
across requests -- ONNX Runtime sessions are thread-safe but expensive
to construct, and we serve from one uvicorn worker by design (see
entrypoint.sh).
"""

from __future__ import annotations

import asyncio
import base64
import binascii
import io
import logging
import os
import time
from typing import Any

from fastapi import FastAPI, File, HTTPException, Request, UploadFile
from fastapi.responses import JSONResponse
from PIL import Image, UnidentifiedImageError

# fast_alpr is imported lazily on first /v1/detect so /health works even
# if the model download is still in flight at startup. Container readiness
# is therefore "process is up", not "model is loaded".
_alpr_lock = asyncio.Lock()
_alpr_engine: Any = None
_alpr_engine_error: str | None = None

# Per-request budget. Matches the contract in the feature spec
# (5s server-side timeout per request). Long-tail single-request latencies
# above this are a model-warmup or pathological-input symptom and the
# caller is better off retrying.
DETECT_TIMEOUT_SECONDS = float(os.environ.get("ALPR_DETECT_TIMEOUT", "5.0"))

# Maximum decoded image bytes accepted by /v1/detect. 10 MiB covers any
# reasonable JPEG keyframe; defends against accidental large uploads.
MAX_IMAGE_BYTES = int(os.environ.get("ALPR_MAX_IMAGE_BYTES", str(10 * 1024 * 1024)))

# Documented model identifiers reported by /health. Defaults match the
# build-time pin in the Dockerfile.
DETECTOR_MODEL = os.environ.get("ALPR_DETECTOR_MODEL", "yolo-v9-t-384-license-plate-end2end")
OCR_MODEL = os.environ.get("ALPR_OCR_MODEL", "global-plates-mobile-vit-v2-model")
REGION = os.environ.get("ALPR_REGION", "us")

# alpr-vehicle-attributes-engine is a future feature. The wrapper exposes
# the capability flag now so the Go client can branch on it without a
# server-side restart when the followup ships.
SUPPORTS_ATTRIBUTES = os.environ.get("ALPR_SUPPORTS_ATTRIBUTES", "false").lower() == "true"

# Best-effort version reporting. Reads the installed fast-alpr distribution
# version so /health reflects the model code in use.
try:
    from importlib.metadata import version as _pkg_version

    ENGINE_VERSION = _pkg_version("fast-alpr")
except Exception:  # pragma: no cover - defensive only
    ENGINE_VERSION = "unknown"

logger = logging.getLogger("alpr")
logging.basicConfig(level=os.environ.get("ALPR_LOG_LEVEL", "INFO").upper())

app = FastAPI(title="comma-alpr", version=ENGINE_VERSION)


async def _get_engine() -> Any:
    """Lazy-init the ALPR engine. Subsequent calls reuse the cached
    instance. Concurrent first-callers are serialised by an asyncio lock
    so we do not pay the model-load cost twice."""
    global _alpr_engine, _alpr_engine_error
    if _alpr_engine is not None:
        return _alpr_engine
    async with _alpr_lock:
        if _alpr_engine is not None:
            return _alpr_engine
        try:
            # Import inside the lock so the import-time model fetch (which
            # may hit the network) is also serialised.
            from fast_alpr import ALPR

            _alpr_engine = ALPR(detector_model=DETECTOR_MODEL, ocr_model=OCR_MODEL)
            _alpr_engine_error = None
        except Exception as exc:  # pragma: no cover - import path
            _alpr_engine_error = f"{type(exc).__name__}: {exc}"
            logger.exception("failed to initialise ALPR engine")
            raise
    return _alpr_engine


@app.get("/health")
async def health() -> dict[str, Any]:
    """Liveness + capability probe. Returns 200 even when the model is
    not yet loaded -- the container is still ready to serve later
    requests, and the engine_loaded flag tells the caller to skip
    detection until it flips true.
    """
    return {
        "ok": True,
        "model": f"{DETECTOR_MODEL}+{OCR_MODEL}",
        "version": ENGINE_VERSION,
        "region": REGION,
        "supports_attributes": SUPPORTS_ATTRIBUTES,
        "engine_loaded": _alpr_engine is not None,
        "engine_error": _alpr_engine_error,
    }


def _decode_image(raw: bytes) -> Image.Image:
    """Decode raw bytes into a PIL Image. Raises HTTPException on any
    failure path so the caller gets a structured 4xx instead of a
    generic 500."""
    if not raw:
        raise HTTPException(status_code=400, detail="image bytes are empty")
    if len(raw) > MAX_IMAGE_BYTES:
        raise HTTPException(
            status_code=413,
            detail=f"image exceeds max size of {MAX_IMAGE_BYTES} bytes",
        )
    try:
        img = Image.open(io.BytesIO(raw))
        img.load()
        return img.convert("RGB")
    except (UnidentifiedImageError, OSError) as exc:
        raise HTTPException(status_code=400, detail=f"failed to decode image: {exc}") from exc


def _normalise_bbox(box: Any, img_w: int, img_h: int) -> dict[str, float]:
    """Convert a fast-alpr bounding box to the documented {x,y,w,h}
    pixel-space dict. Tolerates a few possible upstream shapes
    (BoundingBox dataclass with x1/y1/x2/y2 attributes, dict, or 4-tuple)
    so this wrapper is robust across fast-alpr point releases."""
    if box is None:
        return {"x": 0.0, "y": 0.0, "w": float(img_w), "h": float(img_h)}
    x1 = y1 = x2 = y2 = None
    for attr_x1, attr_y1, attr_x2, attr_y2 in (
        ("x1", "y1", "x2", "y2"),
        ("xmin", "ymin", "xmax", "ymax"),
        ("left", "top", "right", "bottom"),
    ):
        if all(hasattr(box, a) for a in (attr_x1, attr_y1, attr_x2, attr_y2)):
            x1 = float(getattr(box, attr_x1))
            y1 = float(getattr(box, attr_y1))
            x2 = float(getattr(box, attr_x2))
            y2 = float(getattr(box, attr_y2))
            break
    if x1 is None and isinstance(box, dict):
        x1 = float(box.get("x1", box.get("xmin", box.get("left", 0.0))))
        y1 = float(box.get("y1", box.get("ymin", box.get("top", 0.0))))
        x2 = float(box.get("x2", box.get("xmax", box.get("right", 0.0))))
        y2 = float(box.get("y2", box.get("ymax", box.get("bottom", 0.0))))
    if x1 is None and isinstance(box, (list, tuple)) and len(box) == 4:
        x1, y1, x2, y2 = (float(v) for v in box)
    if x1 is None:
        # Final fallback: report the whole frame so the caller does not
        # crash on an unrecognised bbox shape.
        return {"x": 0.0, "y": 0.0, "w": float(img_w), "h": float(img_h)}
    return {
        "x": x1,
        "y": y1,
        "w": max(0.0, x2 - x1),
        "h": max(0.0, y2 - y1),
    }


def _detection_to_dict(det: Any, img_w: int, img_h: int) -> dict[str, Any] | None:
    """Map a fast-alpr ALPRResult to the documented detection dict.
    Returns None for results with no recognised plate text -- the caller
    has no use for a bbox without a plate string."""
    plate_text = ""
    plate_conf = 0.0
    bbox_obj = None

    detection = getattr(det, "detection", None)
    if detection is not None:
        bbox_obj = getattr(detection, "bounding_box", None) or getattr(detection, "bbox", None)

    ocr = getattr(det, "ocr", None)
    if ocr is not None:
        plate_text = str(getattr(ocr, "text", "") or "")
        plate_conf = float(getattr(ocr, "confidence", 0.0) or 0.0)

    if not plate_text:
        return None

    return {
        "plate": plate_text,
        "confidence": plate_conf,
        "bbox": _normalise_bbox(bbox_obj, img_w, img_h),
        # Vehicle attributes are filled by alpr-vehicle-attributes-engine.
        # The key is present-and-null so the Go client's optional-decode
        # path does not need to special-case its absence.
        "vehicle": None,
    }


async def _run_detect(image: Image.Image) -> list[dict[str, Any]]:
    """Run the synchronous ALPR predict() call inside a thread so the
    event loop stays responsive, and apply the per-request timeout."""
    engine = await _get_engine()

    def _predict() -> list[Any]:
        # fast-alpr expects an RGB or BGR numpy array; PIL Image -> numpy
        # is handled internally for >= 0.1.x, but be conservative.
        import numpy as np

        arr = np.asarray(image)
        return engine.predict(arr)

    try:
        results = await asyncio.wait_for(asyncio.to_thread(_predict), timeout=DETECT_TIMEOUT_SECONDS)
    except asyncio.TimeoutError as exc:
        raise HTTPException(status_code=504, detail="alpr detection timed out") from exc

    img_w, img_h = image.size
    detections: list[dict[str, Any]] = []
    for r in results or []:
        d = _detection_to_dict(r, img_w, img_h)
        if d is not None:
            detections.append(d)
    return detections


@app.post("/v1/detect")
async def detect(request: Request, image: UploadFile | None = File(default=None)) -> JSONResponse:
    """Accepts the same image either as multipart form-data (`image`
    field) or as JSON `{image_b64: "..."}`. Returns the documented
    detection list."""
    raw: bytes | None = None
    content_type = request.headers.get("content-type", "")

    if image is not None:
        raw = await image.read()
    elif content_type.startswith("application/json"):
        body = await request.json()
        if not isinstance(body, dict):
            raise HTTPException(status_code=400, detail="json body must be an object")
        b64 = body.get("image_b64")
        if not isinstance(b64, str) or not b64:
            raise HTTPException(status_code=400, detail="image_b64 is required for json requests")
        try:
            raw = base64.b64decode(b64, validate=True)
        except (binascii.Error, ValueError) as exc:
            raise HTTPException(status_code=400, detail=f"image_b64 is not valid base64: {exc}") from exc
    else:
        # No multipart `image` field, no JSON body -- accept a raw image
        # body too because that path is convenient for the Go client and
        # avoids an extra multipart wrap.
        raw = await request.body()

    if not raw:
        raise HTTPException(status_code=400, detail="no image provided")

    img = _decode_image(raw)
    started = time.monotonic()
    detections = await _run_detect(img)
    elapsed_ms = int((time.monotonic() - started) * 1000)
    return JSONResponse(
        status_code=200,
        content={
            "detections": detections,
            "elapsed_ms": elapsed_ms,
        },
    )
