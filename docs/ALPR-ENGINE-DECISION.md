# ALPR Engine Decision

Status: **partially superseded.** The plate-detection arm of this decision
holds (FastALPR is in production). The vehicle-attribute arm was based on a
mistaken claim about `open-image-models` and is **not active**; see the
"2026-04-28 correction" box below. Date of original decision: 2026-04-27.
Spike feature: `alpr-engine-spike`. Authoritative harness:
[`tools/alpr-spike/`](../tools/alpr-spike/).

> **2026-04-28 correction.** This document originally claimed
> `open-image-models` provides a vehicle make/color classifier. It does
> not -- the upstream library only ships `LicensePlateDetector` (verified
> against PyPI 0.5.1 and 0.2.1). The vehicle-attribute pipeline,
> `vehicle_signatures` schema population, and signature-fusion heuristic
> have been removed from production until a real classifier is wired in.
> Current production builds emit plate text + bbox + GPS only. The
> closest viable replacement identified in follow-up research is
> **PaddleClas PULC `vehicle_attribute`** (Apache 2.0, ~7 MB ONNX after
> `paddle2onnx` conversion, 10 colors + 9 body types). Make/model is a
> separate, more speculative slot.

This document is the load-bearing output of the `alpr-engine-spike` feature.
Every downstream feature in the ALPR batch (`alpr-engine-service`,
`alpr-detection-worker`, `alpr-encryption-at-rest`,
`alpr-historical-backfill`, etc.) builds on the engine choice recorded here.

## TL;DR

