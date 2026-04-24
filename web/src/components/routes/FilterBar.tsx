"use client";

import { type ChangeEvent } from "react";
import { Button } from "@/components/ui/Button";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";

/**
 * RouteSortKey mirrors the sort values accepted by
 * GET /v1/route/:dongle_id on the backend. See validSortKeys in
 * internal/api/route.go.
 */
export type RouteSortKey =
  | "date_desc"
  | "date_asc"
  | "duration_desc"
  | "distance_desc";

/**
 * TriState is the shape used for the Preserved and Has-events toggles.
 *
 * "" means "any" (do not send the query param); "true" and "false" map
 * directly to the backend's preserved/has_events boolean filters.
 */
export type TriState = "" | "true" | "false";

/**
 * FilterState holds the user-facing filter values for the routes list.
 *
 * All numeric fields are strings so empty inputs can round-trip through
 * URLSearchParams without being coerced to zero. Durations live in minutes
 * and distances in kilometres because that is what users type; the parent
 * page converts to the seconds/metres the API expects.
 */
export interface FilterState {
  from: string;
  to: string;
  minDurationMin: string;
  maxDurationMin: string;
  minDistanceKm: string;
  maxDistanceKm: string;
  preserved: TriState;
  hasEvents: TriState;
  sort: RouteSortKey;
}

export const DEFAULT_SORT: RouteSortKey = "date_desc";

export const EMPTY_FILTERS: FilterState = {
  from: "",
  to: "",
  minDurationMin: "",
  maxDurationMin: "",
  minDistanceKm: "",
  maxDistanceKm: "",
  preserved: "",
  hasEvents: "",
  sort: DEFAULT_SORT,
};

/**
 * isDefaultFilters returns true when the filter state matches EMPTY_FILTERS.
 * The parent uses this to decide when to render the empty-state Clear CTA
 * and when to omit filter keys from the URL.
 */
export function isDefaultFilters(f: FilterState): boolean {
  return (
    f.from === "" &&
    f.to === "" &&
    f.minDurationMin === "" &&
    f.maxDurationMin === "" &&
    f.minDistanceKm === "" &&
    f.maxDistanceKm === "" &&
    f.preserved === "" &&
    f.hasEvents === "" &&
    f.sort === DEFAULT_SORT
  );
}

const VALID_SORTS: readonly RouteSortKey[] = [
  "date_desc",
  "date_asc",
  "duration_desc",
  "distance_desc",
];

function parseTriState(v: string | null): TriState {
  return v === "true" || v === "false" ? v : "";
}

/** Convert a digit-only string back to a clean numeric string, dropping junk. */
function sanitizeNumber(v: string | null, allowDecimal: boolean): string {
  if (!v) return "";
  const n = Number(v);
  if (!Number.isFinite(n) || n < 0) return "";
  return allowDecimal ? String(n) : String(Math.trunc(n));
}

/**
 * filtersFromSearchParams reconstructs FilterState from a URLSearchParams,
 * using the API-native query-param names (from, to, min_duration_s, etc.).
 * This lets the routes page hydrate from a pasted URL and share the same
 * wire format the backend accepts -- so a user can copy
 * /routes?from=...&sort=... and a fresh tab sees the same view.
 *
 * Durations and distances are stored on the wire in the API-native units
 * (seconds / metres) and converted to minutes / kilometres for the UI.
 * Unknown or malformed params fall back to their default so we never throw
 * on untrusted input.
 */
export function filtersFromSearchParams(params: URLSearchParams): FilterState {
  const fromParam = params.get("from") ?? "";
  const toParam = params.get("to") ?? "";

  const minS = sanitizeNumber(params.get("min_duration_s"), false);
  const maxS = sanitizeNumber(params.get("max_duration_s"), false);
  const minM = sanitizeNumber(params.get("min_distance_m"), true);
  const maxM = sanitizeNumber(params.get("max_distance_m"), true);

  const sortRaw = params.get("sort") ?? "";
  const sort = (VALID_SORTS as readonly string[]).includes(sortRaw)
    ? (sortRaw as RouteSortKey)
    : DEFAULT_SORT;

  return {
    from: isDateOnly(fromParam) ? fromParam : isoDateOnly(fromParam),
    to: isDateOnly(toParam) ? toParam : isoDateOnly(toParam),
    minDurationMin: minS === "" ? "" : String(Math.round(Number(minS) / 60)),
    maxDurationMin: maxS === "" ? "" : String(Math.round(Number(maxS) / 60)),
    minDistanceKm: minM === "" ? "" : roundKm(Number(minM) / 1000),
    maxDistanceKm: maxM === "" ? "" : roundKm(Number(maxM) / 1000),
    preserved: parseTriState(params.get("preserved")),
    hasEvents: parseTriState(params.get("has_events")),
    sort,
  };
}

