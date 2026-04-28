"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { apiFetch } from "@/lib/api";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";
import { classifyAlertSeverity } from "@/lib/useAlertSummary";
import type { AlertSeverityBucket } from "@/lib/useAlertSummary";

/**
 * One row of GET /v1/alpr/alerts. Mirrors alertItem in
 * internal/api/alpr_watchlist.go. Only the fields the dashboard
 * widget renders are documented; future fields can be added here
 * without touching the widget.
 */
export interface AlertItem {
  plate_hash_b64: string;
  plate: string;
  signature: {
    make?: string;
    model?: string;
    color?: string;
    body_type?: string;
    confidence?: number;
  } | null;
  severity: number | null;
  kind: string;
  first_alert_at: string | null;
  last_alert_at: string | null;
  acked_at: string | null;
  encounter_count: number;
  latest_route: {
    dongle_id: string;
    route: string;
    started_at?: string;
    address_label?: string;
  } | null;
  evidence_summary: string;
  notes?: string;
}

interface AlertsListResponse {
  alerts: AlertItem[];
}

interface RecentAlertsWidgetProps {
  /**
   * Number of alerts to fetch. Defaults to 3 (matches the acceptance
   * criterion). Tests can override to assert pagination behavior.
   */
  limit?: number;
}

/**
 * RecentAlertsWidget renders the top-N most-recent open alerts as a
 * compact card sitting below the lifetime-stats grid on the dashboard
 * home page. Hidden entirely when the fetch returns zero rows; the
 * dashboard parent is responsible for hiding it when ALPR is disabled
 * (the widget itself does not consult useAlprSettings, so it can be
 * lazy-loaded without dragging the settings hook into the home page's
 * critical render path).
 *
 * The fetch is intentionally deferred to a useEffect so the card
 * paints empty for one frame, letting the lifetime-stats card paint
 * first. The summary endpoint is cheap and gates the widget from the
 * parent, so this component never mounts when there are zero alerts.
 */
export function RecentAlertsWidget({
  limit = 3,
}: RecentAlertsWidgetProps = {}) {
  const [alerts, setAlerts] = useState<AlertItem[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setError(null);
    apiFetch<AlertsListResponse>(
      `/v1/alpr/alerts?status=open&limit=${encodeURIComponent(String(limit))}`,
    )
      .then((data) => {
        if (cancelled) return;
        setAlerts(data.alerts ?? []);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : "Failed to load alerts");
      });
    return () => {
      cancelled = true;
    };
  }, [limit]);

  // Until the fetch resolves we render a small skeleton so the page
  // doesn't shift when alerts arrive. After the resolve, an empty
  // list hides the widget per acceptance criterion 4.
  if (alerts === null && error == null) {
    return (
      <Card data-testid="alpr-recent-alerts-loading">
        <CardHeader>
          <h2 className="text-subheading">Recent alerts</h2>
        </CardHeader>
        <CardBody className="px-2 py-2">
          <ul className="divide-y divide-[var(--border-primary)]">
            {Array.from({ length: limit }).map((_, i) => (
              <li key={i}>
                <RecentAlertRowSkeleton />
              </li>
            ))}
          </ul>
        </CardBody>
      </Card>
    );
  }

  if (error) {
    return (
      <Card data-testid="alpr-recent-alerts-error">
        <CardHeader>
          <h2 className="text-subheading">Recent alerts</h2>
        </CardHeader>
        <CardBody>
          <p className="text-sm text-[var(--text-secondary)]">{error}</p>
        </CardBody>
      </Card>
    );
  }

  // Resolved + empty: hide entirely (no zero-state card, mirrors the
  // badge logic).
  if (!alerts || alerts.length === 0) return null;

  return (
    <Card data-testid="alpr-recent-alerts-widget">
      <CardHeader>
        <div className="flex items-center justify-between gap-2">
          <h2 className="text-subheading">Recent alerts</h2>
          <Link
            href="/alerts"
            className="text-sm text-[var(--accent)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)] rounded"
          >
            View all
          </Link>
        </div>
      </CardHeader>
      <CardBody className="px-2 py-2">
        <ul className="divide-y divide-[var(--border-primary)]">
          {alerts.map((alert) => (
            <li key={alert.plate_hash_b64}>
              <RecentAlertRow alert={alert} />
            </li>
          ))}
        </ul>
      </CardBody>
    </Card>
  );
}

