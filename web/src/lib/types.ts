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
