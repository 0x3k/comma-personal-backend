/**
 * Shared TypeScript types matching the Go backend API response shapes.
 */

/** A segment's upload status for each file type. */
export interface Segment {
  number: number;
  rlogUploaded: boolean;
  qlogUploaded: boolean;
  fcameraUploaded: boolean;
  ecameraUploaded: boolean;
  dcameraUploaded: boolean;
  qcameraUploaded: boolean;
}

/** Summary of a route as returned by the list endpoint. */
export interface RouteListItem {
  dongleId: string;
  routeName: string;
  startTime: string | null;
  endTime: string | null;
  /** Free-form user note, empty string when unset. */
  note: string;
  /** Starred / favourite flag; toggled via the annotations API. */
  starred: boolean;
  /** User-assigned tags, alphabetically sorted by the backend. */
  tags: string[];
  /** Preserved routes are exempt from retention-based cleanup. */
  preserved: boolean;
  segmentCount: number;
}

/** Response from GET /v1/devices/:dongle_id/tags -- list of distinct tags
 *  across a device's routes, used to populate the tag autocomplete. */
export interface DeviceTagsResponse {
  tags: string[];
}

/** Paginated response from GET /v1/route/:dongle_id */
export interface RouteListResponse {
  routes: RouteListItem[];
  total: number;
  limit: number;
  offset: number;
}

/** Full route detail with segments from GET /v1/route/:dongle_id/:route_name */
export interface RouteDetailResponse {
  dongleId: string;
  routeName: string;
  startTime: string | null;
  endTime: string | null;
  /** Preserved routes are exempt from retention-based cleanup. */
  preserved: boolean;
  /** Free-form user note, empty string when unset. */
  note: string;
  /** Starred / favourite flag; toggled via the annotations API. */
  starred: boolean;
  /** User-assigned tags, alphabetically sorted by the backend. */
  tags: string[];
  segmentCount: number;
  segments: Segment[];
  /** GPS track as an array of [lat, lng] coordinate pairs. May be null/empty. */
  geometry?: [number, number][] | null;
  /**
   * Per-vertex route-relative milliseconds, parallel to `geometry` (same length
   * when both are present). Used by the map to bisect by time so the live
   * "current car position" dot lingers at stops instead of jumping past them.
   * Omitted by the backend when the route's metadata predates the column or
   * the worker has not backfilled it yet -- callers should fall back to
   * time-fraction indexing in that case.
   */
  geometryTimes?: number[] | null;
}

/** A single device configuration parameter as returned by the config API. */
export interface DeviceParam {
  key: string;
  value: string;
}

/** Severity level for a log entry. */
export type LogSeverity = "info" | "warning" | "error";

/** A single parsed log entry for display in the log viewer. */
export interface LogEntry {
  /** Monotonic index for keying. */
  id: number;
  /** ISO timestamp or relative time string. */
  timestamp: string;
  /** Severity level. */
  severity: LogSeverity;
  /** The log message text. */
  message: string;
}

/** Per-device disk consumption as returned by GET /v1/storage/usage. */
export interface DeviceUsage {
  dongleId: string;
  bytes: number;
  routeCount: number;
}

/** Full disk usage report from GET /v1/storage/usage. */
export interface StorageUsageReport {
  devices: DeviceUsage[];
  totalBytes: number;
  filesystemTotalBytes: number;
  filesystemAvailableBytes: number;
  computedAt: string;
}

/** Response from GET/PUT /v1/settings/retention. */
export interface RetentionSetting {
  retention_days: number;
}

/** A segment as returned by the public GET /v1/share/:token endpoint.
 *  Intentionally trimmer than Segment -- rlog/qlog are not exposed.
 */
export interface ShareSegment {
  number: number;
  fcameraUploaded: boolean;
  ecameraUploaded: boolean;
  dcameraUploaded: boolean;
  qcameraUploaded: boolean;
}

/** Response from GET /v1/share/:token (public, token-gated). */
export interface ShareRouteResponse {
  routeName: string;
  startTime: string | null;
  endTime: string | null;
  segmentCount: number;
  segments: ShareSegment[];
  /** GPS track as an array of [lat, lng] coordinate pairs. */
  geometry: [number, number][] | null;
  /** ISO timestamp at which this share link stops being valid. */
  expiresAt: string;
  /** Base path for per-segment media, e.g. "/v1/share/<token>/segments". */
  mediaBaseUrl: string;
}

/** Response from POST /v1/routes/:dongle_id/:route_name/share. */
export interface CreateShareResponse {
  url: string;
  token: string;
  expires_at: string;
}


/**
 * A single aggregated trip row as returned by the stats and trip endpoints.
 * snake_case field names match the Go handler in internal/api/trip.go
 * (tripResponse) -- unlike most types in this file, these are NOT camelCase
 * because the stats endpoint follows the device-facing API surface.
 */
