#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = [
#   "pillow>=10",
#   "psutil>=5.9",
#   "numpy>=1.26",
# ]
# ///
"""
ALPR engine benchmark harness for the comma-personal-backend spike.

Single-file PEP 723 script. Runs each requested engine against a directory of
JPEG frames, compares to a hand-labeled ground-truth CSV, and emits a per-engine
JSON results blob. The aggregate report flag prints a markdown table that can be
pasted into docs/ALPR-ENGINE-DECISION.md.

Engine plugins import their dependencies lazily so the script has zero static
import cost; you only pay the cost of an engine when you ask for it. Each plugin
falls back to a deterministic stub mode (--stub) so the harness is testable
without GPU drivers, ONNX models, or 4 GB of wheels installed.

Usage examples:

    uv run tools/alpr-spike/bench.py --help
    uv run tools/alpr-spike/bench.py --engine fast-alpr \\
        --samples tools/alpr-spike/samples \\
        --ground-truth tools/alpr-spike/ground_truth.csv \\
        --out tools/alpr-spike/results/fast-alpr.json
    uv run tools/alpr-spike/bench.py --engine all --out-dir tools/alpr-spike/results/
    uv run tools/alpr-spike/bench.py --report tools/alpr-spike/results/

The script intentionally does NOT run the engines as Docker containers. The
production engine is wrapped by docker/alpr/ (see alpr-engine-service). This
harness imports engines in-process for fast iteration on the spike.
"""

from __future__ import annotations

import argparse
import csv
import dataclasses
import importlib
import json
import os
import statistics
import sys
import time
from dataclasses import dataclass, field, asdict
from pathlib import Path
from typing import Callable, Optional

# ---------------------------------------------------------------------------
# Result data classes
# ---------------------------------------------------------------------------


@dataclass
class FrameResult:
    filename: str
    expected: str  # ground-truth plate text; "" means none
    detected: list[str]  # engine OCR outputs (uppercase, alphanumeric)
    latency_ms: float
    error: Optional[str] = None


@dataclass
class EngineResult:
    engine: str
    device: str  # "cpu" | "cuda" | "stub"
    model_version: str
    samples: int = 0
    detection_count: int = 0  # frames where the engine returned >=1 plate
    expected_present: int = 0  # frames where ground truth has a plate
    exact_matches: int = 0  # detected == expected (case-insensitive, alnum only)
    char_errors: int = 0  # sum of Levenshtein over expected_present frames
    char_total: int = 0  # sum of len(expected) over expected_present frames
    latencies_ms: list[float] = field(default_factory=list)
    peak_rss_mb: float = 0.0
    model_size_mb: float = 0.0
    notes: list[str] = field(default_factory=list)
    frames: list[FrameResult] = field(default_factory=list)

    # Derived metrics ------------------------------------------------------
    def detection_rate(self) -> float:
        return _safe_div(self.detection_count, self.expected_present)

    def exact_match_rate(self) -> float:
        return _safe_div(self.exact_matches, self.expected_present)

    def cer(self) -> float:
        return _safe_div(self.char_errors, self.char_total)

    def latency_mean_ms(self) -> float:
        return statistics.fmean(self.latencies_ms) if self.latencies_ms else 0.0

    def latency_p95_ms(self) -> float:
        if not self.latencies_ms:
            return 0.0
        return _percentile(self.latencies_ms, 95)


def _safe_div(num: float, den: float) -> float:
    return float(num) / float(den) if den else 0.0


def _percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    s = sorted(values)
    k = (len(s) - 1) * (pct / 100.0)
    lo = int(k)
    hi = min(lo + 1, len(s) - 1)
    if lo == hi:
        return s[lo]
    return s[lo] + (s[hi] - s[lo]) * (k - lo)


# ---------------------------------------------------------------------------
# Ground truth loading
# ---------------------------------------------------------------------------