interface RecentAlertRowProps {
  alert: AlertItem;
}

function RecentAlertRow({ alert }: RecentAlertRowProps) {
  const bucket: AlertSeverityBucket = classifyAlertSeverity(alert.severity);
  const vehicle = vehicleLabel(alert.signature);
  const plateLabel = alert.plate || alert.plate_hash_b64;
  // Plate detail page is a future feature (alpr-watchlist-page sibling).
  // Linking by hash is the documented contract; the URL stabilizes
  // even before the route exists.
  const plateHref = `/plates/${encodeURIComponent(alert.plate_hash_b64)}`;

  return (
    <Link
      href={plateHref}
      data-testid="alpr-recent-alert-row"
      className="block rounded-md px-3 py-3 transition-colors hover:bg-[var(--bg-tertiary)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]"
    >
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-[auto_minmax(0,1fr)] sm:items-start sm:gap-3">
        <div className="flex flex-shrink-0 items-center gap-2">
          <SeverityChip bucket={bucket} severity={alert.severity} />
          <span className="font-mono text-sm font-semibold text-[var(--text-primary)]">
            {plateLabel}
          </span>
          {vehicle && <VehicleBadge label={vehicle} />}
        </div>
        <div className="min-w-0 text-sm text-[var(--text-secondary)] sm:text-right">
          {alert.evidence_summary ? (
            <span className="block truncate">{alert.evidence_summary}</span>
          ) : (
            <span className="italic">No evidence summary</span>
          )}
        </div>
      </div>
    </Link>
  );
}

function RecentAlertRowSkeleton() {
  return (
    <div className="grid grid-cols-1 gap-2 px-3 py-3 sm:grid-cols-[auto_minmax(0,1fr)] sm:items-center sm:gap-3">
      <div className="flex items-center gap-2">
        <div className="h-5 w-12 animate-pulse rounded-full bg-[var(--bg-tertiary)]" />
        <div className="h-4 w-24 animate-pulse rounded bg-[var(--bg-tertiary)]" />
      </div>
      <div className="h-4 w-full animate-pulse rounded bg-[var(--bg-tertiary)]" />
    </div>
  );
}

interface SeverityChipProps {
  bucket: AlertSeverityBucket;
  severity: number | null;
}

/**
 * SeverityChip renders a small "Sev N" pill colored by bucket. Reuses
 * the same color tokens as AlertBadge so the dashboard's two ALPR
 * surfaces feel consistent at a glance.
 */
function SeverityChip({ bucket, severity }: SeverityChipProps) {
  const colorClasses =
    bucket === "red"
      ? "bg-danger-500/15 text-danger-600 dark:text-danger-500"
      : "bg-warning-500/15 text-warning-600 dark:text-warning-500";
  const text = severity != null ? `Sev ${severity}` : "Sev ?";
  return (
    <span
      data-testid="alpr-severity-chip"
      data-severity-bucket={bucket}
      className={[
        "inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium",
        colorClasses,
      ].join(" ")}
    >
      {text}
    </span>
  );
}

interface VehicleBadgeProps {
  label: string;
}

function VehicleBadge({ label }: VehicleBadgeProps) {
  return (
    <span
      data-testid="alpr-vehicle-badge"
      className="inline-flex items-center rounded-full bg-[var(--bg-tertiary)] px-2 py-0.5 text-xs font-medium text-[var(--text-secondary)]"
    >
      {label}
    </span>
  );
}

function vehicleLabel(sig: AlertItem["signature"]): string {
  if (!sig) return "";
  const make = sig.make?.trim();
  const model = sig.model?.trim();
  if (make && model) return `${make} ${model}`;
  if (make) return make;
  if (model) return model;
  return "";
}

export type { RecentAlertsWidgetProps };
