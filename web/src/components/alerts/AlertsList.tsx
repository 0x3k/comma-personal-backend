"use client";

import { useMemo, useRef } from "react";
import Link from "next/link";
import { useVirtualizer } from "@tanstack/react-virtual";
import { Button } from "@/components/ui/Button";
import { Badge } from "@/components/ui/Badge";
import {
  classifyAlertSeverity,
  SEVERITY_COLOR,
  severityLabel,
  type SeverityBucket,
} from "./severity";
import type { AlertItem } from "./api";

/**
 * RowState carries per-row mutable UI state derived from the bulk-ack
 * flow. Rows are keyed by plate_hash_b64.
 *
 * - "selected" means the checkbox is ticked (eligible for bulk-ack).
 * - "ackInFlight" means a single-row ack/unack request is currently
 *   in flight (the row's button shows a spinner-like disabled state).
 * - "failed" means a recent bulk-ack rejected this row -- the row
 *   borders red and the operator can retry.
 *
 * The page owns these maps and patches them as the user interacts;
 * the list is purely presentational with respect to row state.
 */
export interface AlertRowControl {
  selected: boolean;
  ackInFlight: boolean;
  failed: boolean;
  /** True after a successful ack arrived from the server (optimistic). */
  ackedOptimistic: boolean | null;
}

interface AlertsListProps {
  alerts: AlertItem[];
  /**
   * "open" or "acked" determines whether the per-row button reads
   * "Ack" (open tab) or "Unack" (acked tab). The list is otherwise
   * identical for the two views.
   */
  mode: "open" | "acked";
  rowState: Record<string, AlertRowControl | undefined>;
  onToggleSelect: (hash: string) => void;
  onSelectAll: (next: boolean) => void;
  onAckSingle: (item: AlertItem) => void;
  onUnackSingle: (item: AlertItem) => void;
}

const ESTIMATED_ROW_HEIGHT = 56;

/**
 * AlertsList renders the virtualized alert feed. Mirrors the LogViewer
 * pattern (TanStack Virtual + an absolute-positioned row layer). Rows
 * are dense (single line each) so 1k+ alerts paint without jank; the
 * spec calls out density over chrome.
 *
 * The row click target is the row itself: clicking the row navigates
 * to the plate detail page. The checkbox and ack button stop their
 * own propagation so they don't navigate. This matches the routes
 * list pattern (whole-card link) and keeps each row clickable as a
 * single target rather than a row of micro-targets.
 */
export function AlertsList({
  alerts,
  mode,
  rowState,
  onToggleSelect,
  onSelectAll,
  onAckSingle,
  onUnackSingle,
}: AlertsListProps) {
  const parentRef = useRef<HTMLDivElement>(null);

  const virtualizer = useVirtualizer({
    count: alerts.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => ESTIMATED_ROW_HEIGHT,
    overscan: 10,
  });

  const allSelected = useMemo(() => {
    if (alerts.length === 0) return false;
    return alerts.every((a) => rowState[a.plate_hash_b64]?.selected);
  }, [alerts, rowState]);

  const someSelected = useMemo(() => {
    return alerts.some((a) => rowState[a.plate_hash_b64]?.selected);
  }, [alerts, rowState]);

  return (
    <div className="flex flex-col rounded-lg border border-[var(--border-primary)] bg-[var(--bg-surface)]">
      {/* Header row with select-all checkbox + column labels. Always
          rendered (even with 0 rows) so the list reads as a table
          rather than empty surface. */}
      <div
        className="flex items-center gap-2 border-b border-[var(--border-primary)] bg-[var(--bg-secondary)] px-3 py-2 text-xs font-medium text-[var(--text-secondary)]"
      >
        <input
          type="checkbox"
          aria-label="Select all alerts"
          data-testid="alerts-select-all"
          checked={allSelected}
          ref={(el) => {
            // indeterminate is not a React prop; set imperatively.
            if (el) el.indeterminate = !allSelected && someSelected;
          }}
          onChange={(e) => onSelectAll(e.target.checked)}
          className="h-3.5 w-3.5 accent-[var(--accent)]"
        />
        <span className="w-16 shrink-0">Severity</span>
        <span className="w-32 shrink-0">Plate</span>
        <span className="w-40 shrink-0">Vehicle</span>
        <span className="w-44 shrink-0">Last alert</span>
        <span className="w-16 shrink-0 text-right">Routes</span>
        <span className="flex-1 min-w-0">Evidence</span>
        <span className="w-20 shrink-0 text-right">Action</span>
      </div>

      {/* Virtual scroll container. Same height clamp pattern as the
          LogViewer so the list fits any viewport. */}
      <div
        ref={parentRef}
        className="overflow-auto"
        style={{ height: "clamp(300px, 60vh, 720px)" }}
        data-testid="alerts-list-scroll"
      >
        <div
          style={{
            height: `${virtualizer.getTotalSize()}px`,
            width: "100%",
            position: "relative",
          }}
        >
          {virtualizer.getVirtualItems().map((virtualRow) => {
            const item = alerts[virtualRow.index];
            const ctrl = rowState[item.plate_hash_b64];
            return (
              <AlertRow
                key={item.plate_hash_b64}
                item={item}
                mode={mode}
                index={virtualRow.index}
                top={virtualRow.start}
                control={ctrl}
                onToggleSelect={onToggleSelect}
                onAckSingle={onAckSingle}
                onUnackSingle={onUnackSingle}
              />
            );
          })}
        </div>
      </div>
    </div>
  );
}