def load_ground_truth(path: Path) -> dict[str, str]:
    """Returns {filename: expected_plate_uppercase_alnum}. Empty string means no plate."""
    out: dict[str, str] = {}
    if not path.exists():
        return out
    with path.open("r", newline="") as f:
        reader = csv.DictReader(_strip_comments(f))
        for row in reader:
            fn = (row.get("filename") or "").strip()
            if not fn:
                continue
            out[fn] = _normalize_plate(row.get("plate_text") or "")
    return out


def _strip_comments(lines):
    for line in lines:
        s = line.lstrip()
        if s.startswith("#"):
            continue
        yield line


def _normalize_plate(text: str) -> str:
    return "".join(c for c in text.upper() if c.isalnum())


# ---------------------------------------------------------------------------
# Levenshtein for CER
# ---------------------------------------------------------------------------


def _levenshtein(a: str, b: str) -> int:
    if a == b:
        return 0
    if not a:
        return len(b)
    if not b:
        return len(a)
    prev = list(range(len(b) + 1))
    for i, ca in enumerate(a, start=1):
        cur = [i]
        for j, cb in enumerate(b, start=1):
            cost = 0 if ca == cb else 1
            cur.append(min(cur[j - 1] + 1, prev[j] + 1, prev[j - 1] + cost))
        prev = cur
    return prev[-1]


# ---------------------------------------------------------------------------
# Engine adapters
# ---------------------------------------------------------------------------


@dataclass
class EnginePlugin:
    name: str
    license_: str
    detector: str
    ocr: str
    vehicle_attrs: str
    factory: Callable[[argparse.Namespace], "Detector"]


class Detector:
    """Adapter contract: returns list[str] of detected plates (uppercase alnum)."""

    name: str = "abstract"
    model_version: str = "0"
    model_size_mb: float = 0.0

    def detect(self, image_path: Path) -> list[str]:
        raise NotImplementedError


# --- FastALPR --------------------------------------------------------------


def _make_fast_alpr(args: argparse.Namespace) -> Detector:
    """Real FastALPR: pip install fast-alpr ; falls back to stub on ImportError."""
    if args.stub:
        return _StubDetector("fast-alpr", seed=1)
    try:
        fa = importlib.import_module("fast_alpr")
    except ImportError:
        sys.stderr.write(
            "[fast-alpr] fast_alpr not installed; using --stub mode. "
            "Install with: uv run --with fast-alpr tools/alpr-spike/bench.py ...\n"
        )
        return _StubDetector("fast-alpr", seed=1)

    class _FastALPR(Detector):
        name = "fast-alpr"
        model_version = getattr(fa, "__version__", "unknown")

        def __init__(self):
            # Default models per https://github.com/ankandrew/fast-alpr README.
            self.alpr = fa.ALPR(
                detector_model="yolo-v9-t-384-license-plate-end2end",
                ocr_model="global-plates-mobile-vit-v2-model",
            )

        def detect(self, image_path: Path) -> list[str]:
            from PIL import Image

            img = Image.open(image_path).convert("RGB")
            res = self.alpr.predict(img)
            out: list[str] = []
            for det in res or []:
                ocr = getattr(det, "ocr", None) or getattr(det, "plate", None)
                if ocr is None:
                    continue
                text = getattr(ocr, "text", None) or getattr(ocr, "plate", None) or ""
                norm = _normalize_plate(text)
                if norm:
                    out.append(norm)
            return out

    return _FastALPR()


# --- OpenALPR-OSS ----------------------------------------------------------


