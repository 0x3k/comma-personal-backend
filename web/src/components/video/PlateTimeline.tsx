"use client";

import { useEffect, useMemo, useState } from "react";
import { apiFetch } from "@/lib/api";
import { useAlprSettings } from "@/lib/useAlprSettings";

/**
 * One element of the {encounters: [...]} payload returned by GET
 * /v1/routes/:dongle_id/:route_name/plates. Mirrors the wire shape so
 * the component can be fed straight from apiFetch without an adapter.
 */
export interface PlateEncounter {
  plate: string;
  plate_hash_b64: string;
  first_seen_ts: string;
  last_seen_ts: string;
  detection_count: number;
  turn_count: number;
  signature: {
    make?: string;
    model?: string;
    color?: string;
    body_type?: string;
    confidence?: number;
  } | null;
  severity_if_alerted: number | null;
  ack_status: "open" | "acked" | null;
  bbox_first: { x: number; y: number; w: number; h: number } | null;
  sample_thumb_url: string | null;
}

interface PlatesResponse {
  encounters: PlateEncounter[];
}

/**
 * Severity classification used both to color a segment and to decide
 * whether to surface an alert badge in the scrubber strip. Sev 1 is
 * unused by the heuristic but treated as gray (none) so a future
 * heuristic that emits sev 1 doesn't accidentally flag every plate.
 */
type Severity = "none" | "amber" | "red";

function classifySeverity(sev: number | null): Severity {
  if (sev == null) return "none";
  if (sev <= 1) return "none";
  if (sev <= 3) return "amber";
  return "red";
}

/**
 * Inline tooltip color tokens. We do not introduce new palette values
 * -- these map onto the same semantic tokens the Badge component uses
 * (success/warning/danger). Spelled out as inline rgba so the SVG
 * fallback strip can use them too without an extra Tailwind class.
 */
const COLOR_BY_SEVERITY: Record<Severity, string> = {
  none: "var(--color-neutral-500)",
  amber: "var(--color-warning-500)",
  red: "var(--color-danger-500)",
};

interface PlateTimelineProps {
  /** Device dongle id (route identifier, used to fetch encounters). */
  dongleId: string;
  /** Route name (e.g. YYYY-MM-DD--HH-MM-SS). Unencoded -- the component encodes. */
  routeName: string;
  /**
   * Route start timestamp (ISO8601). Used to convert each encounter's
   * absolute first_seen_ts/last_seen_ts into a route-relative offset
   * for positioning on the shared time axis.
   */
  routeStartTs: string | null;
  /**
   * Total duration of the route in seconds, used as the right edge
   * of the time axis so this rail aligns with the SignalTimeline
   * sitting above it.
   */
  routeDurationSec: number;
  /**
   * Called with a route-relative time (seconds) when the user clicks
   * an encounter. The parent maps that into a segment + segment-local
   * seek on the player handle, the same hook SignalTimeline uses.
   */
  onSeek: (routeRelativeSec: number) => void;
  /** Additional CSS classes for the wrapper. */
  className?: string;
}

/** Heights of the two stacked strips this component renders. */
const TRACK_HEIGHT = 14;
const SCRUBBER_HEIGHT = 12;
const STRIP_GAP = 4;

/**
 * PlateTimeline is a sibling to SignalTimeline. It renders one
 * horizontal rail per route showing every plate encounter, color-
 * coded by watchlist severity. Beneath it sits a scrubber strip that
 * surfaces only the severity > 0 encounters as alert-triangle
 * markers, so a user can spot alerted plates without scrolling the
 * timeline.
 *
 * The component is gated by the runtime alpr_enabled flag (read via
 * useAlprSettings) and renders nothing when the flag is off or while
 * the flag is loading -- avoiding a layout flicker on first load is
 * preferable to a one-frame placeholder.
 *
 * The empty state (zero encounters on this route) also renders
 * nothing, on the principle that visual noise is worse than absence.
 */
