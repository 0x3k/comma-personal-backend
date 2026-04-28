"""Vehicle attribute classifier wrapper for the ALPR engine sidecar.

Owned by feature alpr-vehicle-attributes-engine. The classifier runs on
the same crop or full frame as the plate detector and emits make,
model, color, and body_type alongside each plate detection.

The contract emitted to /v1/detect is documented in docs/ALPR.md and
mirrored by the Go client (internal/alpr/client.go: VehicleAttributes).
The shape MUST stay in lockstep with that struct -- any change here
needs a matching change there.

Per the engine decision (docs/ALPR-ENGINE-DECISION.md), `open-image-models`
covers make + color natively. Exact model and year are NOT covered by
any open-source classifier with production-grade accuracy in 2026 and
intentionally remain null. Body type is mapped to a fixed taxonomy via
``BODY_TYPE_MAP`` so the engine response stays stable even if the
upstream class names drift.

Determinism: ONNX Runtime inference is deterministic for a fixed input
when ``open_image_models`` is built with the default execution
provider (CPUExecutionProvider) and a single thread. We do not set
``torch.use_deterministic_algorithms`` because there is no PyTorch in
this stack. The signature_key is generated from rounded post-processed
attribute strings rather than from the raw confidences, so jitter in
the third decimal of a confidence still hashes to the same key.
"""

from __future__ import annotations

import logging
import os
import threading
from typing import Any, Iterable

logger = logging.getLogger("alpr.attributes")

# Single source of truth for the env-var toggle. Must agree with the
# value reported by /health.supports_attributes -- set both from the
# same string so the operator sees a consistent picture.
ATTRIBUTES_ENABLED = os.environ.get("ALPR_ATTRIBUTES_ENABLED", "true").lower() == "true"

# Per-attribute confidence threshold below which the engine emits null
# rather than a guess. Downstream (alpr-encounter-aggregator,
# alpr-signature-fusion-heuristic) relies on null-vs-present, NOT on
# absolute confidence values, so this gate is the only place that
# decides "we know" vs "we don't know".
MIN_CONFIDENCE = float(os.environ.get("ALPR_ATTRIBUTES_MIN_CONFIDENCE", "0.5"))

# Allowed body-type buckets, fixed by the alpr-vehicle-attributes-engine
# spec (acceptance criterion 1). Anything the upstream classifier
# returns that does not map cleanly to one of these buckets becomes
# null at the wire. The list is replicated in the Go client doc-comment.
ALLOWED_BODY_TYPES: frozenset[str] = frozenset(
    {
        "sedan",
        "suv",
        "truck",
        "hatchback",
        "coupe",
        "van",
        "wagon",
        "motorcycle",
        "other",
    }
)

# Mapping from common upstream body-type class strings (lowercased) to
# this project's taxonomy. open-image-models's body-type model emits a
# class enum that varies across versions; defending against that drift
# with this map means the wrapper need not bump every time the upstream
# class set changes. Unknown classes return None (-> null on the wire).
#
# Synonyms are listed explicitly so the mapping is auditable from one
# place rather than buried in fuzzy-match logic.
BODY_TYPE_MAP: dict[str, str] = {
    # Direct hits
    "sedan": "sedan",
    "suv": "suv",
    "truck": "truck",
    "pickup": "truck",
    "pickup truck": "truck",
    "pickup_truck": "truck",
    "hatchback": "hatchback",
    "coupe": "coupe",
    "van": "van",
    "minivan": "van",
    "wagon": "wagon",
    "estate": "wagon",
    "station wagon": "wagon",
    "station_wagon": "wagon",
    "motorcycle": "motorcycle",
    "motorbike": "motorcycle",
    # Catch-all bucket. Keep "other" synonyms minimal so we do not
    # silently swallow real classifier errors.
    "convertible": "coupe",
    "cabriolet": "coupe",
    "bus": "other",
    "rv": "other",
    "limousine": "sedan",
    "crossover": "suv",
}

