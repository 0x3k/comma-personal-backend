# alpr-spike

Time-boxed empirical evaluation of ALPR (Automatic License Plate Recognition) engines
for the comma-personal-backend dashcam pipeline.

The output of this spike is the decision document at
[docs/ALPR-ENGINE-DECISION.md](../../docs/ALPR-ENGINE-DECISION.md).

This directory contains the runnable harness that produced (or can re-produce) the
benchmark numbers in that document. The decision is the deliverable; the code here is
disposable scaffolding.

## What this is for

Pick an ALPR engine to embed (as a Docker sidecar) into the comma-personal-backend
stack. The right answer is empirical: detection rate, OCR exact-match rate, and
inference latency vary by an order of magnitude across engines and by lighting /
plate-region of the user's actual fcamera footage. Public benchmarks bias toward
high-quality, well-cropped plates; dashcam frames in low light at speed do not look
like that.

## Engines evaluated

| Engine | License | Detector | OCR | Vehicle attrs | Notes |
|---|---|---|---|---|---|
| FastALPR | MIT | YOLOv9 (ONNX) | PaddleOCR (ONNX) | open-image-models plugin (make/color) | Recommended; pre-built ONNX models, no GPU required for low FPS |
| OpenALPR-OSS | AGPL (legacy) | LBP/cascade + custom CNN | OCR via Tesseract-derived path | none | Last meaningful release 2018; included for floor reference only |
| YOLOv8 + EasyOCR (DIY) | AGPL-3.0 (Ultralytics) / Apache-2.0 | YOLOv8 (license-plate fine-tune) | EasyOCR | bolt-on classifier required | Highest flexibility, highest integration cost |
| Plate Recognizer (cloud) | commercial | proprietary | proprietary | yes (paid tier) | NOT a deployment option (privacy posture); cited as accuracy ceiling |

See `docs/ALPR-ENGINE-DECISION.md` for the full results table and reasoning.

## Layout

```
tools/alpr-spike/
  README.md            -- this file
  bench.py             -- PEP 723 single-file harness; see Usage
  ground_truth.csv     -- one row per sample image: filename,plate_text,notes
  samples/             -- (git-ignored) operator drops fcamera JPEGs here
  models/              -- (git-ignored) auto-downloaded model caches
  results/             -- (git-ignored) per-engine CSV + JSON output
  .gitignore
```

## Usage

The harness is a single PEP 723 Python script. `uv` resolves dependencies inline,
so there is no `requirements.txt` or `pyproject.toml` to maintain.

### 1. Drop sample frames

```bash
mkdir -p tools/alpr-spike/samples
# Copy ~50 fcamera JPEGs spanning day/night, highway/city, clear/dirty plates
# (tools/alpr-spike/samples is git-ignored)
```

### 2. Provide ground truth

Edit `tools/alpr-spike/ground_truth.csv`. One row per sample, columns:

```
filename,plate_text,notes
frame_0001.jpg,7ABC123,clear daytime
frame_0002.jpg,,no plate visible
frame_0003.jpg,4XYZ789,night highway
```

Empty `plate_text` means "no plate ground-truthed in this frame" -- the harness
treats any detection as a false positive against ground truth and a true positive
against the detection-rate metric only when other rules apply (see bench.py).

### 3. Run the harness

```bash
# Print available engines and flags
uv run tools/alpr-spike/bench.py --help

# Run the FastALPR engine against the sample directory.
uv run tools/alpr-spike/bench.py --engine fast-alpr \
    --samples tools/alpr-spike/samples \
    --ground-truth tools/alpr-spike/ground_truth.csv \
    --out tools/alpr-spike/results/fast-alpr.json

# Compare engines (writes one JSON per engine into results/).
uv run tools/alpr-spike/bench.py --engine all \
    --samples tools/alpr-spike/samples \
    --ground-truth tools/alpr-spike/ground_truth.csv \
    --out-dir tools/alpr-spike/results/

# Print a markdown comparison table from existing result JSONs.
uv run tools/alpr-spike/bench.py --report tools/alpr-spike/results/
```

### 4. CPU vs GPU

By default the harness runs ONNX Runtime with CPU execution providers. To benchmark
GPU latency on a machine with CUDA:

```bash
uv run tools/alpr-spike/bench.py --engine fast-alpr --device cuda \
    --samples tools/alpr-spike/samples \
    --ground-truth tools/alpr-spike/ground_truth.csv \
    --out tools/alpr-spike/results/fast-alpr-cuda.json
```

The harness records `device`, mean latency, p95 latency, peak RSS (Linux/macOS),
and on-disk model size into the result JSON.

## Honest limitations

This harness is a benchmark scaffold, not the production engine. Things it does
NOT do (intentionally):

- It does not run the engines as Docker containers; it imports them in-process so
  iteration is fast. The production engine sidecar is `docker/alpr/` (see
  `alpr-engine-service`).
- It does not reproduce the engine HTTP API contract from `alpr-engine-service`.
- It does not auto-generate ground truth. You hand-label ~50 frames once.

After the operator runs the harness on real fcamera footage, re-validate the
decision in `docs/ALPR-ENGINE-DECISION.md`. If the user's footage differs
materially from public dashcam datasets (regional plates, extreme lighting,
non-Latin scripts), the engine ranking can shift.

## Why this directory is git-ignored

The samples/ subdirectory contains raw fcamera frames and may include identifiable
plate text. The decision document already cites the relevant benchmark numbers, so
the spike code is mostly redundant once the decision is recorded. Keeping the
harness on disk (un-tracked) lets the operator re-run it without re-engineering.

If a future engine swap is needed, this directory is the reproducible record. The
spike code itself (`bench.py`, `README.md`, `.gitignore`, `ground_truth.csv`) is
checked in so future agents have the full harness; only the runtime artifacts
(samples, models, results) are git-ignored.