def _make_openalpr_oss(args: argparse.Namespace) -> Detector:
    """OpenALPR open-source CLI binding. Falls back to stub if `alpr` not on PATH."""
    if args.stub:
        return _StubDetector("openalpr-oss", seed=2)
    import shutil
    import subprocess

    if shutil.which("alpr") is None:
        sys.stderr.write(
            "[openalpr-oss] `alpr` CLI not on PATH; using --stub mode. "
            "Install via OS package manager (apt: openalpr) or build from "
            "https://github.com/openalpr/openalpr (last meaningful release 2018).\n"
        )
        return _StubDetector("openalpr-oss", seed=2)

    class _OpenALPR(Detector):
        name = "openalpr-oss"
        model_version = "2.3.0"  # last upstream tag

        def detect(self, image_path: Path) -> list[str]:
            try:
                proc = subprocess.run(
                    ["alpr", "-j", "-n", "5", str(image_path)],
                    capture_output=True,
                    timeout=10,
                    check=False,
                )
            except subprocess.TimeoutExpired:
                return []
            if proc.returncode != 0:
                return []
            try:
                payload = json.loads(proc.stdout.decode("utf-8", errors="replace"))
            except json.JSONDecodeError:
                return []
            out = []
            for r in payload.get("results", []):
                cands = r.get("candidates") or []
                if cands:
                    out.append(_normalize_plate(cands[0].get("plate", "")))
                else:
                    out.append(_normalize_plate(r.get("plate", "")))
            return [p for p in out if p]

    return _OpenALPR()


# --- DIY YOLOv8 + EasyOCR --------------------------------------------------


def _make_yolo_easyocr(args: argparse.Namespace) -> Detector:
    """DIY pipeline. Stubs unless ultralytics+easyocr+a license-plate weight are present."""
    if args.stub:
        return _StubDetector("yolo-easyocr", seed=3)
    try:
        ultralytics = importlib.import_module("ultralytics")
        easyocr = importlib.import_module("easyocr")
    except ImportError:
        sys.stderr.write(
            "[yolo-easyocr] ultralytics/easyocr not installed; using --stub mode. "
            "Install with: uv run --with ultralytics --with easyocr tools/alpr-spike/bench.py ...\n"
        )
        return _StubDetector("yolo-easyocr", seed=3)

    class _YoloEasyOCR(Detector):
        name = "yolo-easyocr"
        model_version = (
            f"yolov8={getattr(ultralytics, '__version__', '?')}"
            f",easyocr={getattr(easyocr, '__version__', '?')}"
        )

        def __init__(self):
            weight = os.environ.get("YOLO_PLATE_WEIGHT", "yolov8n.pt")
            self.det = ultralytics.YOLO(weight)
            langs = os.environ.get("EASYOCR_LANGS", "en").split(",")
            self.ocr = easyocr.Reader(langs, gpu=(args.device == "cuda"))

        def detect(self, image_path: Path) -> list[str]:
            from PIL import Image
            import numpy as np

            img = Image.open(image_path).convert("RGB")
            arr = np.asarray(img)
            results = self.det.predict(source=arr, verbose=False)
            plates: list[str] = []
            for r in results:
                boxes = getattr(r, "boxes", None)
                if boxes is None:
                    continue
                for box in boxes.xyxy.cpu().numpy():
                    x1, y1, x2, y2 = (int(v) for v in box[:4])
                    crop = arr[y1:y2, x1:x2]
                    if crop.size == 0:
                        continue
                    ocr_out = self.ocr.readtext(crop, detail=0)
                    text = "".join(ocr_out) if ocr_out else ""
                    norm = _normalize_plate(text)
                    if norm:
                        plates.append(norm)
            return plates

    return _YoloEasyOCR()


# --- Plate Recognizer (cloud, ceiling reference, NOT a deployment option) -


def _make_plate_recognizer(args: argparse.Namespace) -> Detector:
    """Cloud API. Documented as accuracy ceiling reference only -- defeats privacy posture."""
    if args.stub or not os.environ.get("PLATE_RECOGNIZER_TOKEN"):
        return _StubDetector("plate-recognizer", seed=4)
    import urllib.request
    import urllib.error

    token = os.environ["PLATE_RECOGNIZER_TOKEN"]

    class _PR(Detector):
        name = "plate-recognizer"
        model_version = "cloud"

        def detect(self, image_path: Path) -> list[str]:
            data = image_path.read_bytes()
            req = urllib.request.Request(
                "https://api.platerecognizer.com/v1/plate-reader/",
                data=_multipart_image(data, image_path.name),
                headers={
                    "Authorization": f"Token {token}",
                    "Content-Type": "multipart/form-data; boundary=----alprspike",
                },
            )
            try:
                with urllib.request.urlopen(req, timeout=15) as resp:
                    payload = json.loads(resp.read().decode("utf-8"))
            except (urllib.error.URLError, json.JSONDecodeError):
                return []
            return [
                _normalize_plate(r.get("plate", ""))
                for r in payload.get("results", [])
                if r.get("plate")
            ]

    return _PR()


