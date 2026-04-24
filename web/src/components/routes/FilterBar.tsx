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
 *
 * starredOnly and tags layer on top of the existing filter set:
 *   - starredOnly is a two-state checkbox ("off" means no filter, "on"
 *     means ?starred=true). We intentionally do NOT surface ?starred=false
 *     here because "only my un-starred routes" is a rare view; the API
 *     still accepts it for callers that build querystrings directly.
 *   - tags is the AND-filter: every tag in the array must be attached to
 *     a route for it to appear. Values are stored lowercased to match the
 *     server-side normalization.
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
  starredOnly: boolean;
  tags: string[];
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
  starredOnly: false,
  tags: [],
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
    f.starredOnly === false &&
    f.tags.length === 0 &&
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

  // The Starred-only checkbox is on when and only when ?starred=true is
  // present. ?starred=false deep-links still work (the API honours them),
  // but the UI renders them as an un-checked checkbox because the
  // checkbox is a two-state control; the user can edit the URL by hand if
  // they really want the inverted view.
  const starredOnly = params.get("starred") === "true";

  // Tags come in as repeated ?tag=a&tag=b entries. Lowercase + trim mirrors
  // the server-side normalization so a pasted URL reconstructs exactly the
  // filter the backend will apply, and duplicate/empty entries are dropped.
  const tags = dedupeTags(params.getAll("tag"));

  return {
    from: isDateOnly(fromParam) ? fromParam : isoDateOnly(fromParam),
    to: isDateOnly(toParam) ? toParam : isoDateOnly(toParam),
    minDurationMin: minS === "" ? "" : String(Math.round(Number(minS) / 60)),
    maxDurationMin: maxS === "" ? "" : String(Math.round(Number(maxS) / 60)),
    minDistanceKm: minM === "" ? "" : roundKm(Number(minM) / 1000),
    maxDistanceKm: maxM === "" ? "" : roundKm(Number(maxM) / 1000),
    preserved: parseTriState(params.get("preserved")),
    hasEvents: parseTriState(params.get("has_events")),
    starredOnly,
    tags,
    sort,
  };
}

/**
 * normalizeTag lowercases + trims a tag value so the UI always stores the
 * canonical form. Mirrors the server-side normalization applied by the
 * annotations API and by the list-filter handler.
 */
export function normalizeTag(raw: string): string {
  return raw.trim().toLowerCase();
}

function dedupeTags(raw: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const r of raw) {
    const t = normalizeTag(r);
    if (t === "" || seen.has(t)) continue;
    seen.add(t);
    out.push(t);
  }
  return out;
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
  if (f.starredOnly) sp.set("starred", "true");
  // Tags are emitted as repeated ?tag=a&tag=b entries so the backend's
  // c.QueryParams()["tag"] slice picks them up as multiple values.
  for (const t of f.tags) {
    sp.append("tag", t);
  }
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
  /**
   * Available tags for the tag picker, populated by the parent from
   * GET /v1/devices/:dongle_id/tags. Matching is case-insensitive; the
   * parent is expected to pre-normalize so the picker and the filter
   * value agree on casing.
   */
  availableTags?: string[];
}

export function FilterBar({
  filters,
  onChange,
  onClear,
  availableTags = [],
}: FilterBarProps) {
  const patch = (p: Partial<FilterState>) => onChange({ ...filters, ...p });

  const onText =
    (key: keyof FilterState) => (e: ChangeEvent<HTMLInputElement>) =>
      patch({ [key]: e.target.value } as Partial<FilterState>);

  const onSelect =
    (key: keyof FilterState) => (e: ChangeEvent<HTMLSelectElement>) =>
      patch({ [key]: e.target.value } as Partial<FilterState>);

  const onStarredToggle = (e: ChangeEvent<HTMLInputElement>) =>
    patch({ starredOnly: e.target.checked });

  const addTag = (raw: string) => {
    const tag = normalizeTag(raw);
    if (!tag) return;
    if (filters.tags.includes(tag)) return;
    patch({ tags: [...filters.tags, tag] });
  };

  const removeTag = (tag: string) => {
    patch({ tags: filters.tags.filter((t) => t !== tag) });
  };

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
              <input
                id="filter-starred-only"
                type="checkbox"
                checked={filters.starredOnly}
                onChange={onStarredToggle}
                className="h-3.5 w-3.5 accent-[var(--accent)]"
              />
              <label htmlFor="filter-starred-only" className={LABEL_CLASS}>
                Starred only
              </label>
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

          {/* Tag multi-select */}
          <TagPicker
            selected={filters.tags}
            available={availableTags}
            onAdd={addTag}
            onRemove={removeTag}
          />
        </div>
      </CardBody>
    </Card>
  );
}

interface TagPickerProps {
  selected: string[];
  available: string[];
  onAdd: (tag: string) => void;
  onRemove: (tag: string) => void;
}

/**
 * TagPicker is the chip-picker used by FilterBar. Selected tags render as
 * removable chips; the select drop-down offers the remaining (not-yet-
 * selected) tags from the device's tag set. We intentionally keep this as
 * a plain <select> rather than a popup combobox: it is keyboard-friendly
 * by default, works without client-side state for accessibility, and
 * matches the styling of the other filter controls.
 *
 * When no tags are available the picker still renders (so the Clear
 * filters button can unstick a state where tags were set from the URL
 * but the autocomplete list has not arrived yet).
 */
function TagPicker({ selected, available, onAdd, onRemove }: TagPickerProps) {
  const selectedSet = new Set(selected.map(normalizeTag));
  const options = available
    .map(normalizeTag)
    .filter((t) => t !== "" && !selectedSet.has(t));

  const onSelectTag = (e: ChangeEvent<HTMLSelectElement>) => {
    const v = e.target.value;
    if (!v) return;
    onAdd(v);
    // Reset the drop-down back to its placeholder so the user can pick
    // another tag without first selecting the placeholder again.
    e.target.value = "";
  };

  return (
    <div className="flex flex-wrap items-center gap-2">
      <span className={LABEL_CLASS}>Tags</span>

      {selected.map((tag) => (
        <span
          key={tag}
          className="inline-flex items-center gap-1 rounded-full border border-[var(--border-secondary)] bg-[var(--bg-secondary)] px-2 py-0.5 text-xs text-[var(--text-primary)]"
        >
          <span>{tag}</span>
          <button
            type="button"
            aria-label={`Remove tag ${tag}`}
            onClick={() => onRemove(tag)}
            className="leading-none text-[var(--text-secondary)] hover:text-[var(--text-primary)] focus:outline-none"
          >
            &times;
          </button>
        </span>
      ))}

      <select
        aria-label="Add tag to filter"
        value=""
        onChange={onSelectTag}
        disabled={options.length === 0}
        className={INPUT_CLASS}
      >
        <option value="" disabled>
          {options.length === 0 ? "No tags available" : "Add tag"}
        </option>
        {options.map((t) => (
          <option key={t} value={t}>
            {t}
          </option>
        ))}
      </select>
    </div>
  );
}
