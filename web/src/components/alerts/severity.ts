/**
 * Severity classification shared between the alerts page and any
 * other consumer that wants to render a severity badge identically.
 *
 * The bucketing matches PlateTimeline.tsx -- gray for sev 0/1/null,
 * amber for 2-3, red for 4-5 -- so the alert list and the per-route
 * scrubber agree on color at a glance.
 */

export type SeverityBucket = "none" | "amber" | "red";

/**
 * classifyAlertSeverity returns the bucket for an alert's severity.
 * Matches the PlateTimeline classifier; pulled out here so the alert
 * center can color-code rows without importing from a deeply nested
 * video component.
 */
export function classifyAlertSeverity(sev: number | null | undefined): SeverityBucket {
  if (sev == null) return "none";
  if (sev <= 1) return "none";
  if (sev <= 3) return "amber";
  return "red";
}

/**
 * Inline color tokens, matching PlateTimeline. Spelled as CSS variable
 * references so a theme override propagates without re-declaring
 * anything here.
 */
export const SEVERITY_COLOR: Record<SeverityBucket, string> = {
  none: "var(--color-neutral-500)",
  amber: "var(--color-warning-500)",
  red: "var(--color-danger-500)",
};

/** Friendly label for the severity badge text. */
export function severityLabel(sev: number | null | undefined): string {
  if (sev == null) return "—";
  return `Sev ${sev}`;
}
