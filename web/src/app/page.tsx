"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import dynamic from "next/dynamic";
import Link from "next/link";
import { apiFetch } from "@/lib/api";
import type { DeviceStats, Trip } from "@/lib/types";
import {
  formatDistance,
  formatDuration,
  formatEngagementPct,
  formatTotalDuration,
} from "@/lib/format";
import { useAlprSettings } from "@/lib/useAlprSettings";
import { useAlertSummary } from "@/lib/useAlertSummary";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import { AlertBadge } from "@/components/alpr/AlertBadge";

// Lazy-load the recent-alerts widget so it does not block the home
// page's initial paint. The widget's GET /v1/alpr/alerts call is
// significantly more expensive than the summary endpoint that gates
// it; deferring keeps the >50ms-paint-regression budget in budget 6.
const RecentAlertsWidget = dynamic(
  () =>
    import("@/components/alpr/RecentAlertsWidget").then(
      (mod) => mod.RecentAlertsWidget,
    ),
  { ssr: false },
);

/**
 * Device list shape as returned by GET /v1/devices. Kept local to the
 * dashboard because other pages re-declare their own. The shape is also
 * duplicated in web/src/app/devices/page.tsx; refactor only if a third
 * consumer appears.
 */
interface DeviceListItem {
  dongleId: string;
  serial?: string;
  lastSeen?: string | null;
}

/**
 * Env-configurable fallback dongle id used before /v1/devices returns. Matches
 * the pattern used by the routes list page so both pages behave identically
 * with no configured device.
 */
const FALLBACK_DONGLE_ID =
  process.env.NEXT_PUBLIC_DONGLE_ID ?? "default";

/** How many recent drives to render in the dashboard section. */
const RECENT_LIMIT = 10;