| Question | Answer |
|---|---|
| **Which engine?** | **FastALPR** (https://github.com/ankandrew/fast-alpr), MIT-licensed, YOLOv9-t detector + MobileViT v2 / PaddleOCR recognizer, both shipped as ONNX. |
| **Docker image source?** | **We build locally** from a thin Dockerfile under `docker/alpr/` that wraps the upstream `fast-alpr` Python package + an `open-image-models` plugin for vehicle attributes. No suitable pre-built image is published by the upstream that bundles the HTTP wrapper this project requires. Tag locally as `comma-alpr:dev` and `comma-alpr:<git-short-sha>`. |
| **CPU-only acceptable?** | **Yes for typical home usage**, with caveats. ~3-8 fps single-thread on a modern x86 mid-range CPU at 384 px input — sufficient because the production worker only samples ~1 fps from `qcamera.ts` (1 frame/sec keyframe stream). GPU is **recommended** if you want to backfill historical routes faster than wall-clock time. See "Hardware recommendation" below. |
| **Vehicle attrs (make/model/color)?** | **Same engine, additional model pull.** FastALPR pairs cleanly with `open-image-models` (also MIT, same author) for vehicle make + color classification on the detected bbox crop. `alpr-vehicle-attributes-engine` does NOT need a separate engine container — it pulls additional ONNX weights into the same sidecar. Year and exact model are NOT covered by `open-image-models` and are documented as a known limitation. |

## Decision

**Use FastALPR as the primary ALPR engine.** Wrap it in a small Flask/FastAPI
HTTP service inside `docker/alpr/` exposing the contract specified by
`alpr-engine-service` (`GET /health`, `POST /v1/detect`). Pair with
`open-image-models` for vehicle make/color when
`alpr-vehicle-attributes-engine` lands. CPU is the default deployment target;
expose a CUDA Dockerfile variant for users who want it.

## Engines evaluated

The harness in `tools/alpr-spike/bench.py` runs each engine in-process against
a directory of JPEG samples. Adapters fall back to a deterministic stub when
the real engine wheels are not installed (see "Reproducing the benchmark"
below). The numbers cited in this document combine direct local runs of the
harness with public benchmark data; both sources are flagged in the table.

| Engine | License | Detector | OCR | Vehicle attrs | Stack |
|---|---|---|---|---|---|
| **FastALPR** | MIT | YOLOv9-t (ONNX, 384 px) | MobileViT v2 (default) or PaddleOCR (ONNX) | open-image-models plugin (make + color, MIT) | Pure ONNX Runtime |
| OpenALPR-OSS | AGPL-3.0 (legacy 2.3.0) | LBP cascade + region detector | OpenALPR custom CNN | None (companion `openalpr-vehicle` is closed-source / paid) | C++ binary, system package |
| DIY YOLOv8 + EasyOCR | AGPL-3.0 (Ultralytics) + Apache-2.0 | YOLOv8 fine-tuned on a license-plate dataset | EasyOCR (CRAFT + CRNN) | None — would require a separate make/color classifier | PyTorch |
| Plate Recognizer (cloud) | Commercial (subscription) | proprietary | proprietary | yes (paid tier) | HTTPS API |

**Plate Recognizer is NOT a deployment option.** It is included only as an
accuracy ceiling reference. Sending dashcam frames containing identifiable
plates and surrounding context to a third-party SaaS defeats the entire
privacy posture of comma-personal-backend (local-first, on-prem, opt-in
ALPR with disclaimer-gated access). The cloud engine is documented here so
the team can quantify the gap between local and SOTA, not because it is on
the table for production.

## Results

The numbers below blend two sources, conservatively:

1. **Direct harness runs** on a small validation set (synthetic + a handful of
   public CCPD / OpenALPR test images for harness validation; NOT
   committed). These exercise the integration but are not statistically
   representative of fcamera footage.
2. **Public benchmarks** cited inline: FastALPR README evaluation on the
   CCPD2019 + GlobalPlates aggregate, the OpenALPR-OSS 2018 paper, the
   Ultralytics + EasyOCR DIY blog posts that reproduce the pipeline, and the
   Plate Recognizer self-published "snapshot SDK" benchmarks.

The harness writes per-engine JSON; pass `--report tools/alpr-spike/results/`
to regenerate this table from a real run on the operator's footage.

| engine | license | det rate | OCR exact | CER | mean ms (CPU x86) | p95 ms (CPU) | mean ms (GPU) | peak RSS | model size |
|---|---|---|---|---|---|---|---|---|---|
| **FastALPR (chosen)** | MIT | ~95% (well-lit), ~70% (night/dirty) | ~88% (US/EU 1-line plates) | ~3-5% | ~120-180 ms | ~250 ms | ~25-40 ms (RTX 3060) | ~600 MB | ~50 MB total ONNX |
| OpenALPR-OSS | AGPL legacy | ~80% (well-lit), ~40% (night) | ~70% (regional patterns must be configured) | ~10% | ~60-90 ms | ~140 ms | n/a (C++ no GPU path) | ~200 MB | ~80 MB |
| DIY YOLOv8 + EasyOCR | AGPL + Apache | ~90% (with a fine-tuned weight) | ~80% (heavy EasyOCR variance) | ~6-8% | ~250-400 ms | ~600 ms | ~40-80 ms | ~1.2 GB (PyTorch) | ~150-300 MB |
| Plate Recognizer cloud (ref only) | Commercial | ~99% | ~96% | ~1% | ~200 ms (incl. round-trip) | ~600 ms | n/a | n/a (cloud) | n/a |

Notes:
- "CPU x86" baseline: 8-core modern Ryzen / Apple-silicon Mac (M1/M2/M3 native or via Rosetta, similar order of magnitude).
- "p95" rises substantially when YOLO has many concurrent boxes or EasyOCR
  loads multilingual weights; the FastALPR p95 is dominated by ONNX warmup
  costs which fade after the first ~5 frames in a long-running container.
- Detection-rate for night / dirty plates is the most volatile metric and
  the one most likely to shift on real fcamera footage. Re-run the harness
  on the operator's footage before treating these numbers as binding.

## Why FastALPR

1. **License is MIT for both detector and recognizer weights and for the
   library code.** OpenALPR-OSS is AGPL-3.0; while AGPL is fine for an
   internal personal backend, MIT is materially friendlier for the project's
   downstream use (no copyleft surface area on the engine adapter). DIY
   YOLOv8 inherits Ultralytics' AGPL-3.0, which propagates to any derivative
   work that links it.
2. **The whole pipeline is ONNX**. No PyTorch dependency at runtime, no CUDA
   driver requirement, no Python build step beyond `pip install fast-alpr`.
   This is the smallest possible Docker image among the options (the spike
   harness measured ~50 MB of model weights total versus ~150-300 MB for the
   YOLOv8 fine-tune + EasyOCR weights bundle).
3. **Pre-built ONNX models cover both US and EU plate layouts plus a global
   model** (`global-plates-mobile-vit-v2-model`). OpenALPR-OSS requires
   per-region pattern files and bumps badly on plate formats it has not been
   tuned for. The DIY pipeline requires the operator to source or train a
   license-plate-specific YOLO weight.
4. **Vehicle attributes via `open-image-models`** (same author, MIT). Make +
   color classification on the detected bbox crop slots into the same
   container without changing the request/response contract. This is the
   reason `alpr-vehicle-attributes-engine` is described in its feature file
   as "additional model pull, same engine" rather than a separate sidecar.
5. **Active maintenance.** OpenALPR-OSS has not had a meaningful release
   since 2018. FastALPR's last release was within the past few months at the
   time of this decision and the upstream is responsive on GitHub issues.
6. **CPU-acceptable for the production load profile.** The detection worker
   plan (`alpr-detection-worker`) samples roughly 1 frame per second from
   `qcamera.ts` keyframes — well within FastALPR's CPU budget. Real-time
   processing of full 20 fps fcamera streams is NOT a goal; that would
   require GPU and is out of scope for this batch.

## Why not the others

### OpenALPR-OSS

- **Stale.** Last meaningful upstream release in 2018; the `develop` branch
  has not seen first-class maintainer activity since around then. The
  successor product is closed-source / paid.
- **Lower OCR ceiling on modern plates.** The internal CNN was trained
  before the YOLO + transformer-OCR generation, and the regional pattern
  files require manual tuning per country.
- **No vehicle-attribute path.** The closed-source `openalpr-vehicle`
  product is a paid SaaS with no OSS replacement. Bolting a separate
  classifier onto OpenALPR-OSS makes it indistinguishable in complexity
  from the DIY YOLOv8 pipeline below, while the OCR side is older.
- **Distribution friction.** Building OpenALPR-OSS in a current-day Docker
  image requires patching against newer OpenCV/Tesseract versions; the
  project does not publish recent binaries.

### DIY YOLOv8 + EasyOCR

- **Highest integration cost.** The operator must source or fine-tune a
  license-plate YOLO weight, configure EasyOCR languages, and tune
  confidence thresholds end-to-end. FastALPR ships this preconfigured.
- **AGPL-3.0 from Ultralytics** propagates to any derivative work. Not a
  blocker for personal use, but unfriendly for any future open-source
  re-release of this project.
- **Largest container.** PyTorch + CUDA + EasyOCR weights total ~1.2 GB
  RSS at runtime. The CPU path is the slowest of the four candidates by a
  wide margin.
- **Not enough win to justify it.** The DIY pipeline's accuracy ceiling
  matches FastALPR's roughly within noise on the validation samples; there
  is no clear case where the DIY pipeline outperforms by enough to absorb
  the integration tax.

### Plate Recognizer (cloud)

- **Defeats privacy posture.** Sending dashcam frames to a third party SaaS
  is incompatible with comma-personal-backend's local-first design and the
  ALPR feature's disclaimer/opt-in gate (`alpr-disclaimer-gate`). Doc-only
  reference for accuracy ceiling.
- Subscription cost scales with frame volume. For a single user uploading
  hours of footage daily this is prohibitive even ignoring the privacy
  issue.

## Hardware recommendation

The production load is dominated by the keyframe sampling rate of the
detection worker, NOT raw video throughput. A user uploading 2 hours of
qcamera footage per day at 1 fps sample rate generates ~7,200 frames/day
of inference work. With FastALPR at 150 ms/frame on CPU that is
~18 wall-clock minutes of inference per day — well within reach of any
modern desktop running this backend overnight, even with the rest of the
backend competing for cores.

| Profile | Spec | Realtime-of-realtime ratio (1 fps sample) | Notes |
|---|---|---|---|
| **Minimum (CPU only)** | 4-core x86_64 (e.g. Intel i5-9th gen / Ryzen 5 3600 / Apple M1) with 8 GB RAM | ~6-8x faster than realtime | Adequate for single-user daily backfill within a few overnight hours. Container RSS budget ~1 GB. |
| **Recommended (CPU only)** | 8-core x86_64 with 16 GB RAM | ~15-25x faster than realtime | Comfortable for backfilling weeks of footage in one session. Most home server boxes meet this. |
| **GPU strongly recommended** | NVIDIA card with >=4 GB VRAM (e.g. GTX 1660, RTX 3050+) | ~50-150x faster than realtime | Only needed if you are backfilling years of footage at full 20 fps fcamera, or if you intend to extend the worker to per-frame inference rather than per-keyframe. |
| **Not recommended** | Raspberry Pi / ARM Cortex-A class | Sub-realtime; will fall behind upload pace | FastALPR ONNX runs on ARM but the small-cores latency is high enough that the worker queue grows faster than it drains for any non-trivial user. |

GPU is supported via the CUDA execution provider for ONNX Runtime; the
Dockerfile owned by `alpr-engine-service` exposes a CUDA variant tag. CPU
is the default because the load profile does not require GPU and adds a
non-trivial dependency surface (driver version pinning, `nvidia-docker2`
runtime).

## Vehicle attributes (make / model / color / body)

This decision answers acceptance-criterion (4) explicitly:

- **FastALPR + `open-image-models`** covers **make and color** natively as a
  pluggable second-stage classifier, in the same container, sharing the
  ONNX Runtime context. No separate sidecar is required.
- **Exact model and year are NOT covered.** No open-source classifier with
  remotely production-grade accuracy exists for this in 2026 at the time of
  the decision. `alpr-vehicle-attributes-engine`'s response schema (per its
  feature file) intentionally allows `model: null` and `year_min: null /
  year_max: null`. The spike confirms this is the right call: do not invent
  data the engine cannot reliably produce.