export function PlateTimeline({
  dongleId,
  routeName,
  routeStartTs,
  routeDurationSec,
  onSeek,
  className = "",
}: PlateTimelineProps) {
  const { enabled } = useAlprSettings();
  const [encounters, setEncounters] = useState<PlateEncounter[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [hoverIndex, setHoverIndex] = useState<number | null>(null);

  // --- Fetch encounters -----------------------------------------------
  // Single call per route; refetch only when dongle/route changes. We
  // skip the fetch entirely while the alpr flag is loading or off so
  // disabled deployments don't hit a 503 on every route open.
  useEffect(() => {
    if (enabled !== true) {
      setEncounters(null);
      return;
    }
    let cancelled = false;
    setError(null);
    apiFetch<PlatesResponse>(
      `/v1/routes/${dongleId}/${encodeURIComponent(routeName)}/plates`,
    )
      .then((data) => {
        if (cancelled) return;
        setEncounters(data.encounters ?? []);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        // Backend returns 503 alpr_disabled when the flag is off;
        // treat that the same as "no data" so we don't surface a
        // banner the operator already knows about (they just toggled
        // the flag).
        const msg = err instanceof Error ? err.message : "failed to load plates";
        if (msg === "alpr_disabled") {
          setEncounters([]);
          return;
        }
        setError(msg);
      });
    return () => {
      cancelled = true;
    };
  }, [enabled, dongleId, routeName]);

  // --- Derive positioned segments -------------------------------------
  // Each encounter becomes a {leftPct, widthPct, severity, ...}
  // descriptor positioned along the same 0..routeDurationSec axis as
  // SignalTimeline above. Encounters whose timestamps fall outside
  // the route window are skipped (rare; usually a clock-skew artifact
  // on the device side).
  const positioned = useMemo(() => {
    if (!encounters || encounters.length === 0) return [];
    if (!routeStartTs || routeDurationSec <= 0) return [];
    const t0 = Date.parse(routeStartTs);
    if (!Number.isFinite(t0)) return [];
    const out: {
      idx: number;
      leftPct: number;
      widthPct: number;
      firstSeenSec: number;
      severity: Severity;
      enc: PlateEncounter;
    }[] = [];
    for (let i = 0; i < encounters.length; i++) {
      const e = encounters[i];
      const firstMs = Date.parse(e.first_seen_ts);
      const lastMs = Date.parse(e.last_seen_ts);
      if (!Number.isFinite(firstMs) || !Number.isFinite(lastMs)) continue;
      const firstSec = (firstMs - t0) / 1000;
      const lastSec = (lastMs - t0) / 1000;
      // Clamp to the route window. An encounter that ends before the
      // route start (or starts after it ends) is skipped entirely.
      if (lastSec < 0 || firstSec > routeDurationSec) continue;
      const startSec = Math.max(0, firstSec);
      const endSec = Math.min(routeDurationSec, lastSec);
      const leftPct = (startSec / routeDurationSec) * 100;
      // A momentary sighting (first_seen == last_seen) gets a
      // minimum-width nub so it stays click-targetable.
      const widthPct = Math.max(
        0.5,
        ((endSec - startSec) / routeDurationSec) * 100,
      );
      out.push({
        idx: i,
        leftPct,
        widthPct,
        firstSeenSec: Math.max(0, firstSec),
        severity: classifySeverity(e.severity_if_alerted),
        enc: e,
      });
    }
    return out;
  }, [encounters, routeStartTs, routeDurationSec]);

  // Severity > 0 markers for the scrubber strip. Same firstSeen-based
  // x position as the main rail so a glance shows where the alert is
  // along the route.
  const alertMarkers = useMemo(
    () => positioned.filter((p) => p.severity !== "none"),
    [positioned],
  );

  // --- Gate rendering --------------------------------------------------
  // Feature flag off (or still loading the flag): render nothing. We
  // do not reserve UI space because the dashboard should not flicker
  // a placeholder rail on every route open.
  if (enabled !== true) return null;
  // Empty state (zero encounters, or backend short-circuited the
  // fetch): render nothing for the same reason.
  if (!encounters || encounters.length === 0 || positioned.length === 0) {
    return null;
  }

  const hovered = hoverIndex != null ? positioned[hoverIndex] : null;

  return (
    <div
      className={["w-full select-none", className].filter(Boolean).join(" ")}
      data-testid="plate-timeline"
    >
      {error && (
        <div className="mb-1 text-xs text-[var(--text-secondary)]">
          {error}
        </div>
      )}

      {/* Track label sits to the left of the rail so the operator
          can identify what the strip represents without hovering. */}
      <div className="mb-1 flex items-center justify-between text-xs text-[var(--text-secondary)]">
        <span>Plate sightings ({encounters.length})</span>
        {alertMarkers.length > 0 && (
          <span className="text-[var(--color-danger-500)]">
            {alertMarkers.length} alerted
          </span>
        )}
      </div>

      {/* Main rail: one colored segment per encounter, positioned by
          first_seen/last_seen. Wrapped in `relative` so the absolutely
          positioned segment children stack against this box. */}
      <div
        className="relative w-full rounded-md bg-[var(--bg-tertiary)]"
        style={{ height: TRACK_HEIGHT }}
        role="list"
        aria-label="Plate encounters"
      >
        {positioned.map((p) => {
          const color = COLOR_BY_SEVERITY[p.severity];
          const label = encounterAriaLabel(p.enc, p.severity);
          return (
            <button
              key={`${p.enc.plate_hash_b64}-${p.idx}`}
              type="button"
              role="listitem"
              aria-label={label}
              data-testid={`plate-segment-${p.idx}`}
              data-severity={p.severity}
              className="absolute top-0 h-full cursor-pointer rounded-sm focus:outline-none focus:ring-2 focus:ring-[var(--ring-focus)]"
              style={{
                left: `${p.leftPct}%`,
                width: `${p.widthPct}%`,
                background: color,
              }}
              onMouseEnter={() => setHoverIndex(p.idx)}
              onMouseLeave={() =>
                setHoverIndex((cur) => (cur === p.idx ? null : cur))
              }
              onFocus={() => setHoverIndex(p.idx)}
              onBlur={() =>
                setHoverIndex((cur) => (cur === p.idx ? null : cur))
              }
              onClick={() => onSeek(p.firstSeenSec)}
            >
              {p.severity !== "none" && (
                /* Alert-triangle glyph baked in as inline SVG to keep
                   the dependency footprint zero (lucide-react is not a
                   dependency of this app). White on the colored
                   background reads on both light and dark themes. */
                <svg
                  aria-hidden="true"
                  viewBox="0 0 24 24"
                  className="absolute left-1/2 top-1/2 h-3 w-3 -translate-x-1/2 -translate-y-1/2"
                  fill="none"
                  stroke="white"
                  strokeWidth="2.5"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                >
                  <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
                  <line x1="12" y1="9" x2="12" y2="13" />
                  <circle cx="12" cy="17" r="0.5" fill="white" />
                </svg>
              )}
            </button>
          );
        })}
      </div>

      {/* Scrubber strip: lightweight, only renders when there is at
          least one alerted encounter. Markers reuse the same xfrac as
          the main rail so the user can spot alerts at the bottom of
          the player without scrolling. */}
      {alertMarkers.length > 0 && (
        <div
          className="relative mt-1 w-full"
          style={{ height: SCRUBBER_HEIGHT, marginTop: STRIP_GAP }}
          aria-label="Alerted plate markers"
          data-testid="plate-scrubber"
        >
          {alertMarkers.map((p) => (
            <button
              key={`scrub-${p.enc.plate_hash_b64}-${p.idx}`}
              type="button"
              data-testid={`plate-scrubber-marker-${p.idx}`}
              data-severity={p.severity}
              aria-label={`Alerted plate ${p.enc.plate || p.enc.plate_hash_b64}`}
              className="absolute top-0 -translate-x-1/2 cursor-pointer focus:outline-none focus:ring-2 focus:ring-[var(--ring-focus)]"
              style={{
                left: `${p.leftPct + (p.widthPct / 2)}%`,
                color: COLOR_BY_SEVERITY[p.severity],
              }}
              onClick={() => onSeek(p.firstSeenSec)}
            >
              <svg
                aria-hidden="true"
                viewBox="0 0 24 24"
                width={SCRUBBER_HEIGHT}
                height={SCRUBBER_HEIGHT}
                fill="currentColor"
                stroke="white"
                strokeWidth="1.5"
                strokeLinejoin="round"
              >
                <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
              </svg>
            </button>
          ))}
        </div>
      )}

      {/* Tooltip. Anchored to the hovered segment via percent-left so
          it tracks horizontally with the segment without needing a
          measurement pass. Stays above the rail (negative top) so it
          doesn't overlap the SignalTimeline that sits below. */}
      {hovered && (
        <PlateTooltip
          encounter={hovered.enc}
          leftPct={hovered.leftPct + hovered.widthPct / 2}
          severity={hovered.severity}
        />
      )}
    </div>
  );
}

/**
 * encounterAriaLabel builds the accessible name for a single segment.
 * Mirrors the tooltip text so screen-reader users get the same
 * information sighted users see on hover.
 */
function encounterAriaLabel(e: PlateEncounter, severity: Severity): string {
  const parts: string[] = [];
  parts.push(e.plate ? `Plate ${e.plate}` : "Plate (encrypted)");
  if (severity !== "none") parts.push("alerted");
  if (e.signature) {
    const veh = vehicleLabel(e.signature);
    if (veh) parts.push(veh);
  }
  if (e.turn_count >= 1) {
    parts.push(`followed through ${e.turn_count} turn${e.turn_count === 1 ? "" : "s"}`);
  }
  return parts.join(", ");
}

function vehicleLabel(sig: PlateEncounter["signature"]): string {
  if (!sig) return "";
  const make = sig.make?.trim();
  const model = sig.model?.trim();
  if (make && model) return `${make} ${model}`;
  if (make) return make;
  if (model) return model;
  return "";
}

/**
 * Maps a free-form vehicle color string (whatever the engine returned
 * -- "white", "silver", "midnight blue") to a CSS color value for the
 * tooltip's color dot. Falls back to a neutral so an unrecognized
 * color does not produce an invisible dot.
 */
function vehicleColorCss(color: string | undefined): string {
  if (!color) return "var(--color-neutral-400)";
  const c = color.trim().toLowerCase();
  // A short list of common car colors; anything outside this set is
  // passed through directly so a CSS-known name like "darkred" still
  // works, and an unrecognized value falls back to neutral via the
  // browser's lenient color parser.
  switch (c) {
    case "white":
      return "#f4f4f5";
    case "silver":
      return "#c0c0c0";
    case "gray":
    case "grey":
      return "#71717a";
    case "black":
      return "#0b0b0c";
    case "red":
      return "#ef4444";
    case "blue":
      return "#3b82f6";
    case "green":
      return "#22c55e";
    case "yellow":
      return "#eab308";
    case "orange":
      return "#f97316";
    case "brown":
      return "#92400e";
    case "tan":
    case "beige":
      return "#d6cfb6";
    default:
      return c;
  }
}

interface PlateTooltipProps {
  encounter: PlateEncounter;
  leftPct: number;
  severity: Severity;
}

/**
 * PlateTooltip renders the hover card. Positioned absolutely above
 * the rail and clamped to the parent's horizontal bounds so segments
 * near the right edge do not push the tooltip off-screen.
 */
function PlateTooltip({ encounter, leftPct, severity }: PlateTooltipProps) {
  // Keep the tooltip away from the very edges so it does not overflow
  // the card. The `transform: translateX(-50%)` then centers it on
  // the clamped percentage.
  const clampedLeft = Math.min(95, Math.max(5, leftPct));
  const veh = vehicleLabel(encounter.signature);
  const showColorDot = !!encounter.signature?.color;
  const ackLine =
    encounter.ack_status === "acked"
      ? "Acknowledged"
      : encounter.ack_status === "open"
        ? "Open alert"
        : null;
  return (
    <div
      role="tooltip"
      data-testid="plate-tooltip"
      className="pointer-events-none absolute z-10 -translate-x-1/2 rounded-md border border-[var(--border-primary)] bg-[var(--bg-surface)] px-3 py-2 text-xs text-[var(--text-primary)] shadow-lg"
      style={{
        left: `${clampedLeft}%`,
        // Sit above the rail; the rail's offsetY is 0 so a negative
        // top floats the tooltip out without affecting layout.
        top: -8,
        transform: "translate(-50%, -100%)",
        minWidth: 180,
        maxWidth: 280,
      }}
    >
      <div className="flex items-center gap-2">
        <span
          className="inline-block h-2 w-2 rounded-full"
          style={{ background: COLOR_BY_SEVERITY[severity] }}
          aria-hidden="true"
        />
        <span className="font-mono text-sm font-semibold">
          {encounter.plate || encounter.plate_hash_b64}
        </span>
      </div>
      {veh && (
        <div className="mt-1 flex items-center gap-1.5">
          {showColorDot && (
            <span
              className="inline-block h-2 w-2 rounded-full ring-1 ring-[var(--border-primary)]"
              style={{ background: vehicleColorCss(encounter.signature?.color) }}
              aria-hidden="true"
            />
          )}
          <span className="text-[var(--text-secondary)]">{veh}</span>
        </div>
      )}
      {encounter.turn_count >= 1 && (
        <div className="mt-1 text-[var(--text-secondary)]">
          Followed through {encounter.turn_count} turn
          {encounter.turn_count === 1 ? "" : "s"}
        </div>
      )}
      {ackLine && (
        <div className="mt-1 text-[var(--text-secondary)]">{ackLine}</div>
      )}
      <div className="mt-1 text-[10px] uppercase tracking-wide text-[var(--text-tertiary)]">
        Click to seek
      </div>
    </div>
  );
}

export type { PlateTimelineProps };
