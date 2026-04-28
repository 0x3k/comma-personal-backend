"use client";

import { Suspense, useCallback, useEffect, useMemo, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { apiFetch } from "@/lib/api";
import { useAlprSettings } from "@/lib/useAlprSettings";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardBody } from "@/components/ui/Card";
import { Spinner } from "@/components/ui/Spinner";
import { AlertsFilterBar } from "@/components/alerts/AlertsFilterBar";
import { AlertsTab } from "@/components/alerts/AlertsTab";
import { WhitelistTab } from "@/components/alerts/WhitelistTab";
import {
  EMPTY_ALERTS_FILTERS,
  alertsQueryString,
  filtersFromSearchParams,
  parseTab,
  type AlertsFilterState,
  type AlertsTab as AlertsTabId,
} from "@/components/alerts/filters";

interface Device {
  dongleId: string;
}

const TABS: { id: AlertsTabId; label: string }[] = [
  { id: "open", label: "Open" },
  { id: "acked", label: "Acknowledged" },
  { id: "whitelist", label: "Whitelist" },
];

/**
 * AlertsPageInner is the body of the alerts page. The Suspense boundary
 * lives in the default export because useSearchParams demands one under
 * Next 15 when the page is statically optimized.
 *
 * Rendering rules:
 * - While the alpr feature flag is loading, render a spinner. The hook
 *   caches after the first fetch so subsequent navigations within the
 *   same session show the page synchronously.
 * - When the flag resolves to false, render a "feature disabled" card
 *   instead of the tabs. This mirrors how PlateTimeline gates itself
 *   off, so the page never flashes a half-rendered alert UI for a
 *   deployment that hasn't enabled ALPR.
 * - When enabled, hydrate filters + active tab from the URL on first
 *   render, then mirror back to the URL on every change.
 */
