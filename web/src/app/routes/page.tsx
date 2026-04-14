"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { apiFetch } from "@/lib/api";
import type { RouteListItem, RouteListResponse } from "@/lib/types";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Badge } from "@/components/ui/Badge";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";

/**
 * Default dongle ID used when none is configured.
 * Set NEXT_PUBLIC_DONGLE_ID to target a specific device.
 */
const DONGLE_ID = process.env.NEXT_PUBLIC_DONGLE_ID ?? "default";

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

export default function RoutesPage() {
  const [routes, setRoutes] = useState<RouteListItem[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchRoutes = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await apiFetch<RouteListResponse>(
        `/v1/route/${DONGLE_ID}?limit=50`,
      );
      setRoutes(data.routes);
      setTotal(data.total);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load routes");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void fetchRoutes();
  }, [fetchRoutes]);

  return (
    <PageWrapper
      title="Routes"
      description={
        loading
          ? "Loading routes..."
          : `${total} route${total === 1 ? "" : "s"} recorded`
      }
    >
      {loading && (
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading routes" />
        </div>
      )}

      {error && (
        <ErrorMessage
          title="Failed to load routes"
          message={error}
          retry={fetchRoutes}
        />
      )}

      {!loading && !error && routes.length === 0 && (
        <Card>
          <CardBody>
            <p className="text-center text-caption py-8">
              No routes found. Routes will appear here once your device uploads
              driving data.
            </p>
          </CardBody>
        </Card>
      )}

      {!loading && !error && routes.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {routes.map((route) => (
            <Link
              key={`${route.dongleId}|${route.routeName}`}
              href={`/routes/${route.dongleId}/${encodeURIComponent(route.routeName)}`}
              className="block transition-transform hover:scale-[1.01] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)] rounded-lg"
            >
              <Card className="h-full">
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
                      <dt className="text-[var(--text-secondary)]">Duration</dt>
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
      )}
    </PageWrapper>
  );
}
