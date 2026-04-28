"use client";

import { useState } from "react";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";
import type {
  PlateDetailEncounter,
  PlateDetailStats,
} from "@/lib/plateDetail";

interface PlateEvidenceAccordionProps {
  stats: PlateDetailStats;
  encounters: PlateDetailEncounter[];
}

/**
 * PlateEvidenceAccordion summarises why a plate looks suspicious in
 * a friendly bullet list. The plate-detail endpoint does not yet
 * return a structured per-heuristic-component breakdown -- the
 * heuristic worker writes its decision into plate_alert_events but
 * the read API only surfaces aggregate stats -- so we derive the
 * bullets here from what we have. When the API is widened to expose
 * evidence components, this component can be updated to read them
 * directly without changing the page's call sites.
 *
 * Each bullet is a single sentence: the operator should be able to
 * scan the list in five seconds.
 */
export function PlateEvidenceAccordion({
  stats,
  encounters,
}: PlateEvidenceAccordionProps) {
  const [open, setOpen] = useState<boolean>(false);

  const bullets = buildEvidenceBullets(stats, encounters);

  return (
    <Card className="mb-4" data-testid="plate-evidence-accordion">
      <CardHeader>
        <button
          type="button"
          aria-expanded={open}
          aria-controls="plate-evidence-body"
          onClick={() => setOpen((p) => !p)}
          className="flex w-full items-center justify-between text-left"
          data-testid="plate-evidence-toggle"
        >
          <span className="text-sm font-semibold text-[var(--text-primary)]">
            Evidence ({bullets.length})
          </span>
          <span
            aria-hidden="true"
            className="ml-2 text-xs text-[var(--text-secondary)]"
          >
            {open ? "Hide" : "Show"}
          </span>
        </button>
      </CardHeader>
      {open && (
        <CardBody id="plate-evidence-body" data-testid="plate-evidence-body">
          {bullets.length === 0 ? (
            <p className="text-xs text-[var(--text-tertiary)]">
              No evidence summary available.
            </p>
          ) : (
            <ul className="list-disc space-y-1 pl-5 text-xs text-[var(--text-secondary)]">
              {bullets.map((b, i) => (
                <li key={i}>{b}</li>
              ))}
            </ul>
          )}
        </CardBody>
      )}
    </Card>
  );
}

/**
 * buildEvidenceBullets renders a small set of natural-language
 * statements derived from the plate's aggregate stats and the
 * per-encounter rows. The phrasing matches the spec's example
 * sentences ("Seen on 5 distinct routes in 30 days", "Followed
 * through 4 turns on Tuesday's drive").
 *
 * Pure function -- exposed for unit testing without rendering.
 */
export function buildEvidenceBullets(
  stats: PlateDetailStats,
  encounters: PlateDetailEncounter[],
): string[] {
  const out: string[] = [];

  if (stats.distinct_routes_30d > 0) {
    out.push(
      `Seen on ${stats.distinct_routes_30d} distinct route${
        stats.distinct_routes_30d === 1 ? "" : "s"
      } in the last 30 days.`,
    );
  }
  if (stats.distinct_areas_30d > 0 && stats.distinct_areas_30d !== stats.distinct_routes_30d) {
    out.push(
      `Crossed ${stats.distinct_areas_30d} distinct area${
        stats.distinct_areas_30d === 1 ? "" : "s"
      } in the last 30 days.`,
    );
  }
  if (stats.total_detections > 0) {
    out.push(
      `${stats.total_detections.toLocaleString()} total detection${
        stats.total_detections === 1 ? "" : "s"
      } across all encounters.`,
    );
  }

  // Highest-turn encounter -- the heuristic flags plates that follow
  // through multiple turns, so calling out the "most-followed" drive
  // is the most actionable single bullet.
  const topTurn = encounters.reduce<PlateDetailEncounter | null>(
    (best, cur) => (best === null || cur.turn_count > best.turn_count ? cur : best),
    null,
  );
  if (topTurn && topTurn.turn_count >= 2) {
    out.push(
      `Followed through ${topTurn.turn_count} turn${
        topTurn.turn_count === 1 ? "" : "s"
      } on ${formatEncounterDate(topTurn.first_seen_ts)}.`,
    );
  }

  if (stats.first_ever_seen) {
    out.push(`First seen on ${formatEncounterDate(stats.first_ever_seen)}.`);
  }
  if (stats.last_ever_seen && stats.last_ever_seen !== stats.first_ever_seen) {
    out.push(`Most recently seen on ${formatEncounterDate(stats.last_ever_seen)}.`);
  }

  return out;
}

/**
 * formatEncounterDate renders an ISO timestamp as a friendly weekday
 * reference. We deliberately keep it short (no clock time) so the
 * bullets stay readable; the exact timestamp is available in the
 * encounter table above.
 */
function formatEncounterDate(iso: string | undefined): string {
  if (!iso) return "an unknown date";
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return "an unknown date";
  const d = new Date(t);
  return d.toLocaleDateString(undefined, {
    weekday: "long",
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}