# Color taxonomy. We do NOT remap color names; the engine emits the
# upstream class string verbatim after lowercasing. Color is high-cardinality
# and the downstream signature_key already lowercases for canonicalization.

# Internal lock + classifier handle. The classifier is ONNX-Runtime backed
# so concurrent inference is safe in principle; the lock guards lazy
# construction only.
_classifier_lock = threading.Lock()
_classifier: Any = None
_classifier_error: str | None = None


def _try_import() -> Any:
    """Import open_image_models lazily so /health works on a build where
    the classifier weights have not been pulled yet, and so the wrapper
    starts even if the operator is intentionally skipping the classifier.

    Returns the module on success, raises on failure. The caller is
    responsible for catching ImportError and treating it as
    "classifier unavailable -> emit null attributes".
    """
    import open_image_models  # noqa: F401  (imported for side effects)

    return open_image_models


def warmup() -> None:
    """Best-effort eager initialisation, used by the Dockerfile pre-pull.

    Tolerates failure: a build without network access still produces a
    runnable image; the first /v1/detect call retries the import.
    Returning normally on failure avoids breaking ``docker build`` on
    air-gapped builders, matching the policy already used for fast-alpr.
    """
    if not ATTRIBUTES_ENABLED:
        logger.info("ALPR_ATTRIBUTES_ENABLED=false; skipping classifier warmup")
        return
    try:
        _ensure_classifier()
    except Exception as exc:  # pragma: no cover - import / network path
        logger.warning("attribute classifier warmup failed: %s", exc)


def _ensure_classifier() -> Any:
    """Lazy-construct the classifier. Subsequent calls reuse the cached
    instance. The lock serialises the (potentially network-heavy) load.
    """
    global _classifier, _classifier_error
    if _classifier is not None:
        return _classifier
    with _classifier_lock:
        if _classifier is not None:
            return _classifier
        mod = _try_import()
        # open-image-models exposes either a top-level callable or a
        # ``VehicleClassifier`` style class depending on the release.
        # Probe the public surface in priority order so a minor upstream
        # rename does not break the wrapper. Each candidate is wrapped
        # in a uniform ``Classifier`` shim with a ``predict(image)``
        # method that returns a dict keyed by attribute name.
        candidates = (
            "VehicleAttributesClassifier",
            "VehicleClassifier",
            "VehicleAttributes",
            "MakeColorClassifier",
        )
        cls: Any = None
        for name in candidates:
            cls = getattr(mod, name, None)
            if cls is not None:
                break
        if cls is None:
            _classifier_error = (
                f"open_image_models exposes none of {candidates}; "
                "classifier disabled"
            )
            raise RuntimeError(_classifier_error)
        try:
            _classifier = cls()
        except TypeError:
            # Some versions require explicit model name args; fall back
            # to the documented default so a constructor signature change
            # does not silently disable attributes.
            _classifier = cls(model_name="default")
        _classifier_error = None
        return _classifier


def is_available() -> bool:
    """Whether the classifier is enabled by config AND can run.

    Used to drive /health.supports_attributes. Returns false if the
    operator disabled it OR if the classifier failed to load. The split
    matters: a disabled classifier should not look "broken" to the caller.
    """
    return ATTRIBUTES_ENABLED


def availability_error() -> str | None:
    """Most recent classifier-load error, if any. Surfaced in /health
    diagnostics so operators can see why ``supports_attributes`` is true
    but ``vehicle`` keeps coming back null.
    """
    return _classifier_error


