/**
 * Formatting helpers shared across the UI.
 */

const BYTE_UNITS = ["B", "KB", "MB", "GB", "TB", "PB"] as const;

/**
 * Render a byte count as a human-readable string.
 *
 * Uses base-1024 (KB = 1024 B) and picks the largest unit that keeps the
 * mantissa below 1024. Values below 1 KB are shown as whole bytes.
 *
 * @example formatBytes(0)            // "0 B"
 * @example formatBytes(1023)         // "1023 B"
 * @example formatBytes(1024)         // "1.0 KB"
 * @example formatBytes(1536)         // "1.5 KB"
 * @example formatBytes(1_073_741_824) // "1.0 GB"
 */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes)) return "-";
  if (bytes < 0) return "-";
  if (bytes < 1024) return `${Math.round(bytes)} B`;

  let value = bytes;
  let unitIndex = 0;
  while (value >= 1024 && unitIndex < BYTE_UNITS.length - 1) {
    value /= 1024;
    unitIndex++;
  }
  // One decimal place matches common storage UIs (e.g. 12.3 GB).
  const formatted = value >= 100 ? value.toFixed(0) : value.toFixed(1);
  return `${formatted} ${BYTE_UNITS[unitIndex]}`;
}

const METERS_PER_MILE = 1609.344;
const METERS_PER_KILOMETER = 1000;

/** Unit system for distance formatting. */
export type DistanceUnit = "imperial" | "metric";

/**
 * Render a distance in meters as a human-readable string.
 *
 * Defaults to imperial (miles). For values under 0.1 of the chosen unit, shows
 * the smaller fractional value with one decimal place so short trips still
 * read meaningfully (e.g. "0.1 mi" rather than "0 mi").
 *
 * @example formatDistance(0)           // "0.0 mi"
 * @example formatDistance(1609.344)     // "1.0 mi"
 * @example formatDistance(19782.4, "imperial") // "12.3 mi"
 * @example formatDistance(12300, "metric")     // "12.3 km"
 */
export function formatDistance(
  meters: number | null | undefined,
  unit: DistanceUnit = "imperial",
): string {
  if (meters === null || meters === undefined || !Number.isFinite(meters)) {
    return "--";
  }
  const safe = Math.max(0, meters);
  if (unit === "metric") {
    const km = safe / METERS_PER_KILOMETER;
    return `${km.toFixed(1)} km`;
  }
  const mi = safe / METERS_PER_MILE;
  return `${mi.toFixed(1)} mi`;
}

/**
 * Render a duration in seconds as a compact human-readable string.
 *
 * The unit cascade adapts to magnitude so a short drive doesn't read as
 * "0:11" (mistakable for 11 seconds) and a multi-hour route doesn't bloat
 * into noise like "73m 24s". Sub-minute values keep their seconds, sub-hour
 * values keep their seconds when nonzero, and hour-scale values drop seconds
 * entirely because that precision is rarely useful at that range.
 *
 * @example formatDuration(0)      // "0s"
 * @example formatDuration(24)     // "24s"
 * @example formatDuration(60)     // "1m"
 * @example formatDuration(90)     // "1m 30s"
 * @example formatDuration(684)    // "11m 24s"
 * @example formatDuration(3600)   // "1h"
 * @example formatDuration(5400)   // "1h 30m"
 */
export function formatDuration(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) {
    return "--";
  }
  const total = Math.max(0, Math.floor(seconds));
  if (total < 60) return `${total}s`;
  if (total < 3600) {
    const m = Math.floor(total / 60);
    const s = total % 60;
    return s === 0 ? `${m}m` : `${m}m ${s}s`;
  }
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  return m === 0 ? `${h}h` : `${h}h ${m}m`;
}

/**
 * Convenience wrapper around formatDuration for callers that hold ISO date
 * strings rather than a precomputed seconds delta. Returns "--" when either
 * end of the range is missing or the range is non-positive.
 */
export function formatDurationBetween(
  start: string | null | undefined,
  end: string | null | undefined,
): string {
  if (!start || !end) return "--";
  const ms = new Date(end).getTime() - new Date(start).getTime();
  if (!Number.isFinite(ms) || ms <= 0) return "--";
  return formatDuration(Math.floor(ms / 1000));
}

/**
 * Render a longer total-time figure (e.g. lifetime drive time) as
 * "H hr M min" or "M min" depending on magnitude.
 *
 * @example formatTotalDuration(0)     // "0 min"
 * @example formatTotalDuration(45)    // "0 min"
 * @example formatTotalDuration(60)    // "1 min"
 * @example formatTotalDuration(3600)  // "1 hr 0 min"
 * @example formatTotalDuration(3725)  // "1 hr 2 min"
 */
export function formatTotalDuration(
  seconds: number | null | undefined,
): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) {
    return "--";
  }
  const safe = Math.max(0, Math.floor(seconds));
  const hours = Math.floor(safe / 3600);
  const minutes = Math.floor((safe % 3600) / 60);
  if (hours === 0) {
    return `${minutes} min`;
  }
  return `${hours} hr ${minutes} min`;
}

/**
 * Compute the engagement percentage from engaged and total driving seconds.
 *
 * Returns "--" when there's no drive time to divide by, to avoid showing
 * "NaN%" on a fresh install.
 */
export function formatEngagementPct(
  engagedSeconds: number | null | undefined,
  totalSeconds: number | null | undefined,
): string {
  if (
    engagedSeconds === null ||
    engagedSeconds === undefined ||
    totalSeconds === null ||
    totalSeconds === undefined ||
    !Number.isFinite(engagedSeconds) ||
    !Number.isFinite(totalSeconds) ||
    totalSeconds <= 0
  ) {
    return "--";
  }
  const pct = Math.max(0, Math.min(100, (engagedSeconds / totalSeconds) * 100));
  return `${pct.toFixed(0)}%`;
}
