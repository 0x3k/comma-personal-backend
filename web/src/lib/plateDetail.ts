/**
 * Typed wrappers around the per-plate detail endpoints. Mirrors the
 * shape of the alerts/api.ts module so the dashboard's data layer
 * stays consistent: small named functions over apiFetch, no client
 * object, individual types exported for component reuse.
 *
 * The wire shapes mirror internal/api/alpr_encounters.go (GET
 * /v1/plates/:hash_b64) and internal/api/alpr_corrections.go (PATCH
 * /v1/alpr/detections/:id, POST /v1/alpr/plates/merge) exactly so a
 * server-side field addition only needs the types widened here.
 */
import { apiFetch } from "@/lib/api";

/**
 * Vehicle signature embedded on the plate-detail payload. Identical
 * to the alerts API's AlertSignature -- duplicated here rather than
 * imported so a future signature divergence between the two endpoints
 * does not silently couple them.
 */
export interface PlateSignature {
  make?: string;
  model?: string;
  color?: string;
  body_type?: string;
  confidence?: number;
}

/**
 * Per-encounter row in the plate-detail response. Carries the route
 * identity (so the encounter list can link to the existing route
 * detail page) plus first/last seen timestamps and a coarse
 * area_cluster_label that the backend resolves from the trip's
 * reverse-geocoded start address (or a "lat,lng" fallback).
 */
export interface PlateDetailEncounter {
  dongle_id: string;
  route: string;
  first_seen_ts: string;
  last_seen_ts: string;
  detection_count: number;
  turn_count: number;
  signature: PlateSignature | null;
  area_cluster_label: string;
}

/**
 * Watchlist row associated with the plate. `null` when the plate has
 * no watchlist record at all -- the page renders "No alerts" in that
 * case.
 *
 * `kind` mirrors the database column: "alerted" for an open/acked
 * watchlist row, "whitelist" for a whitelist entry. The page treats
 * the two distinctly even though they share a table.
 */
export interface PlateWatchlistStatus {
  kind: string;
  severity: number | null;
  label?: string;
  acked_at: string | null;
}

/**
 * Cross-route aggregate stats computed by the backend over the full
 * encounter set (not the paginated slice). Display-as-is on the
 * stats card.
 */
export interface PlateDetailStats {
  distinct_routes_30d: number;
  distinct_areas_30d: number;
  total_detections: number;
  first_ever_seen?: string;
  last_ever_seen?: string;
}

/**
 * Full envelope returned by GET /v1/plates/:hash_b64. `plate` may be
 * the empty string if every encrypted detection failed to decrypt
 * (rare; the backend tries each encounter's sample); the page falls
 * back to a hash slice for display in that case.
 */
export interface PlateDetailResponse {
  plate: string;
  plate_hash_b64: string;
  watchlist_status: PlateWatchlistStatus | null;
  signature: PlateSignature | null;
  encounters: PlateDetailEncounter[];
  stats: PlateDetailStats;
}

/**
 * fetchPlateDetail loads a plate's cross-route history. The hash is
 * passed through verbatim (raw URL-safe base64 with no padding) since
 * URL-encoding it would corrupt the '-' / '_' characters base64-url
 * legitimately produces.
 *
 * 404 surfaces as a thrown Error with `.status === 404`; callers can
 * inspect that to render the "not in your history" empty state. All
 * other error shapes propagate via apiFetch's standard envelope.
 */
export function fetchPlateDetail(
  plateHashB64: string,
  opts: { limit?: number; offset?: number } = {},
): Promise<PlateDetailResponse> {
  const sp = new URLSearchParams();
  if (opts.limit !== undefined) sp.set("limit", String(opts.limit));
  if (opts.offset !== undefined) sp.set("offset", String(opts.offset));
  const qs = sp.toString();
  return apiFetch<PlateDetailResponse>(
    qs ? `/v1/plates/${plateHashB64}?${qs}` : `/v1/plates/${plateHashB64}`,
  );
}

/**
 * Edit-detection wire response. `hint` is a free-form string the UI
 * surfaces verbatim in a toast; `match_hash_b64` is set when the
 * post-edit hash already exists on a different watchlist row, in
 * which case the toast offers a one-click merge into that hash.
 */
export interface EditDetectionResponse {
  accepted: boolean;
  affected_routes: number;
  hint?: string;
  match_hash_b64?: string;
}

/**
 * editDetection rewrites a single plate_detections row's plate text
 * via PATCH /v1/alpr/detections/:id. The backend re-encrypts and
 * re-hashes server-side; the UI passes the new plate text and lets
 * the server own the cryptographic side.
 */
export function editDetection(
  detectionId: number,
  plate: string,
): Promise<EditDetectionResponse> {
  return apiFetch<EditDetectionResponse>(
    `/v1/alpr/detections/${detectionId}`,
    {
      method: "PATCH",
      body: { plate },
    },
  );
}

export interface MergePlatesResponse {
  accepted: boolean;
  affected_routes: number;
}

/**
 * mergePlates folds the source hash into the destination hash. After
 * a 200 the page redirects to the destination plate's URL so the
 * operator stays anchored on the canonical post-merge identity.
 *
 * Errors include 400 (malformed/equal hashes) and 404 (neither hash
 * exists); the calling modal surfaces these inline.
 */