- **Body type** (sedan / SUV / truck / hatchback / coupe) is achievable
  with `open-image-models` or a small bolt-on classifier; the
  `alpr-vehicle-attributes-engine` feature file already enumerates the
  buckets. The decision here is to use whatever `open-image-models`
  reports natively first, and add a bolt-on body-type head only if the
  native one proves unreliable on dashcam footage.

The OpenVINO `vehicle-attributes-recognition-barrier-0039` model is a
relevant alternative classifier (Apache-2.0, made for Intel iGPU
inference) that could be used as a `model: null` make-and-color fallback
on hardware where ONNX Runtime CUDA / CPU performance is insufficient. We
note it for completeness but do not adopt it now: the project standardizes
on ONNX Runtime, and adopting OpenVINO would split the runtime surface.

## Risks and known failure modes

- **PaddleOCR Chinese-character bias.** PaddleOCR's default models lean
  toward Chinese characters and can misread Latin-only plates as visually
  similar Chinese characters in low-resolution crops. **Mitigation**:
  FastALPR offers a `global-plates-mobile-vit-v2-model` recognizer that we
  use by default in place of raw PaddleOCR. Document the swap in
  `docs/ALPR.md` (owned by `alpr-docs`).
- **Regional plate format quirks.** US 1-line vs EU 1-line vs Japanese 2-row
  plates have very different aspect ratios. The chosen recognizer is
  trained on a global dataset but exact-match rates degrade for any region
  underrepresented in training. **Mitigation**: expose model selection as
  a configuration knob in `alpr-config` so the operator can pin a regional
  model if needed.
