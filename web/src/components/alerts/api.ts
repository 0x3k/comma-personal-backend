/**
 * Typed wrappers around the alpr-watchlist-api endpoints. The wire
 * shapes mirror internal/api/alpr_watchlist.go exactly so a future
 * server-side field addition only needs the types widened here.
 *
 * All functions are thin adapters over apiFetch -- error handling and
 * 401 redirects live in the shared client. We do NOT export a "client"
 * object; consumers import the individual functions they need so the
 * tree can be tree-shaken cleanly.
 */
import { apiFetch } from "@/lib/api";

/**
 * AlertSignature is the signature object the listing returns alongside
 * each alert. The watchlist endpoint may return null when the plate
 * has no encounters yet (the row was seeded by a notification import,
 * for example), so consumers must guard on null before reading fields.
 */
export interface AlertSignature {
  make?: string;
  model?: string;
  color?: string;
  body_type?: string;
  confidence?: number;
}

/**
 * AlertLatestRoute is the most-recent route the plate was seen on,
 * used to render the "latest sighting" link on each row. Optional
 * fields (started_at, address_label) come from a JOIN against the
 * trip table, so they may be empty even when route+dongle are set.
 */
export interface AlertLatestRoute {
  dongle_id: string;
  route: string;
  started_at?: string;
  address_label?: string;
}

/**
 * AlertItem matches alertItem in internal/api/alpr_watchlist.go. Most
 * fields are nullable because the listing tolerates partial state
 * (no encounters yet, no signature yet, fresh row never acked).
 *
 * encounter_count is a count(*) over plate_encounters; latest_route
 * carries the most-recent encounter's route name. evidence_summary
 * is the human-readable one-liner derived from the most-recent
 * plate_alert_events row -- empty string when no event exists yet.
 */
export interface AlertItem {
  plate_hash_b64: string;
  plate: string;
  signature: AlertSignature | null;
  severity: number | null;
  kind: string;
  first_alert_at: string | null;
  last_alert_at: string | null;
  acked_at: string | null;
  encounter_count: number;
  latest_route: AlertLatestRoute | null;
  evidence_summary: string;
  notes?: string;
}

interface AlertsListResponse {
  alerts: AlertItem[];
}

/** Status filter accepted by GET /v1/alpr/alerts?status=. */
export type AlertStatus = "open" | "acked" | "all";

/** Listing query params accepted by GET /v1/alpr/alerts. */
export interface ListAlertsParams {
  status?: AlertStatus;
  /** Comma-separated severity values (0..5). Empty array means "any". */
  severity?: number[];
  /** Restrict to a single dongle. Empty/undefined means "any". */
  dongle_id?: string;
  limit?: number;
  offset?: number;
}

/**
 * listAlerts fetches the alert feed. The backend defaults status=open,
 * but the UI passes an explicit status so the URL deep-link round-trip
 * always reflects the active tab.
 */
export function listAlerts(params: ListAlertsParams = {}): Promise<AlertsListResponse> {
  const sp = new URLSearchParams();
  if (params.status) sp.set("status", params.status);
  if (params.severity && params.severity.length > 0) {
    sp.set("severity", params.severity.join(","));
  }
  if (params.dongle_id) sp.set("dongle_id", params.dongle_id);
  if (params.limit !== undefined) sp.set("limit", String(params.limit));
  if (params.offset !== undefined) sp.set("offset", String(params.offset));
  const qs = sp.toString();
  return apiFetch<AlertsListResponse>(
    qs ? `/v1/alpr/alerts?${qs}` : "/v1/alpr/alerts",
  );
}

/**
 * ackAlert marks the watchlist row as acknowledged. The hash is the
 * unpadded base64-url form returned in AlertItem.plate_hash_b64; we
 * pass it through verbatim. Idempotent on the server: a re-ack still
 * returns 200.
 */
export function ackAlert(plateHashB64: string): Promise<{ acked_at: string }> {
  // The plate hash is base64-url with no padding (matches the backend's
  // hashB64 helper). We do NOT URL-encode -- that would corrupt the '-'
  // and '_' characters that base64-url legitimately produces.
  return apiFetch<{ acked_at: string }>(
    `/v1/alpr/alerts/${plateHashB64}/ack`,
    { method: "POST" },
  );
}

/** unackAlert is the symmetric clear of ackAlert. Idempotent. */
export function unackAlert(plateHashB64: string): Promise<{ acked_at: null }> {
  return apiFetch<{ acked_at: null }>(
    `/v1/alpr/alerts/${plateHashB64}/unack`,
    { method: "POST" },
  );
}

/** Whitelist entry shape returned by GET /v1/alpr/whitelist. */
export interface WhitelistItem {
  plate_hash_b64: string;
  label?: string;
  plate: string;
  notes?: string;
  created_at?: string;
  updated_at?: string;
}

interface WhitelistListResponse {
  whitelist: WhitelistItem[];
}

/**
 * listWhitelist fetches the whitelist tab's data. The backend caps
 * limit at 100; the UI requests 100 and assumes the whitelist is small
 * enough to fit on one page (the spec calls out <100 expected).
 */
export function listWhitelist(limit = 100): Promise<WhitelistListResponse> {
  const sp = new URLSearchParams();
  sp.set("limit", String(limit));
  return apiFetch<WhitelistListResponse>(`/v1/alpr/whitelist?${sp.toString()}`);
}

/**
 * addWhitelist creates (or upserts) a whitelist entry. The plate text
 * is normalized server-side (uppercase + strip whitespace/dash/dot);
 * an empty post-normalization plate yields a 400 the caller surfaces
 * inline. label is optional.
 */
export function addWhitelist(
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

/** removeWhitelist deletes a whitelist row by hash. 404 when unknown. */
export function removeWhitelist(plateHashB64: string): Promise<{ removed: boolean }> {
  return apiFetch<{ removed: boolean }>(
    `/v1/alpr/whitelist/${plateHashB64}`,
    { method: "DELETE" },
  );
}