def _multipart_image(data: bytes, filename: str) -> bytes:
    boundary = b"----alprspike"
    return (
        b"--" + boundary + b"\r\n"
        b'Content-Disposition: form-data; name="upload"; filename="'
        + filename.encode("ascii", "replace") + b'"\r\n'
        b"Content-Type: image/jpeg\r\n\r\n" + data + b"\r\n"
        b"--" + boundary + b"--\r\n"
    )


# --- Stub detector (for harness validation) --------------------------------


class _StubDetector(Detector):
    """Deterministic stub. Returns the ground-truth plate ~80% of the time so the
    harness can be smoke-tested without real engines installed. Per-engine seed
    means stubs differ from each other (so the comparison report has variance)."""

    def __init__(self, name: str, seed: int):
        self.name = name
        self.model_version = "stub"
        self.model_size_mb = 0.0
        self._seed = seed

    def detect(self, image_path: Path) -> list[str]:
        # Fake OCR derived deterministically from filename + seed.
        h = abs(hash((image_path.name, self._seed))) % 100
        if h < 80:
            # Pretend we read SOMETHING; harness compares to ground-truth at the
            # caller layer. Returning empty also exercises the "no detection" path.
            return [_normalize_plate(image_path.stem)[:7] or "STUB000"]
        return []


# --- Plugin registry -------------------------------------------------------


PLUGINS: dict[str, EnginePlugin] = {
    "fast-alpr": EnginePlugin(
        name="fast-alpr",
        license_="MIT",
        detector="YOLOv9-t (ONNX, 384px)",
        ocr="MobileViT v2 / PaddleOCR (ONNX)",
        vehicle_attrs="open-image-models plugin (make/color)",
        factory=_make_fast_alpr,
    ),
    "openalpr-oss": EnginePlugin(
        name="openalpr-oss",
        license_="AGPL-3.0 (legacy)",
        detector="LBP cascade",
        ocr="OpenALPR custom CNN",
        vehicle_attrs="none (separate openalpr-vehicle binary, abandoned)",
        factory=_make_openalpr_oss,
    ),
    "yolo-easyocr": EnginePlugin(
        name="yolo-easyocr",
        license_="AGPL-3.0 (Ultralytics) + Apache-2.0 (EasyOCR)",
        detector="YOLOv8 (license-plate fine-tune)",
        ocr="EasyOCR",
        vehicle_attrs="none (separate classifier required)",
        factory=_make_yolo_easyocr,
    ),
    "plate-recognizer": EnginePlugin(
        name="plate-recognizer",
        license_="Commercial (NOT a deployment option)",
        detector="proprietary",
        ocr="proprietary",
        vehicle_attrs="yes (paid tier)",
        factory=_make_plate_recognizer,
    ),
}


# ---------------------------------------------------------------------------
# Benchmark loop
# ---------------------------------------------------------------------------