function AlertsPageInner() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const { enabled, loading: flagLoading } = useAlprSettings();

  // Hydrate from URL once; subsequent URL changes (back-button) are
  // picked up via the searchParams effect below.
  const [tab, setTab] = useState<AlertsTabId>(() =>
    parseTab(searchParams.get("tab")),
  );
  const [filters, setFilters] = useState<AlertsFilterState>(() =>
    filtersFromSearchParams(new URLSearchParams(searchParams.toString())),
  );

  // Re-hydrate filter + tab state when the URL changes externally
  // (back/forward navigation). We compare by stringified URL params
  // rather than the searchParams ref because Next.js can re-render with
  // a fresh ref that holds the same content; we only want to overwrite
  // local state when the actual values differ.
  const urlString = searchParams.toString();
  useEffect(() => {
    const sp = new URLSearchParams(urlString);
    const nextTab = parseTab(sp.get("tab"));
    const nextFilters = filtersFromSearchParams(sp);
    setTab((prev) => (prev === nextTab ? prev : nextTab));
    setFilters((prev) => {
      const same =
        prev.dongle_id === nextFilters.dongle_id &&
        prev.from === nextFilters.from &&
        prev.to === nextFilters.to &&
        prev.severity.length === nextFilters.severity.length &&
        prev.severity.every((s, i) => s === nextFilters.severity[i]);
      return same ? prev : nextFilters;
    });
  }, [urlString]);

  // Mirror state back to URL. The query string format matches the
  // filtersToSearchParams helper so a deep-link reload reconstructs
  // the same view.
  const targetQuery = useMemo(
    () => alertsQueryString(tab, filters),
    [tab, filters],
  );
  useEffect(() => {
    // Only write to the URL when state actually changes -- depending
    // on urlString here would race with the hydrate effect: when the
    // URL changes externally (back-button), hydrate parses the new
    // URL into state, but on that same render the state hasn't yet
    // updated, so mirror would stomp the freshly-arrived URL with
    // the stale state's serialization.
    //
    // Reading the live URL via window.location keeps the no-op fast
    // path (skip when the current URL already matches) without
    // creating a dependency cycle.
    if (typeof window !== "undefined") {
      const currentSearch = window.location.search.startsWith("?")
        ? window.location.search.slice(1)
        : window.location.search;
      if (currentSearch === targetQuery) return;
    }
    router.replace(targetQuery ? `/alerts?${targetQuery}` : "/alerts", {
      scroll: false,
    });
    // router is a stable ref under Next.js's app router but not under
    // our test mock; intentionally omitted so a mock that returns a
    // fresh router each render does not retrigger this effect every
    // render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [targetQuery]);

  // Device list: fetched once. Drives the dongle filter dropdown.
  // Failure is non-fatal -- the alerts list still works (the filter
  // just hides). We swallow the error rather than show a banner.
  const [availableDongles, setAvailableDongles] = useState<string[]>([]);
  useEffect(() => {
    let cancelled = false;
    apiFetch<Device[]>("/v1/devices")
      .then((data) => {
        if (cancelled) return;
        setAvailableDongles(data.map((d) => d.dongleId));
      })
      .catch(() => {
        // Best-effort. The selector falls back to "no selector".
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const onTabChange = useCallback((next: AlertsTabId) => {
    setTab(next);
  }, []);

  // -------------------------------------------------------------------
  // Render branches
  // -------------------------------------------------------------------

  if (flagLoading) {
    return (
      <PageWrapper title="Alerts">
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading" />
        </div>
      </PageWrapper>
    );
  }

  if (enabled !== true) {
    // Feature flag is off (or the settings fetch failed). Render a
    // clean banner rather than empty alert tabs so the operator
    // understands why nothing is here.
    return (
      <PageWrapper title="Alerts">
        <Card>
          <CardBody>
            <div className="py-8 text-center">
              <p className="text-sm font-medium text-[var(--text-primary)]">
                ALPR is disabled
              </p>
              <p className="mt-1 text-xs text-[var(--text-secondary)]">
                Enable ALPR in Settings to surface plate-based alerts.
              </p>
            </div>
          </CardBody>
        </Card>
      </PageWrapper>
    );
  }

  return (
    <PageWrapper
      title="Alerts"
      description="Plates flagged by the watchlist heuristic."
    >
      {/* Tab bar. Three buttons + a thin underline for the active tab.
          Aria-selected is set on the active button so screen readers
          announce the state clearly. */}
      <div
        role="tablist"
        aria-label="Alert center sections"
        className="mb-4 flex items-center gap-1 border-b border-[var(--border-primary)]"
      >
        {TABS.map((t) => {
          const active = t.id === tab;
          return (
            <button
              key={t.id}
              role="tab"
              type="button"
              aria-selected={active}
              data-testid={`alerts-tab-${t.id}`}
              onClick={() => onTabChange(t.id)}
              className={[
                "px-3 py-2 text-sm font-medium transition-colors",
                "border-b-2 -mb-px",
                active
                  ? "border-[var(--accent)] text-[var(--text-primary)]"
                  : "border-transparent text-[var(--text-secondary)] hover:text-[var(--text-primary)]",
              ].join(" ")}
            >
              {t.label}
            </button>
          );
        })}
      </div>

      {/* Filter bar -- only on Open + Acknowledged. The whitelist tab
          doesn't take filters per the spec. */}
      {tab !== "whitelist" && (
        <AlertsFilterBar
          filters={filters}
          onChange={setFilters}
          availableDongles={availableDongles}
        />
      )}

      {/* Tab body. Re-mounting on tab switch is intentional so the
          AlertsTab's row state (selection, in-flight) resets between
          views. The whitelist tab keeps a separate component because
          its data shape and behaviour are different (no virtualization,
          no bulk operations). */}
      {tab === "open" && (
        <AlertsTab
          mode="open"
          filters={filters}
          emptyTitle="No open alerts"
          emptyDescription="Alerts trigger when the same vehicle appears across multiple trips in suspicious patterns."
        />
      )}

      {tab === "acked" && (
        <AlertsTab
          mode="acked"
          filters={filters}
          emptyTitle="No acknowledged alerts yet"
        />
      )}

      {tab === "whitelist" && <WhitelistTab />}
    </PageWrapper>
  );
}

export default function AlertsPage() {
  return (
    <Suspense
      fallback={
        <PageWrapper title="Alerts">
          <div className="flex items-center justify-center py-16">
            <Spinner size="lg" label="Loading" />
          </div>
        </PageWrapper>
      }
    >
      <AlertsPageInner />
    </Suspense>
  );
}
