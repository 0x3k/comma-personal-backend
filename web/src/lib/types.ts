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
  segmentCount: number;
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
  segmentCount: number;
  segments: Segment[];
  /** GPS track as an array of [lat, lng] coordinate pairs. May be null/empty. */
  geometry?: [number, number][] | null;
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