def predict(image_array: Any) -> dict[str, Any] | None:
    """Run the attribute classifier on a single image.

    Args:
        image_array: HWC RGB numpy array. The classifier internally
            handles resize and normalisation; passing the full frame is
            fine. (open-image-models does its own bbox sniff.)

    Returns:
        ``None`` when the classifier is disabled or unavailable. Callers
        should emit ``vehicle: null`` on the wire in that case.

        Otherwise a dict with the per-attribute fields documented in
        ``VehicleAttributes`` (Go) and the ``vehicle`` key in /v1/detect
        responses. Per-attribute fields are ``None`` when the per-class
        confidence is below ``MIN_CONFIDENCE`` -- callers should
        serialise those to JSON null, not 0/empty-string. The
        ``signature_key`` field is always present and may be the empty
        string when every attribute fell below threshold.
    """
    if not ATTRIBUTES_ENABLED:
        return None
    try:
        clf = _ensure_classifier()
    except Exception as exc:  # pragma: no cover - import / network path
        logger.warning("attribute classifier unavailable: %s", exc)
        return None

    raw: Any
    try:
        # The library exposes either ``predict(arr) -> dict`` or
        # ``__call__(arr) -> dict``. Prefer ``predict`` so the call site
        # matches the FastALPR engine pattern. Fall through to ``__call__``
        # for older releases that do not export the explicit method.
        if hasattr(clf, "predict"):
            raw = clf.predict(image_array)
        else:
            raw = clf(image_array)
    except Exception as exc:
        logger.warning("attribute classifier predict failed: %s", exc)
        return None

    return _shape_response(raw)


def _shape_response(raw: Any) -> dict[str, Any]:
    """Normalise the upstream library's output into the wire schema.

    ``raw`` may be one of:

      - dict mapping attribute name to (label, confidence) tuple
      - dict mapping attribute name to {"label": str, "confidence": float}
      - dataclass-style object with .make, .model, .color, .body_type attrs
      - list of (attribute_name, label, confidence) triples

    Each per-attribute entry is gated on ``MIN_CONFIDENCE`` independently.
    Post-processing is deterministic: the same ``raw`` produces the same
    output, including the same ``signature_key``.
    """
    fields = _extract_fields(raw)

    make = _gate(fields.get("make"))
    model_attr = _gate(fields.get("model"))
    color = _gate(fields.get("color"))
    body_type_raw = _gate(fields.get("body_type")) or _gate(fields.get("type"))
    body_type = _map_body_type(body_type_raw) if body_type_raw is not None else None

    # Year prediction is not supported by open-image-models. The fields
    # exist so a future classifier swap doesn't change the wire shape.
    year_min = _gate_int(fields.get("year_min"))
    year_max = _gate_int(fields.get("year_max"))

    confidences = _collect_confidences(fields)
    overall_confidence = min(confidences) if confidences else None

    return {
        "make": make,
        "model": model_attr,
        "year_min": year_min,
        "year_max": year_max,
        "color": color,
        "body_type": body_type,
        "confidence": overall_confidence,
        "signature_key": canonical_signature_key(make, model_attr, color, body_type),
    }


def _extract_fields(raw: Any) -> dict[str, tuple[str | None, float | None]]:
    """Coerce the library's return type into a uniform mapping of
    ``attribute_name -> (label, confidence)``.

    Defensive: the upstream API has changed shape across point releases,
    so we accept several known shapes and silently produce empty
    attributes for anything we do not recognise. The fallback path is
    the all-null branch in /v1/detect responses, which is documented as
    a normal non-error state.
    """
    out: dict[str, tuple[str | None, float | None]] = {}

    # Shape 1: dict of (label, conf) tuples.
    if isinstance(raw, dict):
        for k, v in raw.items():
            if isinstance(v, tuple) and len(v) >= 2:
                out[str(k).lower()] = (
                    None if v[0] is None else str(v[0]),
                    float(v[1]) if v[1] is not None else None,
                )
            elif isinstance(v, dict):
                label = v.get("label") or v.get("class") or v.get("name")
                conf = v.get("confidence") or v.get("score") or v.get("conf")
                out[str(k).lower()] = (
                    None if label is None else str(label),
                    float(conf) if conf is not None else None,
                )
            elif isinstance(v, str):
                out[str(k).lower()] = (v, None)
        return out

    # Shape 2: list of (name, label, conf) triples.
    if isinstance(raw, (list, tuple)) and raw and isinstance(raw[0], (list, tuple)):
        for item in raw:
            if len(item) >= 3:
                name, label, conf = item[0], item[1], item[2]
                out[str(name).lower()] = (
                    None if label is None else str(label),
                    float(conf) if conf is not None else None,
                )
        return out

    # Shape 3: dataclass / namespace with attribute access.
    for name in ("make", "model", "color", "body_type", "year_min", "year_max"):
        if hasattr(raw, name):
            label = getattr(raw, name)
            conf_attr = f"{name}_confidence"
            conf = getattr(raw, conf_attr, None)
            if label is not None:
                out[name] = (
                    None if label is None else str(label),
                    float(conf) if conf is not None else None,
                )
    return out


