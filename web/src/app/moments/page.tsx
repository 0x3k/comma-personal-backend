"use client";

import { Suspense, useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { apiFetch } from "@/lib/api";
import type {
  MomentEvent,
  MomentRoute,
  MomentRoutesResponse,
  MomentsListResponse,
} from "@/lib/types";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";
import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import { Button } from "@/components/ui/Button";

// Known event types the detector can emit. Must match the EventType*
// constants in internal/worker/event_detector.go -- the backend filters by
// exact string match, so a mismatch silently returns zero rows.
const KNOWN_EVENT_TYPES = [
  "hard_brake",
  "disengage",
  "fcw",
  "alert_warning",
] as const;

// Severities the detector stamps on events. Must match the EventSeverity*
// constants in internal/worker/event_detector.go.
const KNOWN_SEVERITIES = ["info", "warn"] as const;
type Severity = (typeof KNOWN_SEVERITIES)[number];

// Page sizes for the route list. The backend's max is 500
// (internal/api/events.go) but listing routes is cheap so we don't need to
// expose the upper end.
const LIMIT_OPTIONS = [10, 25, 50, 100] as const;
const DEFAULT_LIMIT = 25;

// Cap on events fetched per expanded route. Most routes have well under
// this; the few outliers (thousands of alert_warnings) get truncated and a
// "... and N more" footer renders so users can drill into the route page.
const EVENTS_PER_ROUTE_LIMIT = 100;

interface Device {
  dongleId: string;
}

function severityBadgeVariant(severity: string): BadgeVariant {
  switch (severity) {
    case "error":
      return "error";
    case "warn":
    case "warning":
      return "warning";
    case "info":
      return "info";
    default:
      return "neutral";
  }
}

function typeBadgeVariant(type: string): BadgeVariant {
  // Keep a consistent colour per known type so the eye can skim the list.
  switch (type) {
    case "hard_brake":
      return "error";
    case "disengage":
      return "warning";
    case "fcw":
      return "error";
    case "alert_warning":
      return "info";
    default:
      return "neutral";
  }
}

function formatOffset(seconds: number): string {
  if (!Number.isFinite(seconds)) return "--";
  const total = Math.max(0, Math.floor(seconds));
  const m = Math.floor(total / 60);
  const s = total % 60;
  return `${m.toString().padStart(2, "0")}:${s.toString().padStart(2, "0")}`;
}

function formatDateTime(iso: string | null): string {
  if (!iso) return "Unknown";
  const d = new Date(iso);
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function formatRouteTimeRange(
  start: string | null,
  end: string | null,
): string {
  if (!start) return "Unknown time";
  const s = new Date(start);
  const startStr = s.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
  if (!end) return startStr;
  const e = new Date(end);
  const sameDay =
    s.getFullYear() === e.getFullYear() &&
    s.getMonth() === e.getMonth() &&
    s.getDate() === e.getDate();
  const endStr = sameDay
    ? e.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" })
    : e.toLocaleString(undefined, {
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      });
  return `${startStr} -- ${endStr}`;
}

function summarizePayload(payload: unknown): string {
  if (payload === null || payload === undefined) return "";
  if (typeof payload !== "object") return String(payload);
  const entries = Object.entries(payload as Record<string, unknown>);
  if (entries.length === 0) return "";
  return entries
    .slice(0, 2)
    .map(([k, v]) => `${k}=${formatPayloadValue(v)}`)
    .join(" ");
}

function formatPayloadValue(v: unknown): string {
  if (typeof v === "number") {
    if (!Number.isFinite(v)) return String(v);
    return Math.abs(v) >= 10 ? v.toFixed(1) : v.toFixed(2);
  }
  if (typeof v === "string") return v;
  if (typeof v === "boolean") return v ? "true" : "false";
  if (v === null) return "null";
  return JSON.stringify(v);
}

interface RouteEventsState {
  events: MomentEvent[];
  total: number;
  loading: boolean;
  error: string | null;
}

function MomentsPageInner() {
  const router = useRouter();
  const params = useSearchParams();

  // Device selection: remembers the current choice in the URL so a deep
  // link to a filtered Moments view survives a refresh or share.
  const [devices, setDevices] = useState<Device[]>([]);
  const [devicesLoaded, setDevicesLoaded] = useState(false);
  const [deviceError, setDeviceError] = useState<string | null>(null);

  const urlDongle = params.get("device") ?? "";
  const [selectedDongle, setSelectedDongle] = useState<string>(urlDongle);

  // Drop URL values that don't match the current detector vocabulary --
  // older shareable links sent disengagement / alert / warning, which no
  // event row carries and which would otherwise show up as invisible
  // filters that hide every event.
  const urlTypes = params
    .getAll("type")
    .filter((t) => (KNOWN_EVENT_TYPES as readonly string[]).includes(t));
  const rawSeverity = params.get("severity") ?? "";
  const urlSeverity = (KNOWN_SEVERITIES as readonly string[]).includes(rawSeverity)
    ? (rawSeverity as Severity)
    : "";
  const urlLimitRaw = parseInt(params.get("limit") ?? "", 10);
  const urlLimit = LIMIT_OPTIONS.includes(urlLimitRaw as (typeof LIMIT_OPTIONS)[number])
    ? (urlLimitRaw as (typeof LIMIT_OPTIONS)[number])
    : DEFAULT_LIMIT;
  const urlOffsetRaw = parseInt(params.get("offset") ?? "", 10);
  const urlOffset = Number.isFinite(urlOffsetRaw) && urlOffsetRaw >= 0 ? urlOffsetRaw : 0;

  const [selectedTypes, setSelectedTypes] = useState<string[]>(urlTypes);
  const [selectedSeverity, setSelectedSeverity] = useState<"" | Severity>(urlSeverity);
  const [limit, setLimit] = useState<number>(urlLimit);
  const [offset, setOffset] = useState<number>(urlOffset);

  const [routes, setRoutes] = useState<MomentRoute[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Per-route expansion state and a cache of fetched events keyed by
  // route_name. We keep results around so collapsing then re-expanding is
  // instant.
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [routeEvents, setRouteEvents] = useState<Record<string, RouteEventsState>>({});

  // Fetch the device list so the user can pick which device to browse.
  useEffect(() => {
    let cancelled = false;
    async function loadDevices() {
      setDeviceError(null);
      try {
        const data = await apiFetch<Device[]>("/v1/devices");
        if (cancelled) return;
        setDevices(data);
        if (!selectedDongle) {
          const fallback =
            process.env.NEXT_PUBLIC_DONGLE_ID ??
            (data.length === 1 ? data[0].dongleId : "");
          if (fallback) {
            setSelectedDongle(fallback);
          }
        }
      } catch (err) {
        if (!cancelled) {
          setDeviceError(
            err instanceof Error ? err.message : "Failed to load devices",
          );
        }
      } finally {
        if (!cancelled) setDevicesLoaded(true);
      }
    }
    void loadDevices();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Sync filter state to the URL so the view is shareable.
  useEffect(() => {
    const sp = new URLSearchParams();
    if (selectedDongle) sp.set("device", selectedDongle);
    for (const t of selectedTypes) sp.append("type", t);
    if (selectedSeverity) sp.set("severity", selectedSeverity);
    if (limit !== DEFAULT_LIMIT) sp.set("limit", String(limit));
    if (offset > 0) sp.set("offset", String(offset));
    const query = sp.toString();
    router.replace(query ? `/moments?${query}` : "/moments", { scroll: false });
  }, [selectedDongle, selectedTypes, selectedSeverity, limit, offset, router]);

  // Fetch the routes-with-events list. When multiple type filters are
  // active we dispatch one request per type and merge client-side; the
  // backend filter supports a single exact-match type. Routes shared
  // across multiple type filters are deduped on route_name and their
  // event_count / type_counts are summed so the user sees "this route has
  // N events across the selected types".
  const fetchRoutes = useCallback(async () => {
    if (!selectedDongle) {
      setRoutes([]);
      setTotal(0);
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const baseQuery = new URLSearchParams();
      baseQuery.set("limit", String(limit));
      baseQuery.set("offset", String(offset));
      if (selectedSeverity) baseQuery.set("severity", selectedSeverity);

      if (selectedTypes.length <= 1) {
        if (selectedTypes.length === 1) baseQuery.set("type", selectedTypes[0]);
        const data = await apiFetch<MomentRoutesResponse>(
          `/v1/devices/${encodeURIComponent(selectedDongle)}/event-routes?${baseQuery.toString()}`,
        );
        setRoutes(data.routes);
        setTotal(data.total);
      } else {
        const perType = await Promise.all(
          selectedTypes.map(async (t) => {
            const q = new URLSearchParams(baseQuery);
            q.set("type", t);
            return apiFetch<MomentRoutesResponse>(
              `/v1/devices/${encodeURIComponent(selectedDongle)}/event-routes?${q.toString()}`,
            );
          }),
        );
        const byName = new Map<string, MomentRoute>();
        let totalCount = 0;
        for (const page of perType) {
          totalCount += page.total;
          for (const r of page.routes) {
            const existing = byName.get(r.route_name);
            if (!existing) {
              byName.set(r.route_name, { ...r, type_counts: { ...r.type_counts } });
            } else {
              existing.event_count += r.event_count;
              for (const [k, v] of Object.entries(r.type_counts)) {
                existing.type_counts[k] = (existing.type_counts[k] ?? 0) + v;
              }
              if (
                r.last_event_at &&
                (!existing.last_event_at ||
                  new Date(r.last_event_at).getTime() >
                    new Date(existing.last_event_at).getTime())
              ) {
                existing.last_event_at = r.last_event_at;
              }
            }
          }
        }
        const merged = Array.from(byName.values()).sort((a, b) => {
          const at = a.last_event_at ? new Date(a.last_event_at).getTime() : 0;
          const bt = b.last_event_at ? new Date(b.last_event_at).getTime() : 0;
          return bt - at;
        });
        setRoutes(merged.slice(0, limit));
        // totalCount across types overcounts shared routes; use the
        // deduped length as a lower bound. This matches what the user
        // sees rather than the sum of per-type totals.
        setTotal(Math.max(merged.length, totalCount));
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load events");
    } finally {
      setLoading(false);
    }
  }, [selectedDongle, selectedTypes, selectedSeverity, limit, offset]);

  useEffect(() => {
    void fetchRoutes();
  }, [fetchRoutes]);

  // Reset offset and clear the per-route cache whenever filters change so
  // we don't leave stale event lists behind.
  useEffect(() => {
    setOffset(0);
    setExpanded(new Set());
    setRouteEvents({});
  }, [selectedDongle, selectedTypes, selectedSeverity]);

  const toggleType = useCallback((type: string) => {
    setSelectedTypes((prev) =>
      prev.includes(type) ? prev.filter((t) => t !== type) : [...prev, type],
    );
  }, []);

  const clearFilters = useCallback(() => {
    setSelectedTypes([]);
    setSelectedSeverity("");
    setLimit(DEFAULT_LIMIT);
    setOffset(0);
  }, []);

  const fetchRouteEvents = useCallback(
    async (routeName: string) => {
      if (!selectedDongle) return;
      setRouteEvents((prev) => ({
        ...prev,
        [routeName]: {
          events: prev[routeName]?.events ?? [],
          total: prev[routeName]?.total ?? 0,
          loading: true,
          error: null,
        },
      }));
      try {
        const q = new URLSearchParams();
        q.set("limit", String(EVENTS_PER_ROUTE_LIMIT));
        q.set("route_name", routeName);
        if (selectedSeverity) q.set("severity", selectedSeverity);
        // For multi-type, fetch each and merge; same dance as the routes
        // list. Single-type or no-type uses one request.
        if (selectedTypes.length <= 1) {
          if (selectedTypes.length === 1) q.set("type", selectedTypes[0]);
          const data = await apiFetch<MomentsListResponse>(
            `/v1/devices/${encodeURIComponent(selectedDongle)}/events?${q.toString()}`,
          );
          setRouteEvents((prev) => ({
            ...prev,
            [routeName]: {
              events: data.events,
              total: data.total,
              loading: false,
              error: null,
            },
          }));
        } else {
          const perType = await Promise.all(
            selectedTypes.map(async (t) => {
              const qq = new URLSearchParams(q);
              qq.set("type", t);
              return apiFetch<MomentsListResponse>(
                `/v1/devices/${encodeURIComponent(selectedDongle)}/events?${qq.toString()}`,
              );
            }),
          );
          const merged: MomentEvent[] = [];
          const seen = new Set<number>();
          let tot = 0;
          for (const page of perType) {
            tot += page.total;
            for (const e of page.events) {
              if (!seen.has(e.id)) {
                seen.add(e.id);
                merged.push(e);
              }
            }
          }
          merged.sort((a, b) => {
            const at = a.occurred_at ? new Date(a.occurred_at).getTime() : 0;
            const bt = b.occurred_at ? new Date(b.occurred_at).getTime() : 0;
            if (bt !== at) return bt - at;
            return b.id - a.id;
          });
          setRouteEvents((prev) => ({
            ...prev,
            [routeName]: {
              events: merged.slice(0, EVENTS_PER_ROUTE_LIMIT),
              total: tot,
              loading: false,
              error: null,
            },
          }));
        }
      } catch (err) {
        setRouteEvents((prev) => ({
          ...prev,
          [routeName]: {
            events: prev[routeName]?.events ?? [],
            total: prev[routeName]?.total ?? 0,
            loading: false,
            error:
              err instanceof Error ? err.message : "Failed to load events",
          },
        }));
      }
    },
    [selectedDongle, selectedTypes, selectedSeverity],
  );

  const toggleRoute = useCallback(
    (routeName: string) => {
      setExpanded((prev) => {
        const next = new Set(prev);
        if (next.has(routeName)) {
          next.delete(routeName);
        } else {
          next.add(routeName);
          // Lazy-fetch if we don't have events for this route yet (or if
          // the cached entry has an error so the user can retry).
          const cached = routeEvents[routeName];
          if (!cached || cached.error) {
            void fetchRouteEvents(routeName);
          }
        }
        return next;
      });
    },
    [routeEvents, fetchRouteEvents],
  );

  const pageStart = total === 0 ? 0 : offset + 1;
  const pageEnd = Math.min(offset + routes.length, offset + limit);
  const hasPrev = offset > 0;
  const hasNext = offset + routes.length < total;

  const goPrev = useCallback(() => {
    setOffset((o) => Math.max(0, o - limit));
  }, [limit]);
  const goNext = useCallback(() => {
    setOffset((o) => o + limit);
  }, [limit]);

  const showDevicePicker = devices.length > 1;

  return (
    <PageWrapper
      title="Moments"
      description="Events detected across all of your routes -- hard brakes, disengagements, forward-collision warnings, and alerts. Click a route to see its events."
    >
      {/* Device picker -- only shown when multi-device */}
      {devicesLoaded && showDevicePicker && (
        <Card className="mb-4">
          <CardBody className="flex flex-wrap items-center gap-2">
            <label
              htmlFor="device-select"
              className="text-sm text-[var(--text-secondary)]"
            >
              Device
            </label>
            <select
              id="device-select"
              value={selectedDongle}
              onChange={(e) => setSelectedDongle(e.target.value)}
              className="rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-2 py-1.5 text-sm text-[var(--text-primary)] focus:border-[var(--accent)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]"
            >
              <option value="" disabled>
                Select a device
              </option>
              {devices.map((d) => (
                <option key={d.dongleId} value={d.dongleId}>
                  {d.dongleId}
                </option>
              ))}
            </select>
          </CardBody>
        </Card>
      )}

      {deviceError && (
        <ErrorMessage title="Failed to load devices" message={deviceError} />
      )}

      {/* Filter bar */}
      <Card className="mb-4">
        <CardHeader>
          <div className="flex items-center justify-between gap-2">
            <h2 className="text-sm font-medium text-[var(--text-primary)]">
              Filters
            </h2>
            <Button variant="ghost" size="sm" onClick={clearFilters}>
              Clear
            </Button>
          </div>
        </CardHeader>
        <CardBody>
          <div className="flex flex-col gap-3">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-xs text-[var(--text-secondary)]">Type</span>
              {KNOWN_EVENT_TYPES.map((t) => {
                const isSelected = selectedTypes.includes(t);
                return (
                  <button
                    key={t}
                    type="button"
                    onClick={() => toggleType(t)}
                    aria-pressed={isSelected}
                    className={[
                      "rounded-full px-3 py-1 text-xs font-medium transition-colors",
                      isSelected
                        ? "bg-[var(--accent)] text-[var(--text-inverse)]"
                        : "bg-[var(--bg-tertiary)] text-[var(--text-secondary)] hover:bg-[var(--border-secondary)]",
                    ].join(" ")}
                  >
                    {t}
                  </button>
                );
              })}
            </div>

            <div className="flex flex-wrap items-center gap-4">
              <div className="flex items-center gap-2">
                <label
                  htmlFor="severity-select"
                  className="text-xs text-[var(--text-secondary)]"
                >
                  Severity
                </label>
                <select
                  id="severity-select"
                  value={selectedSeverity}
                  onChange={(e) =>
                    setSelectedSeverity(e.target.value as "" | Severity)
                  }
                  className="rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-2 py-1 text-xs text-[var(--text-primary)] focus:border-[var(--accent)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]"
                >
                  <option value="">Any</option>
                  {KNOWN_SEVERITIES.map((s) => (
                    <option key={s} value={s}>
                      {s}
                    </option>
                  ))}
                </select>
              </div>
              <div className="flex items-center gap-2">
                <label
                  htmlFor="limit-select"
                  className="text-xs text-[var(--text-secondary)]"
                >
                  Routes per page
                </label>
                <select
                  id="limit-select"
                  value={limit}
                  onChange={(e) => setLimit(parseInt(e.target.value, 10))}
                  className="rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-2 py-1 text-xs text-[var(--text-primary)] focus:border-[var(--accent)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]"
                >
                  {LIMIT_OPTIONS.map((n) => (
                    <option key={n} value={n}>
                      {n}
                    </option>
                  ))}
                </select>
              </div>
            </div>
          </div>
        </CardBody>
      </Card>

      {/* Results */}
      {loading && (
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading routes" />
        </div>
      )}

      {error && (
        <ErrorMessage
          title="Failed to load events"
          message={error}
          retry={fetchRoutes}
        />
      )}

      {!loading &&
        !error &&
        devicesLoaded &&
        !selectedDongle &&
        devices.length === 0 && (
          <Card>
            <CardBody>
              <p className="py-8 text-center text-caption">
                No devices registered. Register a device to start collecting
                events.
              </p>
            </CardBody>
          </Card>
        )}

      {!loading && !error && selectedDongle && routes.length === 0 && (
        <Card>
          <CardBody>
            <p className="py-8 text-center text-caption">
              No routes have events matching these filters.
            </p>
          </CardBody>
        </Card>
      )}

      {!loading && !error && routes.length > 0 && (
        <Card>
          <CardHeader>
            <div className="flex items-center justify-between gap-2">
              <span className="text-sm text-[var(--text-secondary)]">
                {pageStart}&ndash;{pageEnd} of {total} routes
              </span>
              <div className="flex items-center gap-2">
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={goPrev}
                  disabled={!hasPrev}
                >
                  Prev
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={goNext}
                  disabled={!hasNext}
                >
                  Next
                </Button>
              </div>
            </div>
          </CardHeader>
          <ul className="divide-y divide-[var(--border-primary)]">
            {routes.map((r) => {
              const isOpen = expanded.has(r.route_name);
              const events = routeEvents[r.route_name];
              return (
                <li key={r.route_name}>
                  <RouteRow
                    route={r}
                    open={isOpen}
                    onToggle={() => toggleRoute(r.route_name)}
                  />
                  {isOpen && (
                    <RouteEvents
                      dongleId={selectedDongle}
                      routeName={r.route_name}
                      state={events}
                      onRetry={() => fetchRouteEvents(r.route_name)}
                    />
                  )}
                </li>
              );
            })}
          </ul>
        </Card>
      )}
    </PageWrapper>
  );
}

interface RouteRowProps {
  route: MomentRoute;
  open: boolean;
  onToggle: () => void;
}

function RouteRow({ route, open, onToggle }: RouteRowProps) {
  // Order the type chips by descending count so the most common event
  // type for the route is the leftmost chip.
  const typeEntries = Object.entries(route.type_counts).sort(
    ([, a], [, b]) => b - a,
  );
  return (
    <button
      type="button"
      onClick={onToggle}
      aria-expanded={open}
      className="flex w-full items-center gap-3 px-4 py-3 text-left transition-colors hover:bg-[var(--bg-tertiary)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]"
    >
      <span
        aria-hidden="true"
        className={[
          "inline-block w-3 text-xs text-[var(--text-secondary)] transition-transform",
          open ? "rotate-90" : "",
        ].join(" ")}
      >
        &#9656;
      </span>
      <div className="flex min-w-0 flex-1 flex-col">
        <div className="flex items-center gap-2">
          <span
            className="truncate font-mono text-sm text-[var(--text-primary)]"
            title={route.route_name}
          >
            {route.route_name}
          </span>
          <span className="rounded-full bg-[var(--bg-tertiary)] px-2 py-0.5 text-[10px] font-medium tabular-nums text-[var(--text-secondary)]">
            {route.event_count} {route.event_count === 1 ? "event" : "events"}
          </span>
        </div>
        <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-[var(--text-secondary)]">
          <span>{formatRouteTimeRange(route.start_time, route.end_time)}</span>
          {typeEntries.length > 0 && (
            <span className="flex flex-wrap items-center gap-1">
              {typeEntries.map(([type, count]) => (
                <span
                  key={type}
                  className="inline-flex items-center gap-1"
                  title={`${count} ${type} event${count === 1 ? "" : "s"}`}
                >
                  <Badge variant={typeBadgeVariant(type)}>{type}</Badge>
                  <span className="tabular-nums text-[10px] text-[var(--text-secondary)]">
                    &times;{count}
                  </span>
                </span>
              ))}
            </span>
          )}
        </div>
      </div>
    </button>
  );
}

interface RouteEventsProps {
  dongleId: string;
  routeName: string;
  state: RouteEventsState | undefined;
  onRetry: () => void;
}

function RouteEvents({ dongleId, routeName, state, onRetry }: RouteEventsProps) {
  if (!state || state.loading) {
    return (
      <div className="flex items-center justify-center px-4 py-6">
        <Spinner size="sm" label="Loading events" />
      </div>
    );
  }
  if (state.error) {
    return (
      <div className="px-4 py-3">
        <ErrorMessage
          title="Failed to load events"
          message={state.error}
          retry={onRetry}
        />
      </div>
    );
  }
  if (state.events.length === 0) {
    return (
      <p className="px-4 py-3 text-xs text-[var(--text-secondary)]">
        No events on this route match the active filters.
      </p>
    );
  }
  const truncated = state.total > state.events.length;
  return (
    <div className="bg-[var(--bg-primary)]">
      <div className="grid grid-cols-[5rem_8rem_4rem_1fr_8rem] items-center gap-3 border-b border-[var(--border-primary)] px-4 py-2 text-[10px] font-medium uppercase tracking-wide text-[var(--text-secondary)]">
        <span>Severity</span>
        <span>Type</span>
        <span>Offset</span>
        <span>Detail</span>
        <span>When</span>
      </div>
      <ul>
        {state.events.map((e) => (
          <li key={e.id}>
            <Link
              href={`/routes/${encodeURIComponent(dongleId)}/${encodeURIComponent(routeName)}?t=${encodeURIComponent(e.route_offset_seconds.toFixed(3))}`}
              className="grid grid-cols-[5rem_8rem_4rem_1fr_8rem] items-center gap-3 border-b border-[var(--border-primary)] px-4 py-2 text-xs transition-colors hover:bg-[var(--bg-tertiary)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]"
            >
              <span>
                <Badge variant={severityBadgeVariant(e.severity)}>
                  {e.severity}
                </Badge>
              </span>
              <span>
                <Badge variant={typeBadgeVariant(e.type)}>{e.type}</Badge>
              </span>
              <span className="tabular-nums text-xs text-[var(--text-secondary)]">
                {formatOffset(e.route_offset_seconds)}
              </span>
              <span
                className="truncate font-mono text-[10px] text-[var(--text-tertiary)]"
                title={JSON.stringify(e.payload)}
              >
                {summarizePayload(e.payload)}
              </span>
              <span className="truncate text-xs text-[var(--text-primary)]">
                {formatDateTime(e.occurred_at)}
              </span>
            </Link>
          </li>
        ))}
      </ul>
      {truncated && (
        <p className="px-4 py-2 text-[10px] text-[var(--text-secondary)]">
          Showing the first {state.events.length} of {state.total} events on
          this route. Open the route page for the full list.
        </p>
      )}
    </div>
  );
}

export default function MomentsPage() {
  // useSearchParams requires a Suspense boundary under Next 15 when the
  // page is statically optimized; wrap so the build doesn't trip.
  return (
    <Suspense
      fallback={
        <PageWrapper title="Moments">
          <div className="flex items-center justify-center py-16">
            <Spinner size="lg" label="Loading" />
          </div>
        </PageWrapper>
      }
    >
      <MomentsPageInner />
    </Suspense>
  );
}
