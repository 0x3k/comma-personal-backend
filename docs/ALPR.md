# ALPR (license-plate stalking detection)

> Status: **opt-in, optional**. Disabled by default. ALPR records license
> plate text from your own dashcam footage and stores it in your local
> database; treat that data as sensitive PII. Read this whole document
> before enabling the feature.

This document is the user-facing reference for the ALPR subsystem. For
the engine choice rationale and benchmark methodology, see
[`ALPR-ENGINE-DECISION.md`](ALPR-ENGINE-DECISION.md). For deployment
specifics (GPU passthrough, network isolation, backup of the encryption
key), see [`DEPLOYMENT.md`](DEPLOYMENT.md).

## 1. What this feature does (and what it does not do)

ALPR in this project is a **defensive personal-safety tool**. It runs
offline against footage you have already uploaded to your own server and
warns you when the same vehicle appears across multiple of your trips.
The intended use is detecting that someone may be following you over
days or weeks.

**It does:**

- process frames from your own dashcam, on your own server, against your
  own database
- detect license plates with a local ML engine (FastALPR), hash them,
  encrypt the recognized text at rest, and link them to a route + GPS
  point + timestamp
- group repeated plate reads into "encounters" and run a heuristic that
  flags repeated encounters across separate trips as potentially
  suspicious
- surface those flags in the web UI, with the ability to ack, snooze,
  whitelist, or note individual plates

**It does not:**

- send any data to a third party. The only network call is the backend
  talking to the ALPR engine sidecar over the Compose network on
  localhost. There is no upstream service, no telemetry, no outbound
  API. If you replace the engine URL with a hosted service, that is a
  decision you take on yourself.
- function as law-enforcement-style ALPR. There is no integration with
  any vehicle-of-interest list, hot list, or external database. There
  is no shared-server architecture and no multi-tenant model.
- track pedestrians, faces, or any non-plate identifier.
- run in real time on the device. ALPR is strictly post-upload, batch,
  on segments that already exist on disk.
- detect every plate. Low light, motion blur, weather, dirty plates,
  and unusual layouts cause misses; see the FAQ.

The whole feature is single-user, single-server, opt-in, and gated by
both an env-var-provided encryption key and an explicit disclaimer ack
that the user must perform from the Settings UI.

## 2. Threat model

ALPR is built for one specific scenario: **a private person trying to
notice whether the same vehicle keeps appearing in their dashcam
footage across separate trips.** Outside of that scenario, the feature
makes no claims and offers no protection.

### What this protects against

- A person who keeps showing up behind or near you, in the same vehicle,
  on multiple of your trips. The heuristic looks at distinct trip days
  and shared GPS turns.
- Casual following or "I keep seeing this car" pattern recognition that
  a human cannot reliably do across weeks of footage.
- A starting point for documenting a pattern. The plate hash + encounter
  list + per-route GPS plot is what you would hand to police, with
  full understanding that you are using ALPR data on yourself.

### What this does NOT protect against

- **A state-level or well-funded adversary.** They will use techniques
  this feature has no model for, and they will not be deterred by a
  flagged plate alert.
- **A local attacker who has root on your server, or your encryption
  key.** If they have the key, they can decrypt every plate you have
  ever recorded. Treat `ALPR_ENCRYPTION_KEY` like a database password
  (see Deployment). Filesystem-level encryption of the host is your
  responsibility.
- **A determined adversary who switches vehicles every trip.** The
  heuristic is built around plate identity. New plate, new vehicle, new
  rental, swapped plates -- all of those defeat it.
- **Stationary/non-driving stalking.** ALPR only sees what your
  dashcam saw on a route. Someone who follows you on foot or drives
  past your home while you are not driving is invisible to it.
- **Plates seen through bad optics.** Night driving, heavy rain, heavy
  glare on the plate, or the suspect driving close enough that only a
  partial plate is visible all reduce detection rate substantially.
  See the engine decision doc for measured rates.
- **Spoofing.** A bad actor who knows you run this can drop random
  plates into your footage to noise up the heuristic. The feature has
  no defense against intentional adversarial input.

If you need any of those, this is the wrong tool.

## 3. Privacy and legal posture

### Encryption at rest

All recognized plate text is encrypted at rest with AES-256-GCM. A
single 32-byte master key, supplied via `ALPR_ENCRYPTION_KEY`, is used
to derive subkeys with HKDF; nothing in the database holds plain plate
text. The plate hash (used for grouping repeat reads) is a keyed hash
of the normalized plate text and is also derived from the same root
key.