def benchmark_engine(
    plugin: EnginePlugin,
    samples: list[Path],
    ground_truth: dict[str, str],
    args: argparse.Namespace,
) -> EngineResult:
    detector = plugin.factory(args)
    is_stub = isinstance(detector, _StubDetector)
    device = "stub" if is_stub else args.device
    result = EngineResult(
        engine=plugin.name,
        device=device,
        model_version=detector.model_version,
        model_size_mb=detector.model_size_mb,
    )
    if is_stub:
        result.notes.append(
            "Stub mode: real engine binaries / wheels not installed. "
            "Numbers below are NOT real benchmarks; install the engine and re-run."
        )

    peak_rss_mb = 0.0
    try:
        import psutil

        proc = psutil.Process()
    except Exception:
        proc = None

    for sample in samples:
        expected = ground_truth.get(sample.name, "")
        t0 = time.perf_counter()
        err: Optional[str] = None
        detected: list[str] = []
        try:
            detected = detector.detect(sample)
        except Exception as exc:  # noqa: BLE001 -- intentional, want to record failures
            err = f"{type(exc).__name__}: {exc}"
        latency_ms = (time.perf_counter() - t0) * 1000.0

        result.samples += 1
        result.latencies_ms.append(latency_ms)
        if expected:
            result.expected_present += 1
            if detected:
                result.detection_count += 1
                best = max(detected, key=lambda p: -_levenshtein(p, expected))
                if best == expected:
                    result.exact_matches += 1
                result.char_errors += _levenshtein(best, expected)
                result.char_total += max(len(expected), 1)
            else:
                result.char_errors += len(expected)
                result.char_total += len(expected)
        else:
            # Frame has no ground truth plate; detections are false positives but
            # do not factor into detection_rate / exact_match / CER.
            pass

        result.frames.append(
            FrameResult(
                filename=sample.name,
                expected=expected,
                detected=detected,
                latency_ms=latency_ms,
                error=err,
            )
        )

        if proc is not None:
            try:
                rss_mb = proc.memory_info().rss / (1024 * 1024)
                peak_rss_mb = max(peak_rss_mb, rss_mb)
            except Exception:
                pass

    result.peak_rss_mb = peak_rss_mb
    return result


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def cmd_run(args: argparse.Namespace) -> int:
    samples_dir = Path(args.samples)
    if not samples_dir.exists():
        sys.stderr.write(
            f"[bench] samples dir not found: {samples_dir}\n"
            "        Create it and drop fcamera JPEGs in (see README.md).\n"
        )
        return 2

    sample_paths = sorted(
        p for p in samples_dir.iterdir() if p.suffix.lower() in {".jpg", ".jpeg", ".png"}
    )
    if not sample_paths:
        sys.stderr.write(
            f"[bench] no JPEG/PNG samples in {samples_dir}\n"
            "        Drop ~50 fcamera frames and rerun.\n"
        )
        return 2

    gt = load_ground_truth(Path(args.ground_truth))
    if not gt:
        sys.stderr.write(
            f"[bench] WARN: ground_truth.csv empty or missing: {args.ground_truth}\n"
            "        Detection rate / exact match / CER will all be zero.\n"
        )

    if args.engine == "all":
        engines = list(PLUGINS.keys())
    else:
        engines = [args.engine]

    out_dir = Path(args.out_dir) if args.out_dir else None
    if out_dir:
        out_dir.mkdir(parents=True, exist_ok=True)

    summaries: list[EngineResult] = []
    for engine_name in engines:
        plugin = PLUGINS.get(engine_name)
        if plugin is None:
            sys.stderr.write(f"[bench] unknown engine: {engine_name}\n")
            return 2
        sys.stderr.write(f"[bench] running engine: {engine_name} ({len(sample_paths)} samples)\n")
        result = benchmark_engine(plugin, sample_paths, gt, args)
        summaries.append(result)

        target: Optional[Path]
        if args.out:
            target = Path(args.out)
        elif out_dir:
            target = out_dir / f"{engine_name}.json"
        else:
            target = None

        payload = _result_to_json(result, plugin)
        if target:
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(json.dumps(payload, indent=2))
            sys.stderr.write(f"[bench] wrote {target}\n")
        else:
            json.dump(payload, sys.stdout, indent=2)
            sys.stdout.write("\n")

    # Print a summary line per engine.
    sys.stderr.write("\n")
    sys.stderr.write(_format_table(summaries))
    return 0