export interface Trip {
  id: number;
  dongle_id: string;
  route_id: number;
  route_name: string;
  start_time: string | null;
  distance_meters: number | null;
  duration_seconds: number | null;
  max_speed_mps: number | null;
  avg_speed_mps: number | null;
  engaged_seconds: number | null;
  start_address: string | null;
  end_address: string | null;
  start_lat: number | null;
  start_lng: number | null;
  end_lat: number | null;
  end_lng: number | null;
  computed_at: string | null;
}

/** Lifetime aggregate totals for a device. */
export interface DeviceStatsTotals {
  trip_count: number;
  total_distance_meters: number;
  total_duration_seconds: number;
  total_engaged_seconds: number;
}

/** Response from GET /v1/devices/:dongle_id/stats. */
export interface DeviceStats {
  totals: DeviceStatsTotals;
  recent: Trip[];
  limit: number;
  offset: number;
}

/**
 * A single detected event ("moment") as returned by
 * GET /v1/devices/:dongle_id/events. Field names are snake_case to match
 * the device-facing API surface in internal/api/events.go.
 *
 * `payload` is the raw JSONB blob the detector stored -- its shape depends
 * on the event type (e.g. hard_brake has `{ "deceleration_mps2": ... }`).
 * Consumers should feature-detect known keys rather than assume a schema.
 */
export interface MomentEvent {
  id: number;
  route_name: string;
  type: string;
  severity: string;
  route_offset_seconds: number;
  occurred_at: string | null;
  payload: unknown;
}

/** Paginated response from GET /v1/devices/:dongle_id/events. */
export interface MomentsListResponse {
  events: MomentEvent[];
  total: number;
  limit: number;
  offset: number;
}

/**
 * A crash record persisted by the Sentry envelope relay
 * (POST /api/:project_id/envelope/). Tags and exception are decoded JSONB
 * so the dashboard can drill into them without a second fetch.
 */
export interface CrashListItem {
  id: number;
  event_id: string;
  dongle_id?: string;
  level: string;
  message: string;
  tags: unknown;
  exception: unknown;
  occurred_at: string | null;
  received_at: string | null;
}

/** Paginated response from GET /v1/crashes. */
export interface CrashListResponse {
  crashes: CrashListItem[];
  total: number;
}

/** Full detail for one crash (GET /v1/crashes/:id). */
export interface CrashDetail {
  id: number;
  event_id: string;
  dongle_id?: string;
  level: string;
  message: string;
  fingerprint: unknown;
  tags: unknown;
  exception: unknown;
  breadcrumbs: unknown;
  raw_event: unknown;
  occurred_at: string | null;
  received_at: string | null;
}

/**
 * Kinds accepted by POST /v1/route/:dongle_id/:route_name/request_full_data.
 * Mirrors the constants in internal/api/route_data_request.go.
 */
export type RouteDataRequestKind = "full_video" | "full_logs" | "all";

/**
 * Server-side status of a route data request row. Matches the CHECK
 * constraint in sql/migrations/010_route_data_requests.up.sql and the
 * constants in internal/api/route_data_request.go.
 */
export type RouteDataRequestStatus =
  | "pending"
  | "dispatched"
  | "partial"
  | "complete"
  | "failed";

/**
 * A single route_data_requests row as returned by the JSON API. The
 * Go handler in internal/api/route_data_request.go (rowToResponse)
 * flattens pgtype.* nullables into either a JSON value or null, so
 * each nullable column becomes `T | null` here.
 */
export interface RouteDataRequest {
  id: number;
  routeId: number;
  requestedBy: string | null;
  requestedAt: string | null;
  kind: RouteDataRequestKind;
  status: RouteDataRequestStatus;
  dispatchedAt: string | null;
  completedAt: string | null;
  error: string | null;
  filesRequested: number;
}

/** Aggregate progress block returned by the GET endpoint. */
export interface RouteDataRequestProgress {
  filesRequested: number;
  filesUploaded: number;
  percent: number;
}

/** Per-segment upload state included in the GET endpoint response. */
export interface RouteDataRequestSegment {
  segmentNumber: number;
  fcameraUploaded: boolean;
  ecameraUploaded: boolean;
  dcameraUploaded: boolean;
  rlogUploaded: boolean;
}

/**
 * GET /v1/route/:dongle_id/:route_name/request_full_data/:request_id
 * response. Includes aggregate progress + per-segment upload state.
 */
export interface RouteDataRequestStatusResponse {
  request: RouteDataRequest;
  progress: RouteDataRequestProgress;
  segments: RouteDataRequestSegment[];
}

/**
 * POST /v1/route/:dongle_id/:route_name/request_full_data response.
 * Returns the persisted (or reused, when inside the idempotency window)
 * row; progress + segments are intentionally omitted so the UI uses the
 * GET endpoint for polling.
 */
export interface RouteDataRequestPostResponse {
  request: RouteDataRequest;
}
