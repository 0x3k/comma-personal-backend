"use client";

import { type ChangeEvent } from "react";
import { Button } from "@/components/ui/Button";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";
import {
  EMPTY_ALERTS_FILTERS,
  isDefaultAlertsFilters,
  SEVERITY_VALUES,
  type AlertsFilterState,
  type Severity,
} from "./filters";
import { classifyAlertSeverity, SEVERITY_COLOR } from "./severity";

interface AlertsFilterBarProps {
  filters: AlertsFilterState;
  onChange: (next: AlertsFilterState) => void;
  /**
   * Available device dongles for the dropdown. When the user only has
   * one device the dropdown is hidden entirely (the spec keeps the bar
   * dense when the choice is degenerate).
   */
  availableDongles: string[];
}

const INPUT_CLASS =
  "rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-2 py-1 text-xs text-[var(--text-primary)] focus:border-[var(--accent)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]";

const LABEL_CLASS = "text-xs text-[var(--text-secondary)]";

/**
 * AlertsFilterBar is a compact filter row above the virtualized list.
 * Severity uses chip-style toggles (multi-select) with color tokens
 * matching the row badges, so the filter visually echoes the
 * highlighted rows. Dongle is a plain select. Date range uses two
 * <input type="date"> controls -- consistent with the routes filter
 * bar so the muscle memory carries across pages.
 */
export function AlertsFilterBar({
  filters,
  onChange,
  availableDongles,
}: AlertsFilterBarProps) {
  const patch = (p: Partial<AlertsFilterState>) =>
    onChange({ ...filters, ...p });

  const onText =
    (key: "from" | "to") => (e: ChangeEvent<HTMLInputElement>) =>
      patch({ [key]: e.target.value });

  const onDongle = (e: ChangeEvent<HTMLSelectElement>) =>
    patch({ dongle_id: e.target.value });

  const toggleSeverity = (sev: Severity) => {
    const has = filters.severity.includes(sev);
    const next = has
      ? filters.severity.filter((s) => s !== sev)
      : [...filters.severity, sev].sort((a, b) => a - b);
    patch({ severity: next });
  };

  const hasAny = !isDefaultAlertsFilters(filters);

  // Hide the dongle selector when there is at most one device. The
  // spec calls this out explicitly: "dongle_id selector (when user has
  // more than one device)". A single-device deployment shouldn't see
  // a no-op dropdown.
  const showDongleSelector = availableDongles.length > 1;

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
            onClick={() => onChange(EMPTY_ALERTS_FILTERS)}
            disabled={!hasAny}
          >
            Clear filters
          </Button>
        </div>
      </CardHeader>
      <CardBody>
        <div className="flex flex-col gap-3">
          {/* Severity chips. Color-coded to match the row badges so
              the filter and result colors stay in lock-step. */}
          <div className="flex flex-wrap items-center gap-2">
            <span className={LABEL_CLASS}>Severity</span>
            {SEVERITY_VALUES.map((sev) => {
              const active = filters.severity.includes(sev);
              const bucket = classifyAlertSeverity(sev);
              return (
                <button
                  key={sev}
                  type="button"
                  data-testid={`alerts-filter-severity-${sev}`}
                  data-active={active}
                  onClick={() => toggleSeverity(sev)}
                  aria-pressed={active}
                  className={[
                    "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs transition-colors",
                    active
                      ? "border-[var(--accent)] bg-[var(--accent)]/15 text-[var(--text-primary)]"
                      : "border-[var(--border-secondary)] bg-[var(--bg-secondary)] text-[var(--text-secondary)] hover:bg-[var(--bg-tertiary)]",
                  ].join(" ")}
                >
                  <span
                    aria-hidden="true"
                    className="inline-block h-2 w-2 rounded-full"
                    style={{ background: SEVERITY_COLOR[bucket] }}
                  />
                  Sev {sev}
                </button>
              );
            })}
          </div>

          {/* Dongle selector + date range. Reuses the same row to
              keep filter density high. */}
          <div className="flex flex-wrap items-center gap-3">
            {showDongleSelector && (
              <div className="flex items-center gap-2">
                <label htmlFor="alerts-filter-dongle" className={LABEL_CLASS}>
                  Device
                </label>
                <select
                  id="alerts-filter-dongle"
                  value={filters.dongle_id}
                  onChange={onDongle}
                  className={INPUT_CLASS}
                >
                  <option value="">All devices</option>
                  {availableDongles.map((d) => (
                    <option key={d} value={d}>
                      {d}
                    </option>
                  ))}
                </select>
              </div>
            )}

            <div className="flex items-center gap-2">
              <label htmlFor="alerts-filter-from" className={LABEL_CLASS}>
                From
              </label>
              <input
                id="alerts-filter-from"
                type="date"
                value={filters.from}
                onChange={onText("from")}
                className={INPUT_CLASS}
              />
            </div>
            <div className="flex items-center gap-2">
              <label htmlFor="alerts-filter-to" className={LABEL_CLASS}>
                To
              </label>
              <input
                id="alerts-filter-to"
                type="date"
                value={filters.to}
                onChange={onText("to")}
                className={INPUT_CLASS}
              />
            </div>
          </div>
        </div>
      </CardBody>
    </Card>
  );
}
