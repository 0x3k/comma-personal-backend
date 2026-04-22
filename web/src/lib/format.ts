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
