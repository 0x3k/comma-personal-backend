"use client";

import Link from "next/link";
import { Button } from "@/components/ui/Button";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";
import {
  classifyAlertSeverity,
  SEVERITY_COLOR,
  severityLabel,
} from "@/components/alerts/severity";
import { formatDurationBetween } from "@/lib/format";
import type { PlateDetailEncounter } from "@/lib/plateDetail";

interface PlateEncounterListProps {
  encounters: PlateDetailEncounter[];
  /**
   * Per-encounter severity. Sourced from the watchlist row but
   * uniform across encounters for now (severity is plate-wide, not
   * per-route, on the current schema). The page passes the same
   * value for every row; an encounter-level severity column would
   * require backend work.
   */
  globalSeverity: number | null;
}

/**
 * PlateEncounterList renders the chronological table of encounters.
 * One row per (dongle_id, route) tuple. The whole row is dense, single
 * line, and the route name itself links to the existing route detail
 * page so the operator can dive into a specific drive without
 * leaving the keyboard.
 *
 * Empty state is delegated to the parent: the plate-detail endpoint
 * 404s when no encounters exist, so an empty list is a programmer
 * error rather than a runtime case.
 */
export function PlateEncounterList({
  encounters,
  globalSeverity,
}: PlateEncounterListProps) {
  return (
    <Card className="mb-4" data-testid="plate-encounter-list">
      <CardHeader>
        <h2 className="text-sm font-semibold text-[var(--text-primary)]">
          Encounters ({encounters.length})
        </h2>
      </CardHeader>
      <CardBody className="px-0 py-0">
        <div className="overflow-x-auto">
          <table className="w-full text-xs">
            <thead className="bg-[var(--bg-secondary)] text-[var(--text-secondary)]">
              <tr>
                <Th>Date</Th>
                <Th>Route</Th>
                <Th>Area</Th>
                <Th align="right">Duration</Th>
                <Th align="right">Turns</Th>
                <Th align="right">Detections</Th>
                <Th>Severity</Th>
                <Th align="right">Action</Th>
              </tr>
            </thead>
            <tbody>
              {encounters.map((e) => {
                const bucket = classifyAlertSeverity(globalSeverity);
                const detailHref = `/routes/${encodeURIComponent(e.dongle_id)}/${encodeURIComponent(e.route)}`;
                return (
                  <tr
                    key={`${e.dongle_id}|${e.route}`}
                    data-testid={`plate-encounter-row-${e.dongle_id}-${e.route}`}
                    className="border-t border-[var(--border-primary)]"
                  >
                    <Td>{formatRowDate(e.first_seen_ts)}</Td>
                    <Td>
                      <Link
                        href={detailHref}
                        className="font-mono text-[var(--accent)] hover:underline"
                      >
                        {e.route}
                      </Link>
                    </Td>
                    <Td className="text-[var(--text-secondary)]">
                      {e.area_cluster_label || (
                        <span className="text-[var(--text-tertiary)]">—</span>
                      )}
                    </Td>
                    <Td align="right" className="tabular-nums">
                      {formatDurationBetween(e.first_seen_ts, e.last_seen_ts)}
                    </Td>
                    <Td align="right" className="tabular-nums">
                      {e.turn_count}
                    </Td>
                    <Td align="right" className="tabular-nums">
                      {e.detection_count}
                    </Td>
                    <Td>
                      <span
                        className="inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium"
                        data-severity-bucket={bucket}
                        style={{
                          background: `color-mix(in srgb, ${SEVERITY_COLOR[bucket]} 18%, transparent)`,
                          color: SEVERITY_COLOR[bucket],
                        }}
                      >
                        <span
                          aria-hidden="true"
                          className="inline-block h-1.5 w-1.5 rounded-full"
                          style={{ background: SEVERITY_COLOR[bucket] }}
                        />
                        {severityLabel(globalSeverity)}
                      </span>
                    </Td>
                    <Td align="right">
                      <Link href={detailHref}>
                        <Button variant="secondary" size="sm" type="button">
                          Open route
                        </Button>
                      </Link>
                    </Td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </CardBody>
    </Card>
  );
}

interface CellProps {
  children: React.ReactNode;
  align?: "left" | "right";
  className?: string;
}

function Th({ children, align = "left" }: CellProps) {
  return (
    <th
      scope="col"
      className={[
        "px-3 py-2 text-xs font-medium",
        align === "right" ? "text-right" : "text-left",
      ].join(" ")}
    >
      {children}
    </th>
  );
}

function Td({ children, align = "left", className = "" }: CellProps) {
  return (
    <td
      className={[
        "px-3 py-2 align-middle text-[var(--text-primary)]",
        align === "right" ? "text-right" : "text-left",
        className,
      ]
        .filter(Boolean)
        .join(" ")}
    >
      {children}
    </td>
  );
}

/**
 * formatRowDate is the encounter-list date formatter. Renders a
 * locale-aware compact form so the table stays narrow on small
 * screens.
 */
function formatRowDate(iso: string): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return "—";
  return new Date(t).toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}