def cmd_report(args: argparse.Namespace) -> int:
    results_dir = Path(args.report)
    if not results_dir.exists():
        sys.stderr.write(f"[bench] report dir not found: {results_dir}\n")
        return 2
    summaries: list[EngineResult] = []
    for f in sorted(results_dir.glob("*.json")):
        try:
            payload = json.loads(f.read_text())
        except json.JSONDecodeError:
            continue
        summaries.append(_json_to_result(payload))
    if not summaries:
        sys.stderr.write(f"[bench] no result JSONs found in {results_dir}\n")
        return 2
    sys.stdout.write(_format_table(summaries))
    return 0


def _result_to_json(result: EngineResult, plugin: EnginePlugin) -> dict:
    d = asdict(result)
    d["plugin"] = {
        "license": plugin.license_,
        "detector": plugin.detector,
        "ocr": plugin.ocr,
        "vehicle_attrs": plugin.vehicle_attrs,
    }
    d["derived"] = {
        "detection_rate": result.detection_rate(),
        "exact_match_rate": result.exact_match_rate(),
        "cer": result.cer(),
        "latency_mean_ms": result.latency_mean_ms(),
        "latency_p95_ms": result.latency_p95_ms(),
    }
    return d


def _json_to_result(payload: dict) -> EngineResult:
    frames = [FrameResult(**fr) for fr in payload.get("frames", [])]
    keys = {f.name for f in dataclasses.fields(EngineResult)}
    payload2 = {k: v for k, v in payload.items() if k in keys}
    payload2["frames"] = frames
    return EngineResult(**payload2)


def _format_table(summaries: list[EngineResult]) -> str:
    if not summaries:
        return ""
    rows = [
        "| engine | device | n | det rate | exact | CER | mean ms | p95 ms | peak RSS MB |",
        "|---|---|---|---|---|---|---|---|---|",
    ]
    for s in summaries:
        rows.append(
            "| {engine} | {device} | {n} | {det:.1%} | {exact:.1%} | {cer:.1%} | "
            "{mean:.1f} | {p95:.1f} | {rss:.0f} |".format(
                engine=s.engine,
                device=s.device,
                n=s.samples,
                det=s.detection_rate(),
                exact=s.exact_match_rate(),
                cer=s.cer(),
                mean=s.latency_mean_ms(),
                p95=s.latency_p95_ms(),
                rss=s.peak_rss_mb,
            )
        )
    return "\n".join(rows) + "\n"


def main(argv: Optional[list[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        description="ALPR engine benchmark harness (alpr-engine-spike).",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Engines:\n  "
            + "\n  ".join(
                f"{p.name:<18} {p.license_:<35} {p.detector} + {p.ocr}"
                for p in PLUGINS.values()
            )
            + "\n"
        ),
    )
    parser.add_argument(
        "--engine",
        choices=[*PLUGINS.keys(), "all"],
        help="Engine to benchmark, or 'all'.",
    )
    parser.add_argument(
        "--samples",
        default="tools/alpr-spike/samples",
        help="Directory of fcamera JPEGs (default: tools/alpr-spike/samples).",
    )
    parser.add_argument(
        "--ground-truth",
        default="tools/alpr-spike/ground_truth.csv",
        help="CSV with columns filename,plate_text,notes.",
    )
    parser.add_argument(
        "--out",
        help="Write results JSON to this path (single-engine mode).",
    )
    parser.add_argument(
        "--out-dir",
        help="Write results JSON to this directory (one file per engine).",
    )
    parser.add_argument(
        "--device",
        choices=["cpu", "cuda"],
        default="cpu",
        help="Inference device (CPU default; CUDA only if available).",
    )
    parser.add_argument(
        "--stub",
        action="store_true",
        help="Force stub mode for all engines (smoke-test the harness).",
    )
    parser.add_argument(
        "--report",
        help="Print a markdown comparison table from existing result JSONs.",
    )
    args = parser.parse_args(argv)

    if args.report:
        return cmd_report(args)
    if not args.engine:
        parser.error("--engine is required (or pass --report DIR).")
    return cmd_run(args)


if __name__ == "__main__":
    raise SystemExit(main())