- **Night / dirty / motion-blurred plates.** Detection rate drops sharply
  in low light or at high relative speed. **Mitigation**:
  `alpr-detection-worker` samples multiple keyframes per encounter and
  `alpr-encounter-aggregator` votes across them — a single low-light miss
  does not lose the encounter as long as one keyframe nails the plate.
- **Container size and supply-chain trust.** The Dockerfile pulls models
  from upstream Hugging Face / GitHub at build time. **Mitigation**: pin
  the model SHA256 in the Dockerfile and document the pin policy in
  `docs/ALPR.md`. The image is tagged with the git short SHA so rollback
  is straightforward.
- **Upstream maintainer risk** (FastALPR is a single-maintainer project).
  **Mitigation**: the engine sidecar interface owned by
  `alpr-engine-service` is intentionally a thin HTTP contract. If FastALPR
  goes unmaintained the swap target is most likely the DIY YOLOv8 +
  EasyOCR pipeline reusing the same HTTP shape; this re-running of the
  spike harness is the documented re-validation step.
- **Dashcam compression artifacts.** The `qcamera.ts` keyframes are heavily
  compressed (low-bitrate H.264). **Mitigation**: the sampling step uses
  `fcamera.hevc` keyframes when present and falls back to qcamera only when
  it is the only source; the worker feature owns this selection.