The server cannot start the ALPR pipeline without a valid 32-byte
key. Generate one with:

```bash
go run ./cmd/alpr-keygen >> .env   # adds ALPR_ENCRYPTION_KEY=...
```

If the key is rotated or lost, **all previously stored plate text and
hashes become unreadable**. There is no recovery path. Back the key up
the same way you back up your database password.

### Retention

The defaults match the threat model: keep recent reads long enough to
notice repeats; drop them after a month if nothing flagged them.

| Class | Default | Setting key |
|---|---|---|
| Unflagged plate reads | 30 days | `alpr_retention_days_unflagged` (env: `ALPR_RETENTION_DAYS_UNFLAGGED`) |
| Plate reads tied to a flagged event | 365 days | `alpr_retention_days_flagged` (env: `ALPR_RETENTION_DAYS_FLAGGED`) |

`0` in either column disables retention for that class, which is not
recommended -- the database grows unbounded and the heuristic does not
benefit from very old reads. Adjust with care.

The retention sweep is owned by a dedicated worker
(`internal/worker/alpr_cleanup.go`) that runs once every 24 hours when
both `CLEANUP_ENABLED=true` and `alpr_enabled=true`. It performs four
tiered passes per run:

1. **Unflagged detections.** Delete `plate_detections` older than
   `ALPR_RETENTION_DAYS_UNFLAGGED` whose `plate_hash` is NOT in the
   "flagged set" -- alerted+unacked watchlist rows OR alerted rows with
   `severity >= 4`. Whitelisted plates and acked low-severity (1..3)
   alerts are not in the flagged set, so their old detections age out
   under the unflagged window.
2. **Absolute ceiling.** Delete every `plate_detections` row older than
   `ALPR_RETENTION_DAYS_FLAGGED` regardless of flag state. Acts as the
   belt-and-braces guarantee that even a long-running alert cannot keep
   evidence past the operator-configured maximum window.
3. **Orphan encounters.** Delete `plate_encounters` rows whose
   underlying detections have all been pruned in the previous tiers.
4. **Orphan alert events.** Delete `plate_alert_events` older than 90
   days for plates no longer in `plate_watchlist`. Plates still on the
   watchlist keep their full audit trail.

`plate_watchlist` and `vehicle_signatures` are **never** touched by the
worker -- the former is user-curated state, the latter is reserved for
a future vehicle-attribute classifier (currently inactive).

`DELETE_DRY_RUN=true` (the default on first boot) makes the worker log
the delete sets it would issue without executing them, so operators can
audit the planned scope before flipping to real deletion. The same
flag also gates the route cleanup worker, so a single env toggle
controls both.

### Networking

The only HTTP call ALPR makes is from the Go backend to the ALPR engine
sidecar at `ALPR_ENGINE_URL` (default `http://alpr:8081`, the Compose
network hostname). There is no outbound HTTPS, no telemetry, no model
download at runtime, no analytics. The Docker image is built locally
from a pinned `fast-alpr` and `onnxruntime` and the model files are
baked in at build time.

If you publish the engine port (`8081`) to the host, you have decided
to expose it. The default `docker-compose.yml` does **not** publish
that port -- the backend reaches the engine over the Compose network
only.

### Jurisdictional notes

These are pointers, not legal advice. **Recording plate text in public
is regulated, and the rules differ by jurisdiction. Consult a lawyer in
your jurisdiction before enabling ALPR on footage you intend to keep.**
The disclaimer ack flow that gates the in-UI toggle says the same
thing.

- **Illinois (US)** -- BIPA does not directly cover plates, but the
  Illinois biometric privacy regime is the strictest in the US and
  related rulings have moved toward broad PII protection. Specific
  Illinois ALPR statutes also exist; check current law.
- **EU (GDPR)** -- a license plate plus location plus timestamp is
  considered personal data. Personal/household-use exemptions exist but
  are narrowly construed; sharing the data outside your household is
  almost certainly processing under GDPR.
- **UK (DPA 2018 / UK GDPR)** -- substantially similar to the EU
  position. The ICO has published guidance on dashcam recording in
  public. The personal-use exemption is narrow.
- **Canada (PIPEDA)** -- collecting plates from public space for
  personal safety is generally permitted, but commercial use or sharing
  triggers PIPEDA obligations.

The disclaimer the user acks before enabling ALPR is versioned -- the
current version is `2026-04-v1`. Any change to disclaimer wording bumps
the version, which causes the existing ack to expire and the UI toggle
to gate again until the user re-acks.

## 4. How it works

The pipeline is fully offline (relative to the public internet) and
runs in stages:

```
                                                     server (Go + Postgres)
+-------------------+   uploads     +----------------------------------------------------+
|  comma device     |  qcamera.ts   |                                                    |
|  (athenad +       +-------------->|  storage layer                                     |
|   uploader)       |               |    |                                               |
+-------------------+               |    v                                               |
                                    |  frame-extractor worker  (qcamera.ts -> JPEGs)     |
                                    |    |                                               |
                                    |    v                                               |
                                    |  detection worker --HTTP--> +--------------------+ |
                                    |    |                        |  ALPR engine       | |
                                    |    |   /v1/detect           |  (FastALPR sidecar)| |
                                    |    | <----------------------+                    | |
                                    |    v                        +--------------------+ |
                                    |  plate_detections (encrypted text + plate hash)    |
                                    |    |                                               |
                                    |    v                                               |
                                    |  encounter aggregator (group repeat reads)         |
                                    |    |                                               |
                                    |    v                                               |
                                    |  stalking heuristic (cross-trip + shared turns)    |
                                    |    |                                               |
                                    |    v                                               |
                                    |  plate_alert_events  --> notifier (email/webhook)  |
                                    |                                                    |
                                    |  web UI (Settings, alerts, watchlist, route view)  |
                                    +----------------------------------------------------+
```

In words: a frame-extractor reads keyframes out of `qcamera.ts` for
each segment of each route at the configured `ALPR_FPS` (default
2 fps), passes each frame to the ALPR engine sidecar over HTTP,
persists detections (encrypted plate text + keyed plate hash + GPS
sample + segment timestamp) into Postgres, groups consecutive reads of
the same plate into encounters, and runs the stalking heuristic at the
encounter level. Heuristic hits become alert events; alerts above the
configured severity trigger an email or webhook notification.

The heuristic also reads `route_turns` (turn signal events along the
route) so it can score how many of *your* turns the suspect plate also
took. Sharing many turns across many separate days raises the
severity. `route_turns` is intentionally retained even after ALPR is
disabled because it has uses outside ALPR (turn-based event detection,
trip clustering).

### Vehicle attribute classifier (deferred)

The original design called for a second-stage classifier (make / model
/ color / body type) so plate reads could be fused with a vehicle
signature. The library that scoping work assumed (`open-image-models`)
turned out to never have shipped that capability, so the entire
make/model/color/body-type pipeline is **not active** in this build.
Detections carry plate text + bbox + GPS only; the `vehicle_signatures`
table exists in the schema for forward compatibility but is never
populated.

The path to re-enable is documented in
[`ALPR-ENGINE-DECISION.md`](ALPR-ENGINE-DECISION.md): swap a real
classifier (PaddleClas PULC `vehicle_attribute` is the closest fit) into
the engine sidecar. Until then there is no `vehicle` field on detection
responses and no signature-fusion alerting.

## 5. Hardware requirements

CPU is the default deployment target and is fine for typical home use.
GPU only matters if you intend to backfill years of footage at once.
Numbers below are summarized from
[`ALPR-ENGINE-DECISION.md`](ALPR-ENGINE-DECISION.md); see that document
for the full benchmark methodology.

| Tier | Hardware | Throughput | When to pick this |
|---|---|---|---|
| Minimum (CPU only) | 4-core x86_64 (e.g. i5 9th gen / Ryzen 5 3600 / Apple M1), 8 GB RAM | ~6-8x faster than realtime at 1 fps sample rate | Single-user daily backfill within a few overnight hours. |
| Recommended (CPU only) | 8-core x86_64, 16 GB RAM | ~15-25x faster than realtime | Backfill weeks of footage in a single session. Most home server boxes meet this. |
| GPU (optional) | NVIDIA card with >=4 GB VRAM (GTX 1660, RTX 3050+) | ~50-150x faster than realtime | Backfilling years of footage, or extending the worker to per-frame inference. |

Memory profile, measured on the spike harness:

- ALPR engine container resident set: ~600 MB at steady state.
- Backend additional RSS for the workers: ~50-100 MB on top of the
  baseline server.
- ONNX model files baked into the image: ~50 MB total.

Disk impact, with default settings (2 fps sampled, 30-day unflagged
retention, US plate region):

- Plate detections per minute of driving: ~10-30 rows depending on
  traffic density.
- Storage cost per 30-day unflagged retention window: roughly 5-15 MB
  in Postgres (the encrypted plate text and the GPS sample dominate;
  the plate hash is fixed-size).
- Ballpark per-route disk impact at default fps: under 1 MB for a
  typical 30-minute commute; multiply by traffic.