interface AlertRowProps {
  item: AlertItem;
  mode: "open" | "acked";
  index: number;
  top: number;
  control: AlertRowControl | undefined;
  onToggleSelect: (hash: string) => void;
  onAckSingle: (item: AlertItem) => void;
  onUnackSingle: (item: AlertItem) => void;
}

/**
 * AlertRow is a single virtualized row. Stateless w.r.t. ack/unack
 * progress -- it reads from the page-owned control map and reports
 * intents up.
 *
 * Whole-row navigation: the Link wraps the row so clicking anywhere
 * outside the checkbox and Ack button navigates to /plates/:hash.
 * The interactive controls call e.stopPropagation() to opt out.
 */
function AlertRow({
  item,
  mode,
  top,
  control,
  onToggleSelect,
  onAckSingle,
  onUnackSingle,
}: AlertRowProps) {
  const bucket = classifyAlertSeverity(item.severity);
  const failed = control?.failed === true;
  const selected = control?.selected === true;
  const inFlight = control?.ackInFlight === true;
  // ackedOptimistic carries the post-bulk-ack state. Falls back to
  // the wire's acked_at so a single-row toggle still flips the button
  // label without waiting for a full refetch.
  const acked = control?.ackedOptimistic ?? item.acked_at !== null;

  const lastAlertAbs = item.last_alert_at ?? "";
  const lastAlertRel = item.last_alert_at
    ? formatRelativeTime(item.last_alert_at)
    : "—";

  const vehicleLabel = formatVehicle(item.signature);

  const detailHref = `/plates/${item.plate_hash_b64}`;

  return (
    <Link
      href={detailHref}
      data-testid={`alerts-row-${item.plate_hash_b64}`}
      data-failed={failed}
      data-acked={acked}
      style={{
        position: "absolute",
        top: 0,
        left: 0,
        width: "100%",
        transform: `translateY(${top}px)`,
        height: ESTIMATED_ROW_HEIGHT,
      }}
      className={[
        "flex items-center gap-2 border-b border-[var(--border-primary)] px-3 text-xs",
        "hover:bg-[var(--bg-tertiary)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]",
        failed
          ? "bg-danger-500/10 ring-1 ring-inset ring-danger-500/40"
          : "",
      ]
        .filter(Boolean)
        .join(" ")}
    >
      {/* Checkbox -- own click target; do not navigate when toggled. */}
      <input
        type="checkbox"
        data-testid={`alerts-row-select-${item.plate_hash_b64}`}
        aria-label={`Select alert for ${item.plate || item.plate_hash_b64}`}
        checked={selected}
        onChange={() => onToggleSelect(item.plate_hash_b64)}
        onClick={(e) => e.stopPropagation()}
        className="h-3.5 w-3.5 shrink-0 accent-[var(--accent)]"
      />

      {/* Severity badge. Color-coded to match the timeline. */}
      <span className="w-16 shrink-0">
        <SeverityBadge bucket={bucket} sev={item.severity} />
      </span>

      {/* Plate (monospace per spec). */}
      <span className="w-32 shrink-0 truncate font-mono text-[var(--text-primary)]">
        {item.plate || item.plate_hash_b64.slice(0, 12)}
      </span>

      {/* Vehicle badge. Empty when no signature. */}
      <span className="w-40 shrink-0 truncate">
        {vehicleLabel ? (
          <Badge variant="neutral">{vehicleLabel}</Badge>
        ) : (
          <span className="text-[var(--text-tertiary)]">—</span>
        )}
      </span>

      {/* Last-alert: relative + absolute on hover via title. */}
      <span
        className="w-44 shrink-0 text-[var(--text-secondary)]"
        title={lastAlertAbs || "Never alerted"}
      >
        {lastAlertRel}
      </span>

      {/* Encounter count. */}
      <span className="w-16 shrink-0 text-right tabular-nums text-[var(--text-secondary)]">
        {item.encounter_count}
      </span>

      {/* Evidence summary -- single-line ellipsis on overflow. */}
      <span
        className="flex-1 min-w-0 truncate text-[var(--text-secondary)]"
        title={item.evidence_summary || ""}
      >
        {item.evidence_summary || (
          <span className="text-[var(--text-tertiary)]">No evidence yet</span>
        )}
      </span>

      {/* Ack / Unack button. Stops propagation so clicking it does
          not also navigate to the detail page. */}
      <span className="w-20 shrink-0 text-right">
        <Button
          variant={mode === "open" ? "primary" : "secondary"}
          size="sm"
          disabled={inFlight}
          data-testid={`alerts-row-ack-${item.plate_hash_b64}`}
          onClick={(e) => {
            e.preventDefault();
            e.stopPropagation();
            if (mode === "open") onAckSingle(item);
            else onUnackSingle(item);
          }}
        >
          {inFlight ? "..." : mode === "open" ? "Ack" : "Unack"}
        </Button>
      </span>
    </Link>
  );
}

