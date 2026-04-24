"use client";

import { Suspense, useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { apiFetch } from "@/lib/api";
import { useDebouncedValue } from "@/lib/useDebouncedValue";
import type { RouteListItem, RouteListResponse } from "@/lib/types";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import {
  EMPTY_FILTERS,
  FilterBar,
  filtersFromSearchParams,
  filtersToSearchParams,
  isDefaultFilters,
  type FilterState,
} from "@/components/routes/FilterBar";
import { RouteThumbnail } from "@/components/routes/RouteThumbnail";

/**
 * Default dongle ID used when none is configured.
 * Set NEXT_PUBLIC_DONGLE_ID to target a specific device.
 */
const DONGLE_ID = process.env.NEXT_PUBLIC_DONGLE_ID ?? "default";

const PAGE_LIMIT = 50;

// The filter bar updates state on every keystroke, but we only want to
// refetch and rewrite the URL after the user pauses. 300ms matches the
// budget called out in the acceptance criteria.
const FILTER_DEBOUNCE_MS = 300;

function formatDate(iso: string | null): string {
  if (!iso) return "Unknown date";
  const d = new Date(iso);
  return d.toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function formatDuration(start: string | null, end: string | null): string {
  if (!start || !end) return "--";
  const ms = new Date(end).getTime() - new Date(start).getTime();
  if (ms <= 0) return "--";
  const totalSeconds = Math.floor(ms / 1000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes === 0) return `${seconds}s`;
  return `${minutes}m ${seconds}s`;
}

function RoutesPageInner() {
  const router = useRouter();
  const searchParams = useSearchParams();

  // Hydrate the filter state from the URL on first render. This means
  // reloading the page (or opening a shared link) reconstructs the same
  // filtered view the author saw.
  const [filters, setFilters] = useState<FilterState>(() =>
    filtersFromSearchParams(new URLSearchParams(searchParams.toString())),
  );

  const [routes, setRoutes] = useState<RouteListItem[]>([]);
  const [total, setTotal] = useState(0);
  const [offset, setOffset] = useState(0);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The debounced copy of filters drives both the URL rewrite and the
  // fetch. Typing in a text input or dragging the date picker therefore
  // stops hammering the backend once the user pauses for 300ms.
  const debouncedFilters = useDebouncedValue(filters, FILTER_DEBOUNCE_MS);

  const filterParams = useMemo(
    () => filtersToSearchParams(debouncedFilters),
    [debouncedFilters],
  );

  // Sync debounced filter state to the URL. We stringify so the value is
  // stable across renders, avoiding the useEffect re-run loop that would
  // come from putting filterParams directly in the dep array.
  const filterQuery = filterParams.toString();
  useEffect(() => {
    router.replace(filterQuery ? `/routes?${filterQuery}` : "/routes", {
      scroll: false,
    });
  }, [filterQuery, router]);

  // Any filter change resets pagination to offset 0 so the user isn't
  // stuck on page 3 of a now-narrower result set.
  useEffect(() => {
    setOffset(0);
  }, [filterQuery]);

  const fetchRoutes = useCallback(
    async (nextOffset: number, append: boolean) => {
      if (append) {
        setLoadingMore(true);
      } else {
        setLoading(true);
      }
      setError(null);
      try {
        const query = new URLSearchParams(filterParams);
        query.set("limit", String(PAGE_LIMIT));
        query.set("offset", String(nextOffset));
        const data = await apiFetch<RouteListResponse>(
          `/v1/route/${DONGLE_ID}?${query.toString()}`,
        );
        setTotal(data.total);
        setRoutes((prev) => (append ? [...prev, ...data.routes] : data.routes));
      } catch (err) {
        setError(err instanceof Error ? err.message : "Failed to load routes");
      } finally {
        setLoading(false);
        setLoadingMore(false);
      }
    },
    [filterParams],
  );

  // Re-fetch from offset 0 whenever the debounced filters change. The
  // inner fetchRoutes has filterParams in its closure so this effect is
  // the only place we trigger a reset-style load.
  useEffect(() => {
    void fetchRoutes(0, false);
  }, [fetchRoutes]);

  const handleClear = useCallback(() => {
    setFilters(EMPTY_FILTERS);
  }, []);

  const handleLoadMore = useCallback(() => {
    const next = offset + PAGE_LIMIT;
    setOffset(next);
    void fetchRoutes(next, true);
  }, [fetchRoutes, offset]);

  const hasMore = routes.length < total;
  const filtersActive = !isDefaultFilters(filters);

  return (
    <PageWrapper
      title="Routes"
      description={
        loading
          ? "Loading routes..."
          : filtersActive
            ? `${total} route${total === 1 ? "" : "s"} match current filters`
            : `${total} route${total === 1 ? "" : "s"} recorded`
      }
    >
      <FilterBar
        filters={filters}
        onChange={setFilters}
        onClear={handleClear}
      />

      {loading && (
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading routes" />
        </div>
      )}

      {error && (
        <ErrorMessage
          title="Failed to load routes"
          message={error}
          retry={() => {
            void fetchRoutes(offset, false);
          }}
        />
      )}

      {!loading && !error && routes.length === 0 && (
        <Card>
          <CardBody>
            <div className="py-8 text-center">
              <p className="text-caption">
                {filtersActive
                  ? "No routes match these filters."
                  : "No routes found. Routes will appear here once your device uploads driving data."}
              </p>
              {filtersActive && (
                <div className="mt-4 flex justify-center">
                  <Button variant="secondary" size="sm" onClick={handleClear}>
                    Clear filters
                  </Button>
                </div>
              )}
            </div>
          </CardBody>
        </Card>
      )}

      {!loading && !error && routes.length > 0 && (
        <>
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {routes.map((route) => (
              <Link
                key={`${route.dongleId}|${route.routeName}`}
                href={`/routes/${route.dongleId}/${encodeURIComponent(route.routeName)}`}
                className="block transition-transform hover:scale-[1.01] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)] rounded-lg"
              >
                <Card className="h-full overflow-hidden">
                  <RouteThumbnail
                    dongleId={route.dongleId}
                    routeName={route.routeName}
                    variant="list"
                    className="rounded-none"
                  />
                  <CardHeader>
                    <div className="flex items-center justify-between gap-2">
                      <h2 className="text-sm font-medium text-[var(--text-primary)] truncate">
                        {route.routeName}
                      </h2>
                      <Badge variant="info">
                        {route.segmentCount}{" "}
                        {route.segmentCount === 1 ? "seg" : "segs"}
                      </Badge>
                    </div>
                  </CardHeader>
                  <CardBody>
                    <dl className="space-y-1 text-sm">
                      <div className="flex justify-between">
                        <dt className="text-[var(--text-secondary)]">Date</dt>
                        <dd className="text-[var(--text-primary)]">
                          {formatDate(route.startTime)}
                        </dd>
                      </div>
                      <div className="flex justify-between">
                        <dt className="text-[var(--text-secondary)]">
                          Duration
                        </dt>
                        <dd className="text-[var(--text-primary)]">
                          {formatDuration(route.startTime, route.endTime)}
                        </dd>
                      </div>
                      <div className="flex justify-between">
                        <dt className="text-[var(--text-secondary)]">Device</dt>
                        <dd className="font-mono text-xs text-[var(--text-secondary)]">
                          {route.dongleId}
                        </dd>
                      </div>
                    </dl>
                  </CardBody>
                </Card>
              </Link>
            ))}
          </div>

          {hasMore && (
            <div className="mt-4 flex justify-center">
              <Button
                variant="secondary"
                size="md"
                onClick={handleLoadMore}
                disabled={loadingMore}
              >
                {loadingMore ? "Loading..." : "Load more"}
              </Button>
            </div>
          )}
        </>
      )}
    </PageWrapper>
  );
}

export default function RoutesPage() {
  // useSearchParams requires a Suspense boundary under Next 15 when the
  // page is statically optimized; wrap so the build doesn't trip.
  return (
    <Suspense
      fallback={
        <PageWrapper title="Routes" description="Loading routes...">
          <div className="flex items-center justify-center py-16">
            <Spinner size="lg" label="Loading" />
          </div>
        </PageWrapper>
      }
    >
      <RoutesPageInner />
    </Suspense>
  );
}
