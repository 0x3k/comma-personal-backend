"use client";

import Link from "next/link";
import { classifyAlertSeverity } from "@/lib/useAlertSummary";
import type { AlertSummary } from "@/lib/useAlertSummary";

interface AlertBadgeProps {
  summary: AlertSummary | null;
  /** Extra classes for the wrapping link. */
  className?: string;
}

/**
 * AlertBadge renders the dashboard's open-alerts pill. Hidden when
 * the summary has not yet resolved or `open_count` is zero -- there
 * is no zero-state pill, on the principle that visual noise is worse
 * than absence.
 *
 * Color follows max_open_severity: 2-3 -> amber, 4-5 -> red. Click
 * navigates to /alerts (the alpr-watchlist-page route, which may not
 * yet have shipped on this branch -- the URL is the contract).
 */
export function AlertBadge({ summary, className = "" }: AlertBadgeProps) {
  if (!summary || summary.open_count <= 0) return null;

  const bucket = classifyAlertSeverity(summary.max_open_severity);
  const colorClasses =
    bucket === "red"
      ? "bg-danger-500/15 text-danger-600 ring-danger-500/30 dark:text-danger-500"
      : "bg-warning-500/15 text-warning-600 ring-warning-500/30 dark:text-warning-500";

  const label =
    summary.open_count === 1
      ? "1 open alert"
      : `${summary.open_count.toLocaleString()} open alerts`;

  return (
    <Link
      href="/alerts"
      data-testid="alpr-alert-badge"
      data-severity-bucket={bucket}
      aria-label={label}
      className={[
        "inline-flex items-center gap-1.5 rounded-full px-3 py-1 text-xs font-semibold ring-1 ring-inset transition-colors",
        "hover:brightness-110 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]",
        colorClasses,
        className,
      ]
        .filter(Boolean)
        .join(" ")}
    >
      {/* Inline alert-triangle so the dependency footprint stays zero;
          matches the glyph used by PlateTimeline. */}
      <svg
        aria-hidden="true"
        viewBox="0 0 24 24"
        className="h-3.5 w-3.5"
        fill="none"
        stroke="currentColor"
        strokeWidth="2.5"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
        <line x1="12" y1="9" x2="12" y2="13" />
        <circle cx="12" cy="17" r="0.5" fill="currentColor" />
      </svg>
      <span>{label}</span>
    </Link>
  );
}

export type { AlertBadgeProps };