interface SeverityBadgeProps {
  bucket: SeverityBucket;
  sev: number | null;
}

function SeverityBadge({ bucket, sev }: SeverityBadgeProps) {
  // Custom inline-styled badge so the color stays in lock-step with
  // PlateTimeline (same CSS variable references). The Badge component
  // would force one of the predefined variants which don't include
  // amber-vs-red; spelling it out here keeps that distinction.
  return (
    <span
      data-testid="alerts-row-severity"
      data-severity-bucket={bucket}
      className="inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium"
      style={{
        background: `color-mix(in srgb, ${SEVERITY_COLOR[bucket]} 18%, transparent)`,
        color: SEVERITY_COLOR[bucket],
      }}
    >
      <span
        aria-hidden="true"
        className="inline-block h-2 w-2 rounded-full"
        style={{ background: SEVERITY_COLOR[bucket] }}
      />
      {severityLabel(sev)}
    </span>
  );
}

/**
 * formatVehicle distills the signature into a one-line vehicle label
 * for the dedicated column. Returns "" when the signature carries no
 * make/model/color, in which case the row renders an em dash.
 */
function formatVehicle(sig: AlertItem["signature"]): string {
  if (!sig) return "";
  const make = sig.make?.trim();
  const model = sig.model?.trim();
  const color = sig.color?.trim();
  const parts: string[] = [];
  if (color) parts.push(color);
  if (make && model) parts.push(`${make} ${model}`);
  else if (make) parts.push(make);
  else if (model) parts.push(model);
  return parts.join(" ");
}

/**
 * formatRelativeTime turns an ISO8601 string into "5m ago" / "2h ago".
 * Coarse buckets are fine for the alert list; the absolute timestamp
 * is always available on hover via the row's title attribute.
 */
function formatRelativeTime(iso: string): string {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return "—";
  const deltaMs = Date.now() - t;
  if (deltaMs < 0) return "just now";
  const sec = Math.floor(deltaMs / 1000);
  if (sec < 60) return `${sec}s ago`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const day = Math.floor(hr / 24);
  if (day < 30) return `${day}d ago`;
  const mo = Math.floor(day / 30);
  if (mo < 12) return `${mo}mo ago`;
  const yr = Math.floor(day / 365);
  return `${yr}y ago`;
}

export { AlertRow };
