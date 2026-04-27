"use client";

import { Suspense, useCallback, useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useVirtualizer } from "@tanstack/react-virtual";
import { apiFetch } from "@/lib/api";
import type { MomentEvent, MomentsListResponse } from "@/lib/types";
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

// Paginated page sizes offered in the filter bar. Keep in sync with the
// backend's maxEventsLimit (500) in internal/api/events.go.
const LIMIT_OPTIONS = [25, 50, 100, 200] as const;
const DEFAULT_LIMIT = 50;

const ROW_HEIGHT = 44;

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

function formatOccurredAt(iso: string | null): string {
  if (!iso) return "Unknown";
  const d = new Date(iso);
  return d.toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function summarizePayload(payload: unknown): string {
  if (payload === null || payload === undefined) return "";
  if (typeof payload !== "object") return String(payload);
  const entries = Object.entries(payload as Record<string, unknown>);
  if (entries.length === 0) return "";
  // Render the first two key/value pairs on the row; users can click through
  // for the full rlog context.
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

  // Filter state. Types is a multi-select so the user can pick any subset;
  // the backend only supports a single `type=` param, so we hit it
  // multiple times when more than one is selected and merge the results.
  // Drop URL values that don't match the current detector vocabulary --
  // older shareable links sent `disengagement` / `alert` / `warning`,
  // which no event row carries and which would otherwise show up as
  // invisible filters that hide every event.
  const urlTypes = params
    .getAll("type")
    .filter((t): t is (typeof KNOWN_EVENT_TYPES)[number] =>
      (KNOWN_EVENT_TYPES as readonly string[]).includes(t),
    );
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

  const [events, setEvents] = useState<MomentEvent[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Fetch the device list so the user can pick which device to browse.
  // On first load we also auto-select the only device when there's just
  // one, or fall back to the NEXT_PUBLIC_DONGLE_ID if set.
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
    // Intentionally empty deps: we want this to run exactly once.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Sync filter state to the URL so the view is shareable. We use
  // router.replace so the browser history isn't polluted with a new entry
  // for every filter tweak.
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

  // Fetch events. When multiple type filters are active we dispatch one
  // request per type and merge client-side; the backend filter supports a
  // single exact-match type, and splitting the server logic for set
  // filters would require another schema change.
  const fetchEvents = useCallback(async () => {
    if (!selectedDongle) {
      setEvents([]);
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
        const data = await apiFetch<MomentsListResponse>(
          `/v1/devices/${encodeURIComponent(selectedDongle)}/events?${baseQuery.toString()}`,
        );
        setEvents(data.events);
        setTotal(data.total);
      } else {
        // Multiple types: fetch each, merge, dedupe, sort by occurred_at desc.
        // Each request returns up to `limit` rows per type; callers who need
        // deeper pagination across a multi-type filter should narrow the
        // type filter.
        const perType = await Promise.all(
          selectedTypes.map(async (t) => {
            const q = new URLSearchParams(baseQuery);
            q.set("type", t);
            return apiFetch<MomentsListResponse>(
              `/v1/devices/${encodeURIComponent(selectedDongle)}/events?${q.toString()}`,
            );
          }),
        );
        const merged: MomentEvent[] = [];
        const seen = new Set<number>();
        let totalCount = 0;
        for (const page of perType) {
          totalCount += page.total;
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
        setEvents(merged.slice(0, limit));
        setTotal(totalCount);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load events");
    } finally {
      setLoading(false);
    }
  }, [selectedDongle, selectedTypes, selectedSeverity, limit, offset]);

  useEffect(() => {
    void fetchEvents();
  }, [fetchEvents]);

  // Reset offset whenever the filters change so the user isn't stuck on
  // page 3 of a now-empty result set.
  useEffect(() => {
    setOffset(0);
  }, [selectedDongle, selectedTypes, selectedSeverity, limit]);

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

  // Virtualizer for the events list. Falls back to rendering an empty
  // container until the parent ref is attached.
  const parentRef = useRef<HTMLDivElement>(null);
  const virtualizer = useVirtualizer({
    count: events.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: 10,
  });

  const pageStart = total === 0 ? 0 : offset + 1;
  const pageEnd = Math.min(offset + events.length, offset + limit);
  const hasPrev = offset > 0;
  const hasNext = offset + events.length < total;

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
      description="Events detected across all of your routes -- hard brakes, disengagements, forward-collision warnings, and alerts."
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
            {/* Type multi-select (chip checkboxes) */}
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

            {/* Severity + limit on one row */}
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
                  Per page
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
          <Spinner size="lg" label="Loading events" />
        </div>
      )}

      {error && (
        <ErrorMessage
          title="Failed to load events"
          message={error}
          retry={fetchEvents}
        />
      )}

      {!loading && !error && devicesLoaded && !selectedDongle && devices.length === 0 && (
        <Card>
          <CardBody>
            <p className="py-8 text-center text-caption">
              No devices registered. Register a device to start collecting
              events.
            </p>
          </CardBody>
        </Card>
      )}

      {!loading && !error && selectedDongle && events.length === 0 && (
        <Card>
          <CardBody>
            <p className="py-8 text-center text-caption">
              No events match these filters.
            </p>
          </CardBody>
        </Card>
      )}

      {!loading && !error && events.length > 0 && (
        <Card>
          <CardHeader>
            <div className="flex items-center justify-between gap-2">
              <span className="text-sm text-[var(--text-secondary)]">
                {pageStart}&ndash;{pageEnd} of {total}
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
          <div
            ref={parentRef}
            className="overflow-auto"
            style={{ height: "clamp(360px, 60vh, 720px)" }}
          >
            {/* Column headers */}
            <div className="sticky top-0 z-10 grid grid-cols-[6rem_8rem_1fr_5rem_1fr] gap-3 border-b border-[var(--border-primary)] bg-[var(--bg-surface)] px-4 py-2 text-xs font-medium uppercase tracking-wide text-[var(--text-secondary)]">
              <span>Severity</span>
              <span>Type</span>
              <span>Route</span>
              <span>Offset</span>
              <span>When / Detail</span>
            </div>
            <div
              style={{
                height: `${virtualizer.getTotalSize()}px`,
                width: "100%",
                position: "relative",
              }}
            >
              {virtualizer.getVirtualItems().map((vr) => {
                const e = events[vr.index];
                const href = `/routes/${encodeURIComponent(selectedDongle)}/${encodeURIComponent(e.route_name)}?t=${encodeURIComponent(e.route_offset_seconds.toFixed(3))}`;
                return (
                  <Link
                    key={e.id}
                    href={href}
                    data-index={vr.index}
                    ref={virtualizer.measureElement}
                    style={{
                      position: "absolute",
                      top: 0,
                      left: 0,
                      width: "100%",
                      transform: `translateY(${vr.start}px)`,
                    }}
                    className="grid grid-cols-[6rem_8rem_1fr_5rem_1fr] items-center gap-3 border-b border-[var(--border-primary)] px-4 py-2 text-sm transition-colors hover:bg-[var(--bg-tertiary)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]"
                  >
                    <span>
                      <Badge variant={severityBadgeVariant(e.severity)}>
                        {e.severity}
                      </Badge>
                    </span>
                    <span>
                      <Badge variant={typeBadgeVariant(e.type)}>{e.type}</Badge>
                    </span>
                    <span
                      className="truncate font-mono text-xs text-[var(--text-primary)]"
                      title={e.route_name}
                    >
                      {e.route_name}
                    </span>
                    <span className="tabular-nums text-xs text-[var(--text-secondary)]">
                      {formatOffset(e.route_offset_seconds)}
                    </span>
                    <span className="flex flex-col overflow-hidden">
                      <span className="truncate text-xs text-[var(--text-primary)]">
                        {formatOccurredAt(e.occurred_at)}
                      </span>
                      <span
                        className="truncate font-mono text-[10px] text-[var(--text-tertiary)]"
                        title={JSON.stringify(e.payload)}
                      >
                        {summarizePayload(e.payload)}
                      </span>
                    </span>
                  </Link>
                );
              })}
            </div>
          </div>
        </Card>
      )}
    </PageWrapper>
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