If you have terabytes of historical routes you want to backfill, run
the GPU variant of the engine for that pass and switch back to CPU
afterwards (see Deployment).

## 6. Enabling ALPR (step by step)

ALPR cannot be enabled by accident. The toggle is gated by both an
encryption key being present and the operator acking the current
disclaimer version.

1. **Generate the encryption key.** This needs to happen exactly once
   per deployment. Save the output somewhere safe (password manager).
   Losing it means losing the ability to read any plate data ever
   recorded.

   ```bash
   go run ./cmd/alpr-keygen
   # prints e.g. ALPR_ENCRYPTION_KEY=NWY3M2I0...   (base64, 32 bytes)
   ```

2. **Put it in `.env`.** Append the line to your `.env` file:

   ```bash
   ALPR_ENCRYPTION_KEY=...the value from step 1...
   ```

3. **Restart the server** so it reloads the env var.
   `make prod-down && make prod-up` if you are on the Docker stack;
   otherwise `systemctl restart comma-backend` (or however you run it).

4. **Open the dashboard** and go to Settings -> Optional services ->
   ALPR. The toggle is visible only when a valid encryption key is
   configured; otherwise the section explains what is missing.

5. **Acknowledge the disclaimer.** The UI shows the current disclaimer
   text (version `2026-04-v1`); read it, pick your jurisdiction, and
   confirm. The ack is recorded with a timestamp; the toggle becomes
   actionable only after.

6. **Enable the toggle.** Workers do not start yet, because the engine
   sidecar is not running.

7. **Bring up the engine sidecar.**

   ```bash
   make alpr-build   # first time only; builds comma-alpr:dev locally
   make alpr-up
   ```

   The `alpr` Compose profile is gated, so bare `docker compose up`
   and `make prod-up` do not start it. `make alpr-up` is the only
   normal way to start it.

8. **Verify.** The Settings ALPR section reports engine health. The
   first detections should appear in the route detail view after the
   detection workers process the next uploaded segment.

To bring up everything in one shot from a cold start:
`make prod-up && make alpr-up`.

## 7. Disabling ALPR cleanly

Disable order matters. Toggle first, then stop the sidecar.

1. **Settings -> Optional services -> ALPR -> off.** This stops the
   workers, gates the ALPR HTTP endpoints (they return 503), and hides
   the ALPR surfaces in the web UI. Existing detections, encounters,
   and alerts remain in the database.

2. **Stop the sidecar:**

   ```bash
   make alpr-down
   ```

   This stops and removes the `comma-alpr` container so its CPU/RAM is
   freed. The image stays around so re-enabling is fast (`make alpr-up`).

After this, no new ALPR data is recorded and no engine container is
running. Existing data is retained until either the configured
retention window elapses (and the cleanup worker runs), or you wipe it
yourself (next section).

## 8. Optional data wipe

If you want to scrub every plate the system has ever seen -- e.g.
before handing the box to someone else, or after a key rotation that
made the data unreadable anyway -- you can truncate the ALPR tables
manually. **This is irreversible. There is no soft-delete and no
undo.**

```sql
-- run as the comma DB user
TRUNCATE TABLE
  plate_detections,
  plate_encounters,
  plate_watchlist,
  plate_alert_events,
  alpr_segment_progress,
  alpr_audit_log,
  vehicle_signatures
RESTART IDENTITY CASCADE;
```

Notes:

- `route_turns` is **not** in the wipe list. It is intentionally
  retained because it is useful for non-ALPR features (turn-based event
  detection, trip clustering). Drop it manually if you want.
- `vehicle_signatures` is included for forward compatibility; it sits
  empty in current builds (the vehicle-attribute classifier is deferred
  -- see Section 4).
- The `RESTART IDENTITY CASCADE` clause resets the row IDs so
  re-enabling later starts fresh.
- The encryption key in `.env` is unaffected. Removing the key string
  from `.env` is a separate (and recommended) cleanup step.

## 9. Operational tips

### How to spot OCR drift

If the engine starts misreading plates -- a model regression after
a pin bump, a noisy camera, dirt on the lens -- the cheapest signal is
the plate-hash distribution. Healthy footage produces a long-tailed
hash distribution: many distinct plates, each seen a small number of
times. If a single hash starts dominating, or hash entropy collapses,
something is wrong. Pull a sample:

```sql
SELECT plate_hash, count(*)
FROM plate_detections
WHERE detected_at > now() - interval '24 hours'
GROUP BY plate_hash
ORDER BY count(*) DESC
LIMIT 25;
```

