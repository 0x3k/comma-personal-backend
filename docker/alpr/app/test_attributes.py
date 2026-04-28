"""Unit tests for the vehicle-attribute helpers in app.attributes.

These cover the deterministic logic that runs without invoking the real
``open_image_models`` library: signature_key canonicalization, body-type
taxonomy mapping, confidence gating, and response shaping. The
classifier load path itself is exercised by the determinism integration
test in tests/test_determinism.py (skipped when Docker is unavailable).

Run from the engine container or from the project root:

    uv run --project docker/alpr python -m pytest docker/alpr/app/test_attributes.py
"""

from __future__ import annotations

import importlib

import pytest

from app import attributes


@pytest.fixture(autouse=True)
def _restore_module_state(monkeypatch):
    """Reset module-level globals between tests so toggling
    ATTRIBUTES_ENABLED via monkeypatch does not leak."""
    yield
    importlib.reload(attributes)


# -- signature_key canonicalization -------------------------------------------------


def test_signature_key_full_attributes():
    assert (
        attributes.canonical_signature_key("toyota", "camry", "silver", "sedan")
        == "toyota|camry|silver|sedan"
    )


def test_signature_key_drops_make_model_when_null():
    # The example called out in the acceptance criteria.
    assert (
        attributes.canonical_signature_key(None, None, "silver", "sedan")
        == "silver|sedan"
    )


def test_signature_key_drops_color_when_null():
    assert (
        attributes.canonical_signature_key("toyota", "camry", None, "sedan")
        == "toyota|camry|sedan"
    )


def test_signature_key_all_null_returns_empty():
    assert attributes.canonical_signature_key(None, None, None, None) == ""


def test_signature_key_empty_string_treated_as_null():
    # Empty strings (e.g. an upstream "" leak) must drop out the same as
    # None so the wire format does not contain double pipes.
    assert (
        attributes.canonical_signature_key("toyota", "", "silver", " ")
        == "toyota|silver"
    )


def test_signature_key_lowercases_and_strips():
    assert (
        attributes.canonical_signature_key("Toyota", " Camry ", "SILVER", "Sedan")
        == "toyota|camry|silver|sedan"
    )


def test_signature_key_fixed_field_order():
    # Same components, different argument-order would only ever happen
    # if a future caller passed the args wrong; lock the wire order in
    # so a regression there is caught by tests.
    a = attributes.canonical_signature_key("toyota", "camry", "silver", "sedan")
    b = attributes.canonical_signature_key("toyota", "camry", "silver", "sedan")
    assert a == b == "toyota|camry|silver|sedan"


# -- body type taxonomy --------------------------------------------------------


def test_body_type_passthrough_for_allowed():
    for value in attributes.ALLOWED_BODY_TYPES:
        assert attributes._map_body_type(value) == value


def test_body_type_synonyms_normalised():
    assert attributes._map_body_type("pickup") == "truck"
    assert attributes._map_body_type("pickup truck") == "truck"
    assert attributes._map_body_type("minivan") == "van"
    assert attributes._map_body_type("station wagon") == "wagon"
    assert attributes._map_body_type("motorbike") == "motorcycle"
    assert attributes._map_body_type("crossover") == "suv"


def test_body_type_unknown_returns_none():
    # Unknown classifier classes must NOT silently map to "other"; we
    # want the null state on the wire so downstream can tell apart "we
    # know it is something weird" from "we have no idea".
    assert attributes._map_body_type("hovercraft") is None
    assert attributes._map_body_type("") is None


# -- confidence gate -----------------------------------------------------------


def test_gate_below_threshold_returns_none(monkeypatch):
    monkeypatch.setattr(attributes, "MIN_CONFIDENCE", 0.5)
    assert attributes._gate(("toyota", 0.3)) is None


def test_gate_at_or_above_threshold_returns_lowercased(monkeypatch):
    monkeypatch.setattr(attributes, "MIN_CONFIDENCE", 0.5)
    assert attributes._gate(("Toyota", 0.5)) == "toyota"
    assert attributes._gate(("TOYOTA", 0.95)) == "toyota"


def test_gate_none_label_returns_none():
    assert attributes._gate((None, 0.99)) is None


def test_gate_missing_confidence_keeps_label():
    # Some upstream shapes drop the confidence entirely. Treat that as
    # "the engine did not score it but did predict a label" -- preserve
    # the label rather than over-aggressively gating.
    assert attributes._gate(("Toyota", None)) == "toyota"


def test_gate_int_year_passes_through(monkeypatch):
    monkeypatch.setattr(attributes, "MIN_CONFIDENCE", 0.5)
    assert attributes._gate_int(("2020", 0.9)) == 2020


def test_gate_int_year_rejects_non_numeric(monkeypatch):
    monkeypatch.setattr(attributes, "MIN_CONFIDENCE", 0.5)
    assert attributes._gate_int(("twenty-twenty", 0.9)) is None


# -- shape_response ------------------------------------------------------------