function formatDriveDate(iso: string | null): string {
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

/**
 * A single lifetime-stats tile. Four of these sit side-by-side at the top of
 * the dashboard. The number is deliberately large and monospaced-ish to read
 * like a scoreboard.
 */
function StatTile({
  label,
  value,
  loading,
}: {
  label: string;
  value: string;
  loading?: boolean;
}) {
  return (
    <Card className="h-full">
      <CardBody className="flex flex-col gap-1">
        <span className="text-xs uppercase tracking-wide text-[var(--text-secondary)]">
          {label}
        </span>
        {loading ? (
          <div
            className="mt-1 h-8 w-24 animate-pulse rounded bg-[var(--bg-tertiary)]"
            aria-hidden="true"
          />
        ) : (
          <span className="text-2xl font-semibold text-[var(--text-primary)]">
            {value}
          </span>
        )}
      </CardBody>
    </Card>
  );
}

/** A single row in the recent-drives list. */
function RecentDriveRow({ trip }: { trip: Trip }) {
  const href = `/routes/${trip.dongle_id}/${encodeURIComponent(trip.route_name)}`;
  const hasEndpoints = Boolean(trip.start_address || trip.end_address);
  return (
    <Link
      href={href}
      className="block rounded-md transition-colors hover:bg-[var(--bg-tertiary)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]"
    >
      <div className="grid grid-cols-1 gap-2 px-3 py-3 sm:grid-cols-[minmax(0,1.5fr)_minmax(0,2fr)_minmax(0,0.75fr)_minmax(0,0.75fr)] sm:items-center sm:gap-4">
        <div className="min-w-0">
          <div className="text-sm font-medium text-[var(--text-primary)]">
            {formatDriveDate(trip.start_time)}
          </div>
          <div className="truncate font-mono text-xs text-[var(--text-secondary)]">
            {trip.route_name}
          </div>
        </div>
        <div className="min-w-0 text-sm text-[var(--text-secondary)]">
          {hasEndpoints ? (
            <span className="block truncate">
              <span className="text-[var(--text-primary)]">
                {trip.start_address ?? "Unknown start"}
              </span>
              <span className="mx-1 text-[var(--text-secondary)]">-&gt;</span>
              <span className="text-[var(--text-primary)]">
                {trip.end_address ?? "Unknown end"}
              </span>
            </span>
          ) : (
            <span className="italic">No address data</span>
          )}
        </div>
        <div className="text-sm tabular-nums text-[var(--text-primary)] sm:text-right">
          {formatDuration(trip.duration_seconds)}
        </div>
        <div className="text-sm tabular-nums text-[var(--text-primary)] sm:text-right">
          {formatDistance(trip.distance_meters)}
        </div>
      </div>
    </Link>
  );
}

/** A single skeleton row shown while recent drives are loading. */
function RecentDriveRowSkeleton() {
  return (
    <div className="grid grid-cols-1 gap-2 px-3 py-3 sm:grid-cols-[minmax(0,1.5fr)_minmax(0,2fr)_minmax(0,0.75fr)_minmax(0,0.75fr)] sm:items-center sm:gap-4">
      <div className="space-y-1.5">
        <div className="h-4 w-32 animate-pulse rounded bg-[var(--bg-tertiary)]" />
        <div className="h-3 w-24 animate-pulse rounded bg-[var(--bg-tertiary)]" />
      </div>
      <div className="h-4 w-full animate-pulse rounded bg-[var(--bg-tertiary)]" />
      <div className="h-4 w-12 animate-pulse rounded bg-[var(--bg-tertiary)] sm:ml-auto" />
      <div className="h-4 w-16 animate-pulse rounded bg-[var(--bg-tertiary)] sm:ml-auto" />
    </div>
  );
}

/**
 * Dashboard home page. Shows lifetime stats and the most recent drives for
 * the primary device. If multiple devices are registered, a switcher lets
 * the user pick which device the stats belong to.
 */
export default function Home() {
  const [devices, setDevices] = useState<DeviceListItem[]>([]);
  const [devicesLoading, setDevicesLoading] = useState(true);
  const [devicesError, setDevicesError] = useState<string | null>(null);
  const [selectedDongleId, setSelectedDongleId] = useState<string | null>(null);

  const [stats, setStats] = useState<DeviceStats | null>(null);
  const [statsLoading, setStatsLoading] = useState(false);
  const [statsError, setStatsError] = useState<string | null>(null);

  // ALPR badge + widget are runtime-gated on the master flag. When
  // the flag is loading or false, the alert-summary hook stays idle
  // (no fetch) and the badge / widget render nothing.
  const { enabled: alprEnabled } = useAlprSettings();
  const alprActive = alprEnabled === true;
  const { summary: alertSummary } = useAlertSummary(alprActive);
  const showAlprBadge =
    alprActive && (alertSummary?.open_count ?? 0) > 0;
  const showRecentAlerts = showAlprBadge;

  const fetchDevices = useCallback(async () => {
    setDevicesLoading(true);
    setDevicesError(null);
    try {
      const data = await apiFetch<DeviceListItem[]>("/v1/devices");
      setDevices(data);
      // Keep the current selection if it's still present, otherwise fall
      // back to the first device, or the env-configured default.
      setSelectedDongleId((current) => {
        if (current && data.some((d) => d.dongleId === current)) {
          return current;
        }
        if (data.length > 0) return data[0].dongleId;
        return FALLBACK_DONGLE_ID;
      });
    } catch (err) {
      setDevicesError(
        err instanceof Error ? err.message : "Failed to load devices",
      );
      // Still let stats try with the fallback id so a standalone install
      // with a single known device can render.
      setSelectedDongleId((current) => current ?? FALLBACK_DONGLE_ID);
    } finally {
      setDevicesLoading(false);
    }
  }, []);

  const fetchStats = useCallback(async (dongleId: string) => {
    setStatsLoading(true);
    setStatsError(null);
    try {
      const data = await apiFetch<DeviceStats>(
        `/v1/devices/${encodeURIComponent(dongleId)}/stats?limit=${RECENT_LIMIT}`,
      );
      setStats(data);
    } catch (err) {
      setStatsError(err instanceof Error ? err.message : "Failed to load stats");
      setStats(null);
    } finally {
      setStatsLoading(false);
    }
  }, []);

  useEffect(() => {
    void fetchDevices();
  }, [fetchDevices]);

  useEffect(() => {
    if (!selectedDongleId) return;
    void fetchStats(selectedDongleId);
  }, [selectedDongleId, fetchStats]);

  const totalsLoading = statsLoading && !stats;
  const recentLoading = statsLoading && !stats;
  const showMultipleDevices = devices.length > 1;

  const { distanceText, driveTimeText, engagementText, driveCountText } =
    useMemo(() => {
      const totals = stats?.totals;
      return {
        distanceText: formatDistance(totals?.total_distance_meters ?? 0),
        driveTimeText: formatTotalDuration(totals?.total_duration_seconds ?? 0),
        engagementText: formatEngagementPct(
          totals?.total_engaged_seconds,
          totals?.total_duration_seconds,
        ),
        driveCountText: totals ? totals.trip_count.toLocaleString() : "--",
      };
    }, [stats]);

  const deviceDescription = selectedDongleId
    ? `Lifetime stats for ${selectedDongleId}`
    : "Lifetime stats";

  const retryStats = useCallback(() => {
    if (selectedDongleId) void fetchStats(selectedDongleId);
  }, [selectedDongleId, fetchStats]);

  return (
    <PageWrapper title="Dashboard" description={deviceDescription}>
      {/* ALPR open-alerts badge. Sits at the top of the dashboard so
          a freshly-opened tab surfaces an active alert before the
          user even reads the lifetime stats. The badge is responsive
          on its own (the underlying pill flexes), so no extra mobile
          styling is needed at this level. */}
      {showAlprBadge && (
        <div className="mb-5 flex flex-wrap items-center gap-2">
          <AlertBadge summary={alertSummary} />
        </div>
      )}

      {showMultipleDevices && (
        <div className="mb-5 flex flex-wrap items-center gap-2">
          <label
            htmlFor="dashboard-device"
            className="text-sm text-[var(--text-secondary)]"
          >
            Device
          </label>
          <select
            id="dashboard-device"
            value={selectedDongleId ?? ""}
            onChange={(e) => setSelectedDongleId(e.target.value)}
            className="rounded-md border border-[var(--border-primary)] bg-[var(--bg-surface)] px-2 py-1 text-sm text-[var(--text-primary)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]"
          >
            {devices.map((d) => (
              <option key={d.dongleId} value={d.dongleId}>
                {d.dongleId}
              </option>
            ))}
          </select>
        </div>
      )}

      {devicesError && !devicesLoading && (
        <div className="mb-5">
          <ErrorMessage
            title="Failed to load devices"
            message={devicesError}
            retry={fetchDevices}
          />
        </div>
      )}

      {/* Lifetime stats -- 4 tiles */}
      <div className="mb-6 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <StatTile
          label="Total distance"
          value={distanceText}
          loading={totalsLoading}
        />
        <StatTile
          label="Drive time"
          value={driveTimeText}
          loading={totalsLoading}
        />
        <StatTile
          label="Engagement"
          value={engagementText}
          loading={totalsLoading}
        />
        <StatTile
          label="Drives"
          value={driveCountText}
          loading={totalsLoading}
        />
      </div>

      {/* Recent open alerts. Lazy-loaded (next/dynamic) so the
          chunk does not cost the home page's first paint. The
          summary endpoint already gates the badge above; we mount
          the widget on the same condition so it never fetches the
          (more expensive) full alerts list when there are none. */}
      {showRecentAlerts && (
        <div className="mb-6">
          <RecentAlertsWidget />
        </div>
      )}

      {/* Recent drives */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between gap-2">
            <h2 className="text-subheading">Recent drives</h2>
            {selectedDongleId && (
              <Link
                href={`/routes?device=${encodeURIComponent(selectedDongleId)}`}
                className="text-sm text-[var(--accent)] hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)] rounded"
              >
                View all
              </Link>
            )}
          </div>
        </CardHeader>
        <CardBody className="px-2 py-2">
          {statsError && !recentLoading && (
            <div className="p-2">
              <ErrorMessage
                title="Failed to load stats"
                message={statsError}
                retry={retryStats}
              />
            </div>
          )}

          {!statsError && recentLoading && (
            <ul className="divide-y divide-[var(--border-primary)]">
              {Array.from({ length: 4 }).map((_, i) => (
                <li key={i}>
                  <RecentDriveRowSkeleton />
                </li>
              ))}
            </ul>
          )}

          {!statsError &&
            !recentLoading &&
            stats &&
            stats.recent.length === 0 && (
              <div className="px-3 py-8 text-center">
                <p className="text-caption">
                  No drives yet. Once your device uploads driving data, trips
                  will show up here.
                </p>
                <div className="mt-3">
                  <Link
                    href="/routes"
                    className="text-sm text-[var(--accent)] hover:underline"
                  >
                    Browse all routes
                  </Link>
                </div>
              </div>
            )}

          {!statsError &&
            !recentLoading &&
            stats &&
            stats.recent.length > 0 && (
              <ul className="divide-y divide-[var(--border-primary)]">
                {stats.recent.map((trip) => (
                  <li key={trip.id}>
                    <RecentDriveRow trip={trip} />
                  </li>
                ))}
              </ul>
            )}
        </CardBody>
      </Card>
    </PageWrapper>
  );
}