function roundKm(km: number): string {
  // Round to 0.1 km for display; the user typed "12.3" originally so
  // preserving that shape matters more than micro-precision.
  return (Math.round(km * 10) / 10).toString();
}

function isDateOnly(s: string): boolean {
  return /^\d{4}-\d{2}-\d{2}$/.test(s);
}

// isoDateOnly trims a full ISO/RFC3339 string down to the YYYY-MM-DD prefix
// so pasted deep links that carry a time round-trip into the <input
// type="date"> control without confusing it.
function isoDateOnly(s: string): string {
  if (!s) return "";
  const m = s.match(/^(\d{4}-\d{2}-\d{2})/);
  return m ? m[1] : "";
}

/**
 * filtersToSearchParams serializes FilterState into the wire format the
 * backend expects on GET /v1/route/:dongle_id, skipping any keys whose
 * value is empty/default. The output is reused for both the browser URL
 * (so reload + copy-paste restore the view) and the backend query string.
 *
 * Dates are normalized to start-of-day / end-of-day RFC3339 timestamps
 * because the API uses r.start_time >= from and r.start_time < to; leaving
 * them as bare "YYYY-MM-DD" would be rejected by the strict RFC3339 parser
 * in internal/api/route.go.
 */
export function filtersToSearchParams(f: FilterState): URLSearchParams {
  const sp = new URLSearchParams();
  if (f.from) {
    const iso = dateOnlyToRFC3339(f.from, false);
    if (iso) sp.set("from", iso);
  }
  if (f.to) {
    // Add one day so that "to=YYYY-MM-DD" means "through the end of that
    // day" from the user's perspective -- the backend uses a strict < cmp.
    const iso = dateOnlyToRFC3339(f.to, true);
    if (iso) sp.set("to", iso);
  }
  if (f.minDurationMin !== "") {
    const n = Number(f.minDurationMin);
    if (Number.isFinite(n) && n >= 0) {
      sp.set("min_duration_s", String(Math.round(n * 60)));
    }
  }
  if (f.maxDurationMin !== "") {
    const n = Number(f.maxDurationMin);
    if (Number.isFinite(n) && n >= 0) {
      sp.set("max_duration_s", String(Math.round(n * 60)));
    }
  }
  if (f.minDistanceKm !== "") {
    const n = Number(f.minDistanceKm);
    if (Number.isFinite(n) && n >= 0) {
      sp.set("min_distance_m", String(Math.round(n * 1000)));
    }
  }
  if (f.maxDistanceKm !== "") {
    const n = Number(f.maxDistanceKm);
    if (Number.isFinite(n) && n >= 0) {
      sp.set("max_distance_m", String(Math.round(n * 1000)));
    }
  }
  if (f.preserved !== "") sp.set("preserved", f.preserved);
  if (f.hasEvents !== "") sp.set("has_events", f.hasEvents);
  if (f.sort !== DEFAULT_SORT) sp.set("sort", f.sort);
  return sp;
}

function dateOnlyToRFC3339(ymd: string, endOfDay: boolean): string {
  if (!isDateOnly(ymd)) return "";
  // Midnight UTC keeps the filter boundary deterministic across time zones.
  // Adding a day for "to" mirrors the half-open interval the API uses.
  const base = new Date(`${ymd}T00:00:00Z`);
  if (Number.isNaN(base.getTime())) return "";
  if (endOfDay) base.setUTCDate(base.getUTCDate() + 1);
  return base.toISOString();
}

const INPUT_CLASS =
  "rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-2 py-1 text-xs text-[var(--text-primary)] focus:border-[var(--accent)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]";

const LABEL_CLASS = "text-xs text-[var(--text-secondary)]";

interface FilterBarProps {
  filters: FilterState;
  onChange: (next: FilterState) => void;
  onClear: () => void;
}