export function mergePlates(
  fromHashB64: string,
  toHashB64: string,
): Promise<MergePlatesResponse> {
  return apiFetch<MergePlatesResponse>("/v1/alpr/plates/merge", {
    method: "POST",
    body: { from_hash_b64: fromHashB64, to_hash_b64: toHashB64 },
  });
}

/**
 * Watchlist add/remove + ack/unack thin wrappers, duplicated from the
 * alerts/api.ts module so the plate-detail page does not need to
 * cross-import a directory it otherwise has no dependency on. Both
 * endpoints are idempotent on the server.
 */
export function ackPlateAlert(
  plateHashB64: string,
): Promise<{ acked_at: string }> {
  return apiFetch<{ acked_at: string }>(
    `/v1/alpr/alerts/${plateHashB64}/ack`,
    { method: "POST" },
  );
}

export function unackPlateAlert(
  plateHashB64: string,
): Promise<{ acked_at: null }> {
  return apiFetch<{ acked_at: null }>(
    `/v1/alpr/alerts/${plateHashB64}/unack`,
    { method: "POST" },
  );
}

export function addPlateToWhitelist(
  plate: string,
  label?: string,
): Promise<{ plate_hash_b64: string; kind: string }> {
  return apiFetch<{ plate_hash_b64: string; kind: string }>(
    "/v1/alpr/whitelist",
    {
      method: "POST",
      body: { plate, label: label ?? "" },
    },
  );
}

export function removePlateFromWhitelist(
  plateHashB64: string,
): Promise<{ removed: boolean }> {
  return apiFetch<{ removed: boolean }>(
    `/v1/alpr/whitelist/${plateHashB64}`,
    { method: "DELETE" },
  );
}

/**
 * Per-route trip metadata used by the map panel to resolve an
 * encounter's first_seen location. The backend's plate-detail
 * endpoint does not embed coordinates on each encounter (the
 * encounters table holds bbox + timestamps but not GPS), so the page
 * fetches the trip row per encounter to plot a marker. The numbers
 * are small (one fetch per route the plate appears in, capped at the
 * default page size of 50) and trip rows are tiny.
 */
export interface TripBriefForPlateMap {
  start_lat: number | null;
  start_lng: number | null;
  start_address?: string | null;
  start_time?: string | null;
  duration_seconds?: number | null;
}

interface TripWireForPlateMap {
  start_lat: number | null;
  start_lng: number | null;
  start_address: string | null;
  start_time: string | null;
  duration_seconds: number | null;
}

/**
 * Subset of the per-route encounters response (GET
 * /v1/routes/:dongle_id/:route_name/plates) the plate-detail page
 * cares about: just enough to discover a representative detection id
 * for the edit modal. The full shape lives in PlateTimeline.tsx; we
 * narrow here to the fields actually consumed.
 */
interface RouteEncounterForLookup {
  plate_hash_b64: string;
  sample_thumb_url: string | null;
}

interface RouteEncountersResponse {
  encounters: RouteEncounterForLookup[];
}

/**
 * findRepresentativeDetectionId locates the most recent detection id
 * for the given plate on the given route. The plate-detail response
 * carries encounters but not detection ids, so we issue a single
 * follow-up call to the per-route endpoint and parse the id out of
 * sample_thumb_url (the only place the id surfaces in the read API).
 *
 * Returns null when the route has no matching encounter, or when the
 * matching encounter has no thumb URL to parse. Callers fall back to
 * a "no editable detection found" empty state.
 */
export async function findRepresentativeDetectionId(
  dongleId: string,
  routeName: string,
  plateHashB64: string,
): Promise<number | null> {
  const resp = await apiFetch<RouteEncountersResponse>(
    `/v1/routes/${encodeURIComponent(dongleId)}/${encodeURIComponent(routeName)}/plates`,
  );
  for (const enc of resp.encounters) {
    if (enc.plate_hash_b64 !== plateHashB64) continue;
    if (!enc.sample_thumb_url) return null;
    // sample_thumb_url is "/v1/alpr/detections/<id>/thumbnail"; we
    // pull the integer between /detections/ and /thumbnail. A failed
    // parse returns null rather than throwing -- the modal degrades
    // to "no editable detection found" instead of crashing.
    const m = enc.sample_thumb_url.match(/\/v1\/alpr\/detections\/(\d+)/);
    if (!m) return null;
    const id = Number.parseInt(m[1], 10);
    return Number.isFinite(id) && id > 0 ? id : null;
  }
  return null;
}

/**
 * fetchTripBriefForRoute pulls the trip metadata for a single route.
 * Returns null when the route has no trip row (a fresh ingest before
 * the trip-aggregator-worker has populated it). Network-level errors
 * propagate so the caller can surface them, but a 404 collapses to
 * null because "no trip yet" is a soft state, not a failure.
 */
export async function fetchTripBriefForRoute(
  dongleId: string,
  routeName: string,
): Promise<TripBriefForPlateMap | null> {
  try {
    const trip = await apiFetch<TripWireForPlateMap>(
      `/v1/routes/${encodeURIComponent(dongleId)}/${encodeURIComponent(routeName)}/trip`,
    );
    return {
      start_lat: trip.start_lat,
      start_lng: trip.start_lng,
      start_address: trip.start_address,
      start_time: trip.start_time,
      duration_seconds: trip.duration_seconds,
    };
  } catch (err) {
    const status = (err as { status?: number }).status;
    if (status === 404) return null;
    throw err;
  }
}
