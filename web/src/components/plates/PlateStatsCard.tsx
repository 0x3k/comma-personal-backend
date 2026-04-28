"use client";

import { Card, CardBody } from "@/components/ui/Card";
import type { PlateDetailStats } from "@/lib/plateDetail";

interface PlateStatsCardProps {
  stats: PlateDetailStats;
}

/**
 * PlateStatsCard renders the dense "what do we know about this plate"
 * summary block. Five stats laid out as a grid; each cell is a label
 * + value pair so the operator can scan for the answer they want
 * without parsing prose.
 */
export function PlateStatsCard({ stats }: PlateStatsCardProps) {
  return (
    <Card className="mb-4" data-testid="plate-stats-card">
      <CardBody>
        <dl className="grid grid-cols-2 gap-x-4 gap-y-3 sm:grid-cols-3 lg:grid-cols-5">
          <Stat
            label="Distinct routes (30d)"
            value={String(stats.distinct_routes_30d)}
            testid="plate-stats-routes-30d"
          />
          <Stat
            label="Distinct areas (30d)"
            value={String(stats.distinct_areas_30d)}
            testid="plate-stats-areas-30d"
          />
          <Stat
            label="Total detections"
            value={stats.total_detections.toLocaleString()}
            testid="plate-stats-total-detections"
          />
          <Stat
            label="First seen"
            value={formatStatDate(stats.first_ever_seen)}
            testid="plate-stats-first-seen"
          />
          <Stat
            label="Last seen"
            value={formatStatDate(stats.last_ever_seen)}
            testid="plate-stats-last-seen"
          />
        </dl>
      </CardBody>
    </Card>
  );
}

interface StatProps {
  label: string;
  value: string;
  testid: string;
}

function Stat({ label, value, testid }: StatProps) {
  return (
    <div>
      <dt className="text-xs font-medium uppercase tracking-wide text-[var(--text-tertiary)]">
        {label}
      </dt>
      <dd
        className="mt-1 text-base font-semibold tabular-nums text-[var(--text-primary)]"
        data-testid={testid}
      >
        {value}
      </dd>
    </div>
  );
}

/**
 * formatStatDate renders an ISO timestamp as a compact local date.
 * Empty / undefined inputs collapse to an em dash so missing data
 * reads as "we don't know" rather than "now".
 */
function formatStatDate(iso: string | undefined): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return "—";
  const d = new Date(t);
  return d.toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}