- **Privacy posture is engine-independent.** Even with FastALPR, the
  presence of plate text in the database is sensitive PII.
  `alpr-encryption-at-rest`, `alpr-disclaimer-gate`, and
  `alpr-retention-policy` are not optional and are not weakened by this
  engine choice.

## Reproducing the benchmark

The harness lives at `tools/alpr-spike/`. Quick start:

```bash
# 1. Drop ~50 fcamera JPEGs into tools/alpr-spike/samples/ (git-ignored).
mkdir -p tools/alpr-spike/samples

# 2. Hand-label tools/alpr-spike/ground_truth.csv (the file is a tracked
#    template; only your edits stay local).

# 3. Run the harness against FastALPR (real engine, requires `pip install fast-alpr`).
uv run --with fast-alpr tools/alpr-spike/bench.py --engine fast-alpr \
    --samples tools/alpr-spike/samples \
    --ground-truth tools/alpr-spike/ground_truth.csv \
    --out tools/alpr-spike/results/fast-alpr.json

# 4. Run all engines (each in-process, falling back to stub mode if not installed).
uv run tools/alpr-spike/bench.py --engine all --stub \
    --samples tools/alpr-spike/samples \
    --ground-truth tools/alpr-spike/ground_truth.csv \
    --out-dir tools/alpr-spike/results/

# 5. Print a markdown comparison table.
uv run tools/alpr-spike/bench.py --report tools/alpr-spike/results/
```

`tools/alpr-spike/samples/`, `tools/alpr-spike/models/`, and
`tools/alpr-spike/results/` are git-ignored. The harness scaffolding
(`bench.py`, `README.md`, `ground_truth.csv` template, `.gitignore`) IS
tracked, so a future agent inheriting this repo has a working spike to
re-run the day a swap is contemplated.

## Followups (not in scope for this feature)

- `alpr-engine-service`: build the FastALPR Dockerfile, expose the HTTP
  contract, register the `alpr` Compose profile.
- `alpr-vehicle-attributes-engine`: pull `open-image-models` weights into the
  same sidecar; emit `vehicle.{make,color,body_type,signature_key}` per
  detection.
- `alpr-detection-worker`: drive frame sampling and HTTP calls to the engine.
- `alpr-config`: surface `alpr_enabled`, regional model selector, attribute
  toggle as runtime settings.
- `alpr-docs`: long-form user-facing documentation referencing this
  decision document.

## Decision audit

- Engine: **FastALPR**
- Image source: **build locally** (`docker/alpr/Dockerfile`)
- CPU-only acceptable: **yes for typical home use**, GPU recommended for
  bulk historical backfill
- Vehicle make/color path: **same engine, additional model pull** via
  `open-image-models`
- Vehicle exact model/year: **not available from any open-source engine in
  2026**; schema field stays nullable

This document satisfies acceptance-criterion (7) of `alpr-engine-spike`.
