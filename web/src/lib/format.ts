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
 * Render a duration in seconds as a compact HH:MM string.
 *
 * Used for individual drive durations on the dashboard. Anything under an hour
 * still includes a leading "0:" so the column stays visually aligned.
 *
 * @example formatDurationHM(0)      // "0:00"
 * @example formatDurationHM(59)     // "0:00"
 * @example formatDurationHM(60)     // "0:01"
 * @example formatDurationHM(3600)   // "1:00"
 * @example formatDurationHM(3725)   // "1:02"
 */
export function formatDurationHM(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) {
    return "--";
  }
  const safe = Math.max(0, Math.floor(seconds));
  const hours = Math.floor(safe / 3600);
  const minutes = Math.floor((safe % 3600) / 60);
  return `${hours}:${minutes.toString().padStart(2, "0")}`;
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