A plate-hash that appears far more often than the next-most-common one
in everyday driving (excluding your own car if you have not whitelisted
it) is the first sign of an OCR drift. Re-run the spike harness
(`tools/alpr-spike/`) against your own footage if you suspect drift.

### When to re-tune

- **False positives from common neighbors.** If the heuristic flags the
  same plate every week and the plate is a neighbor or a coworker, add
  it to the watchlist as a whitelist entry. Whitelisted plates skip the
  alerting step but still record detections.
- **Missed plates that you can see by eye.** Lower
  `alpr_confidence_min` (env: `ALPR_CONFIDENCE_MIN`, default 0.75) in
  small steps (0.05). Going below 0.5 is not recommended -- you trade
  one problem for noisier detections. The runtime override accepts the
  range [0.5, 0.95].
- **Engine timeouts under load.** Bump `ALPR_DETECTOR_CONCURRENCY`
  cautiously; the engine sidecar is single-process and does not
  parallelize ONNX inference for free.

### Log signals to watch

The backend exports Prometheus counters and gauges. Helpful ones:

- `alpr_engine_errors_total` -- non-zero rate means the engine sidecar
  is unhealthy. Tail `make alpr-logs`.
- `alpr_dropped_no_gps_total` -- segments where the GPS track was
  missing or empty. A frame without a GPS sample cannot be turned into
  a useful detection (the heuristic needs the GPS point to score
  shared-turn overlap), so it is dropped on purpose. Persistent
  growth here points to the trip-aggregator or the upstream upload.
- `alpr_extractor_queue_depth` and `alpr_detector_queue_depth` --
  growing monotonically means the worker is falling behind real-time
  ingest.

The Grafana dashboard at `docs/grafana-dashboard.json` has panels for
each of these once the ALPR pipeline is enabled.

## 10. FAQ

**Why are some plates always missed?**
Low light, motion blur, heavy rain or glare, dirty plates, and unusual
plate layouts (vanity plates, government plates, foreign formats) all
reduce the OCR success rate. The engine decision doc reports ~95% on
well-lit US/EU plates and ~70% on night/dirty plates. ALPR is a
"notice the pattern" tool, not a "catch every plate" tool.

**Why did my own car alert?**
It should not, because the heuristic looks at cross-trip patterns and
your own car is a passenger of every trip. If it does -- e.g. you
share a vehicle with a household member who also has the dashcam
running -- whitelist the plate via Settings or the watchlist API. The
plate stays in detections (so the database remains internally
consistent) but never raises an alert.

**Can I export blurred video?**
Not yet. Plate redaction on share/export is tracked as a future
feature (`alpr-video-redaction-export`). Today, share links surface
the same video the device uploaded, with no redaction. Share links
are time-limited and signed -- treat them as you would any sensitive
link.

**Is this legal where I live?**
Recording dashcam video in public is generally legal in most
jurisdictions; *retaining* recognized plate text plus location plus
timestamp moves the data into "personal data" territory in many
jurisdictions and is more regulated. **This is not legal advice.**
The relevant frameworks (Illinois state law, EU GDPR, UK DPA, Canada
PIPEDA) all have their own definitions and exceptions. Consult counsel
in your jurisdiction before enabling ALPR on data you intend to keep.

**What if I rotate the encryption key?**
Every previously stored plate text and plate hash becomes unreadable.
There is no recovery, by design -- the encryption is doing its job.
Rotation also means the heuristic loses the ability to correlate
post-rotation reads with pre-rotation history, because the keyed plate
hash will not match. Rotate only when you truly need to (key
compromise, deliberate scrub) and accept the loss.

**Can I run this without Docker?**
Yes. The engine is just an HTTP service that implements
`GET /health` + `POST /v1/detect`; nothing about the Go backend
requires Docker. Run the engine bare-metal, point `ALPR_ENGINE_URL` at
its address, and the rest works. You will lose the convenience of
`make alpr-up`/`make alpr-down`, but the contract is the same.

## See also

- [`ALPR-ENGINE-DECISION.md`](ALPR-ENGINE-DECISION.md) -- engine choice rationale, benchmark methodology
- [`DEPLOYMENT.md`](DEPLOYMENT.md) -- GPU passthrough, network isolation, encryption-key backup
- [`tools/alpr-spike/`](../tools/alpr-spike) -- benchmark harness for re-validating the engine choice
- [`docker/alpr/`](../docker/alpr) -- sidecar Dockerfile, FastAPI wrapper, dependency pins
- [`internal/alpr`](../internal/alpr) -- Go HTTP client + typed errors