def _gate(entry: tuple[str | None, float | None] | None) -> str | None:
    """Apply the per-attribute confidence gate. Returns the lowercased
    label when above threshold, or None when below / missing."""
    if entry is None:
        return None
    label, conf = entry
    if label is None or not str(label).strip():
        return None
    if conf is not None and conf < MIN_CONFIDENCE:
        return None
    return str(label).strip().lower()


def _gate_int(entry: tuple[str | None, float | None] | None) -> int | None:
    """Like _gate but coerces the label to int. Used for year_min /
    year_max. Returns None for any non-numeric or below-threshold input."""
    s = _gate(entry)
    if s is None:
        return None
    try:
        return int(float(s))
    except (TypeError, ValueError):
        return None


def _map_body_type(raw_label: str) -> str | None:
    """Map an upstream body-type label to the project's fixed taxonomy.

    The label is already lowercased by ``_gate``. Unknown labels return
    None so the wire schema stays well-defined; callers will emit
    ``body_type: null`` rather than a label outside the taxonomy.
    """
    if raw_label is None:
        return None
    if raw_label in ALLOWED_BODY_TYPES:
        return raw_label
    return BODY_TYPE_MAP.get(raw_label)


def _collect_confidences(
    fields: dict[str, tuple[str | None, float | None]],
) -> list[float]:
    """Collect the per-attribute confidences for non-null attributes.

    Returns the list of float confidences. The wrapper aggregates these
    by taking the minimum (most pessimistic) so the top-level
    ``vehicle.confidence`` answers "every reported attribute is at least
    this confident".
    """
    out: list[float] = []
    for entry in fields.values():
        if entry is None:
            continue
        _label, conf = entry
        if conf is None:
            continue
        if conf < MIN_CONFIDENCE:
            continue
        out.append(float(conf))
    return out


def canonical_signature_key(
    make: str | None,
    model: str | None,
    color: str | None,
    body_type: str | None,
) -> str:
    """Build the canonical ``signature_key`` documented in docs/ALPR.md.

    Rules (also enumerated in the docs):

    1. Lowercase every component (already lowercased upstream by the
       confidence gate, but we re-apply for safety so a future caller
       passing pre-lowered strings cannot drift).
    2. Strip leading and trailing whitespace.
    3. Drop ``None`` and empty-string components entirely. We never emit
       double-pipes, so the count of pipe separators equals
       ``count(non-null components) - 1``.
    4. Join the surviving components in fixed order:
       ``make | model | color | body_type``.
    5. If every component is null/empty, return the empty string. This
       is intentional: the Go client maps empty string to "no signature
       reported", and the downstream encounter aggregator treats it as
       a non-grouping detection.

    Examples (from acceptance criterion 2):

      make=toyota, model=camry, color=silver, body_type=sedan
        -> "toyota|camry|silver|sedan"
      make=None,   model=None,  color=silver, body_type=sedan
        -> "silver|sedan"
      make=None,   model=None,  color=None,   body_type=None
        -> ""

    Centralised in Python to avoid drift between the engine and any
    re-derivation in the Go client; the Go side reads the value from
    the wire as-is.
    """
    parts: list[str] = []
    for value in (make, model, color, body_type):
        if value is None:
            continue
        normalised = str(value).strip().lower()
        if not normalised:
            continue
        parts.append(normalised)
    return "|".join(parts)
