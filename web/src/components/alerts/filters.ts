/**
 * Filter state for the alerts feed and the URL-querystring round-trip
 * helpers. Mirrors the FilterBar pattern used by the routes list -- the
 * filter type is a plain interface (so the React state hook can spread
 * and patch), and the encode/decode helpers are pure functions so they
 * can be unit-tested without rendering.
 *
 * The filter set is intentionally minimal: severity, dongle_id, and a
 * date range. The backend listing endpoint does not support a date
 * filter directly, so the client filters in-memory by last_alert_at;
 * this keeps the wire shape stable while still giving the user a
 * "what alerted in the last week" view.
 */

import type { AlertStatus } from "./api";

/** Severity values the heuristic emits. 0/null means "no severity". */
export const SEVERITY_VALUES = [0, 1, 2, 3, 4, 5] as const;
export type Severity = (typeof SEVERITY_VALUES)[number];

/**
 * AlertsFilterState holds the user-facing filter values for the
 * alerts list. severity is a sorted unique array because the URL
 * round-trip preserves order; from/to are YYYY-MM-DD strings (the
 * format <input type="date"> emits) -- empty string means "no
 * filter" so they round-trip cleanly through URLSearchParams.
 */
export interface AlertsFilterState {
  severity: Severity[];
  dongle_id: string;
  from: string;
  to: string;
}

export const EMPTY_ALERTS_FILTERS: AlertsFilterState = {
  severity: [],
  dongle_id: "",
  from: "",
  to: "",
};

/**
 * isDefaultAlertsFilters returns true when nothing is filtered. Used
 * to decide whether to show a "Clear filters" button.
 */
export function isDefaultAlertsFilters(f: AlertsFilterState): boolean {
  return (
    f.severity.length === 0 &&
    f.dongle_id === "" &&
    f.from === "" &&
    f.to === ""
  );
}

/** Tab values surfaced in the URL as ?tab=. */
export const TAB_VALUES = ["open", "acked", "whitelist"] as const;
export type AlertsTab = (typeof TAB_VALUES)[number];

/**
 * tabToStatus maps the visible tab onto the API's status filter. The
 * whitelist tab uses the whitelist endpoint, not the alerts endpoint,
 * so it is not part of this mapping; callers branch on tab===whitelist
 * upstream of this helper.
 */
export function tabToStatus(tab: AlertsTab): AlertStatus | null {
  switch (tab) {
    case "open":
      return "open";
    case "acked":
      return "acked";
    case "whitelist":
      return null;
  }
}

/**
 * parseTab safely turns a raw string into an AlertsTab, falling back
 * to "open" so a typo in the URL still renders a usable view rather
 * than breaking with an invalid-state error.
 */
export function parseTab(raw: string | null): AlertsTab {
  if (raw === "acked" || raw === "whitelist") return raw;
  return "open";
}

const SEV_SET = new Set<number>(SEVERITY_VALUES);

function parseSeverityList(raw: string | null): Severity[] {
  if (!raw) return [];
  const out: number[] = [];
  for (const tok of raw.split(",")) {
    const n = parseInt(tok.trim(), 10);
    if (Number.isFinite(n) && SEV_SET.has(n) && !out.includes(n)) {
      out.push(n);
    }
  }
  out.sort((a, b) => a - b);
  return out as Severity[];
}

function isDateOnly(s: string): boolean {
  return /^\d{4}-\d{2}-\d{2}$/.test(s);
}

/**
 * filtersFromSearchParams reconstructs AlertsFilterState from a
 * URLSearchParams. Unknown / malformed values fall back to the empty
 * default so a hand-edited URL never throws.
 */
export function filtersFromSearchParams(
  params: URLSearchParams,
): AlertsFilterState {
  const severity = parseSeverityList(params.get("severity"));
  const dongleId = params.get("dongle_id") ?? "";
  const fromRaw = params.get("from") ?? "";
  const toRaw = params.get("to") ?? "";
  return {
    severity,
    dongle_id: dongleId,
    from: isDateOnly(fromRaw) ? fromRaw : "",
    to: isDateOnly(toRaw) ? toRaw : "",
  };
}

/**
 * filtersToSearchParams serializes AlertsFilterState back into a
 * URLSearchParams. Empty/default fields are omitted so a clean state
 * yields an empty query string. Severity is encoded as a single
 * comma-separated value to match the backend's parseSeverityFilter.
 */
export function filtersToSearchParams(
  f: AlertsFilterState,
): URLSearchParams {
  const sp = new URLSearchParams();
  if (f.severity.length > 0) sp.set("severity", f.severity.join(","));
  if (f.dongle_id) sp.set("dongle_id", f.dongle_id);
  if (f.from) sp.set("from", f.from);
  if (f.to) sp.set("to", f.to);
  return sp;
}

/**
 * Build the full querystring for an alerts page URL: filters + tab.
 * Centralised so the page and tests agree on key ordering and
 * default-omission rules.
 */
export function alertsQueryString(
  tab: AlertsTab,
  filters: AlertsFilterState,
): string {
  const sp = filtersToSearchParams(filters);
  if (tab !== "open") sp.set("tab", tab);
  return sp.toString();
}

/**
 * alertWithinDateRange filters an alert against the from/to bounds.
 * The bounds are inclusive of the start day and exclusive of the day
 * AFTER the end day -- "to=2026-04-25" means "through end of 2026-04-25".
 *
 * An alert with a null last_alert_at is dropped from filtered views
 * (a row in this state has never alerted, so it is not relevant to a
 * "what alerted between X and Y" query).
 */
export function alertWithinDateRange(
  lastAlertAt: string | null,
  from: string,
  to: string,
): boolean {
  if (!from && !to) return true;
  if (!lastAlertAt) return false;
  const t = Date.parse(lastAlertAt);
  if (!Number.isFinite(t)) return false;
  if (from) {
    const fromMs = Date.parse(`${from}T00:00:00Z`);
    if (Number.isFinite(fromMs) && t < fromMs) return false;
  }
  if (to) {
    // Exclusive upper bound: add one day so "to=YYYY-MM-DD" is
    // inclusive of the entire day in the user's mental model.
    const toMs = Date.parse(`${to}T00:00:00Z`);
    if (Number.isFinite(toMs) && t >= toMs + 24 * 60 * 60 * 1000) return false;
  }
  return true;
}