def test_shape_response_full_attributes(monkeypatch):
    monkeypatch.setattr(attributes, "MIN_CONFIDENCE", 0.5)
    raw = {
        "make": ("Toyota", 0.95),
        "model": ("Camry", 0.81),
        "color": ("Silver", 0.73),
        "body_type": ("Sedan", 0.88),
    }
    out = attributes._shape_response(raw)
    assert out["make"] == "toyota"
    assert out["model"] == "camry"
    assert out["color"] == "silver"
    assert out["body_type"] == "sedan"
    assert out["year_min"] is None
    assert out["year_max"] is None
    assert out["confidence"] == pytest.approx(0.73)
    assert out["signature_key"] == "toyota|camry|silver|sedan"


def test_shape_response_drops_below_threshold(monkeypatch):
    monkeypatch.setattr(attributes, "MIN_CONFIDENCE", 0.6)
    raw = {
        # make below threshold -> dropped to None
        "make": ("Toyota", 0.4),
        "model": ("Camry", 0.4),
        "color": ("Silver", 0.81),
        "body_type": ("Sedan", 0.91),
    }
    out = attributes._shape_response(raw)
    assert out["make"] is None
    assert out["model"] is None
    assert out["color"] == "silver"
    assert out["body_type"] == "sedan"
    assert out["signature_key"] == "silver|sedan"
    # Confidence is the minimum among surviving attributes.
    assert out["confidence"] == pytest.approx(0.81)


def test_shape_response_all_null(monkeypatch):
    monkeypatch.setattr(attributes, "MIN_CONFIDENCE", 0.6)
    raw = {
        "make": ("Toyota", 0.1),
        "model": ("Camry", 0.1),
        "color": ("Silver", 0.1),
        "body_type": ("Sedan", 0.1),
    }
    out = attributes._shape_response(raw)
    assert out["make"] is None
    assert out["model"] is None
    assert out["color"] is None
    assert out["body_type"] is None
    assert out["confidence"] is None
    assert out["signature_key"] == ""


def test_shape_response_handles_label_dict_shape(monkeypatch):
    """Library shape variant: dict-of-dicts with 'label'/'confidence' keys."""
    monkeypatch.setattr(attributes, "MIN_CONFIDENCE", 0.5)
    raw = {
        "make": {"label": "Toyota", "confidence": 0.9},
        "model": {"label": "Camry", "confidence": 0.85},
        "color": {"label": "Silver", "confidence": 0.7},
        "body_type": {"label": "Pickup", "confidence": 0.95},
    }
    out = attributes._shape_response(raw)
    assert out["make"] == "toyota"
    # body_type "pickup" is mapped to "truck" via the synonym table.
    assert out["body_type"] == "truck"
    assert out["signature_key"] == "toyota|camry|silver|truck"


def test_shape_response_unknown_body_type_becomes_null(monkeypatch):
    monkeypatch.setattr(attributes, "MIN_CONFIDENCE", 0.5)
    raw = {
        "make": ("Toyota", 0.9),
        "color": ("Silver", 0.7),
        "body_type": ("hovercraft", 0.95),
    }
    out = attributes._shape_response(raw)
    assert out["body_type"] is None
    assert out["signature_key"] == "toyota|silver"


# -- predict() with disabled toggle --------------------------------------------


def test_predict_returns_none_when_disabled(monkeypatch):
    monkeypatch.setattr(attributes, "ATTRIBUTES_ENABLED", False)
    # The library should NOT be touched; pass a sentinel that would
    # explode if it were.
    sentinel = object()
    assert attributes.predict(sentinel) is None


def test_predict_returns_none_when_classifier_unavailable(monkeypatch):
    monkeypatch.setattr(attributes, "ATTRIBUTES_ENABLED", True)

    def _boom():
        raise ImportError("open_image_models not installed in this environment")

    monkeypatch.setattr(attributes, "_try_import", _boom)
    # Force the lazy state to forget any cached classifier.
    monkeypatch.setattr(attributes, "_classifier", None)
    assert attributes.predict(object()) is None


# -- determinism guarantee -----------------------------------------------------


def test_signature_key_deterministic_for_equal_inputs(monkeypatch):
    """The wrapper's canonicalization must produce the same string for
    the same inputs across runs; this is how the engine satisfies the
    determinism criterion at the post-processing layer. ML jitter in
    raw confidences is absorbed by the gate so the key stays stable."""
    monkeypatch.setattr(attributes, "MIN_CONFIDENCE", 0.5)
    raw_a = {
        "make": ("toyota", 0.9001),
        "model": ("camry", 0.7501),
        "color": ("silver", 0.6101),
        "body_type": ("sedan", 0.9201),
    }
    raw_b = {
        "make": ("toyota", 0.9123),  # jittered third decimal
        "model": ("camry", 0.7623),
        "color": ("silver", 0.6232),
        "body_type": ("sedan", 0.9311),
    }
    out_a = attributes._shape_response(raw_a)
    out_b = attributes._shape_response(raw_b)
    assert out_a["signature_key"] == out_b["signature_key"]