export function FilterBar({ filters, onChange, onClear }: FilterBarProps) {
  const patch = (p: Partial<FilterState>) => onChange({ ...filters, ...p });

  const onText =
    (key: keyof FilterState) => (e: ChangeEvent<HTMLInputElement>) =>
      patch({ [key]: e.target.value } as Partial<FilterState>);

  const onSelect =
    (key: keyof FilterState) => (e: ChangeEvent<HTMLSelectElement>) =>
      patch({ [key]: e.target.value } as Partial<FilterState>);

  const hasAnyFilter = !isDefaultFilters(filters);

  return (
    <Card className="mb-4">
      <CardHeader>
        <div className="flex items-center justify-between gap-2">
          <h2 className="text-sm font-medium text-[var(--text-primary)]">
            Filters
          </h2>
          <Button
            variant="ghost"
            size="sm"
            onClick={onClear}
            disabled={!hasAnyFilter}
          >
            Clear filters
          </Button>
        </div>
      </CardHeader>
      <CardBody>
        <div className="flex flex-col gap-3">
          {/* Date range */}
          <div className="flex flex-wrap items-center gap-3">
            <div className="flex items-center gap-2">
              <label htmlFor="filter-from" className={LABEL_CLASS}>
                From
              </label>
              <input
                id="filter-from"
                type="date"
                value={filters.from}
                onChange={onText("from")}
                className={INPUT_CLASS}
              />
            </div>
            <div className="flex items-center gap-2">
              <label htmlFor="filter-to" className={LABEL_CLASS}>
                To
              </label>
              <input
                id="filter-to"
                type="date"
                value={filters.to}
                onChange={onText("to")}
                className={INPUT_CLASS}
              />
            </div>
          </div>

          {/* Duration range */}
          <div className="flex flex-wrap items-center gap-3">
            <span className={LABEL_CLASS}>Duration (min)</span>
            <div className="flex items-center gap-1">
              <label htmlFor="filter-min-duration" className="sr-only">
                Minimum duration in minutes
              </label>
              <input
                id="filter-min-duration"
                type="number"
                inputMode="numeric"
                min={0}
                step={1}
                placeholder="min"
                value={filters.minDurationMin}
                onChange={onText("minDurationMin")}
                className={`${INPUT_CLASS} w-20`}
              />
              <span className={LABEL_CLASS}>&ndash;</span>
              <label htmlFor="filter-max-duration" className="sr-only">
                Maximum duration in minutes
              </label>
              <input
                id="filter-max-duration"
                type="number"
                inputMode="numeric"
                min={0}
                step={1}
                placeholder="max"
                value={filters.maxDurationMin}
                onChange={onText("maxDurationMin")}
                className={`${INPUT_CLASS} w-20`}
              />
            </div>

            {/* Distance range */}
            <span className={LABEL_CLASS}>Distance (km)</span>
            <div className="flex items-center gap-1">
              <label htmlFor="filter-min-distance" className="sr-only">
                Minimum distance in kilometres
              </label>
              <input
                id="filter-min-distance"
                type="number"
                inputMode="decimal"
                min={0}
                step="0.1"
                placeholder="min"
                value={filters.minDistanceKm}
                onChange={onText("minDistanceKm")}
                className={`${INPUT_CLASS} w-20`}
              />
              <span className={LABEL_CLASS}>&ndash;</span>
              <label htmlFor="filter-max-distance" className="sr-only">
                Maximum distance in kilometres
              </label>
              <input
                id="filter-max-distance"
                type="number"
                inputMode="decimal"
                min={0}
                step="0.1"
                placeholder="max"
                value={filters.maxDistanceKm}
                onChange={onText("maxDistanceKm")}
                className={`${INPUT_CLASS} w-20`}
              />
            </div>
          </div>

          {/* Toggles + sort */}
          <div className="flex flex-wrap items-center gap-3">
            <div className="flex items-center gap-2">
              <label htmlFor="filter-preserved" className={LABEL_CLASS}>
                Preserved
              </label>
              <select
                id="filter-preserved"
                value={filters.preserved}
                onChange={onSelect("preserved")}
                className={INPUT_CLASS}
              >
                <option value="">Any</option>
                <option value="true">Yes</option>
                <option value="false">No</option>
              </select>
            </div>
            <div className="flex items-center gap-2">
              <label htmlFor="filter-has-events" className={LABEL_CLASS}>
                Has events
              </label>
              <select
                id="filter-has-events"
                value={filters.hasEvents}
                onChange={onSelect("hasEvents")}
                className={INPUT_CLASS}
              >
                <option value="">Any</option>
                <option value="true">Yes</option>
                <option value="false">No</option>
              </select>
            </div>
            <div className="flex items-center gap-2">
              <label htmlFor="filter-sort" className={LABEL_CLASS}>
                Sort
              </label>
              <select
                id="filter-sort"
                value={filters.sort}
                onChange={onSelect("sort")}
                className={INPUT_CLASS}
              >
                <option value="date_desc">Newest first</option>
                <option value="date_asc">Oldest first</option>
                <option value="duration_desc">Longest first</option>
                <option value="distance_desc">Furthest first</option>
              </select>
            </div>
          </div>
        </div>
      </CardBody>
    </Card>
  );
}
