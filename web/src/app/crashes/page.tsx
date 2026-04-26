"use client";

import { Suspense, useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { apiFetch } from "@/lib/api";
import type { CrashListItem, CrashListResponse } from "@/lib/types";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";
import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import { Button } from "@/components/ui/Button";

const KNOWN_LEVELS = ["fatal", "error", "warning", "info", "debug"] as const;
type Level = (typeof KNOWN_LEVELS)[number];

const LIMIT_OPTIONS = [25, 50, 100, 200] as const;
const DEFAULT_LIMIT = 50;

interface Device {
  dongleId: string;
}

function levelBadgeVariant(level: string): BadgeVariant {
  switch (level) {
    case "fatal":
    case "error":
      return "error";
    case "warning":
      return "warning";
    case "info":
      return "info";
    default:
      return "neutral";
  }
}

function formatTimestamp(iso: string | null): string {
  if (!iso) return "Unknown";
  const d = new Date(iso);
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

// summarizeException pulls a one-line description from the exception
// JSONB. Sentry serializes exceptions as {values: [{type, value}, ...]};
// we use the first frame so the dashboard list stays readable.
function summarizeException(exception: unknown): string {
  if (!exception || typeof exception !== "object") return "";
  const obj = exception as Record<string, unknown>;
  const values = obj.values;
  if (!Array.isArray(values) || values.length === 0) return "";
  const first = values[0] as Record<string, unknown>;
  const type = typeof first.type === "string" ? first.type : "";
  const value = typeof first.value === "string" ? first.value : "";
  if (type && value) return `${type}: ${value}`;
  return type || value;
}

function CrashesPageInner() {
  const router = useRouter();
  const params = useSearchParams();

  // Device picker state: optional. When unset, list every crash.
  const [devices, setDevices] = useState<Device[]>([]);
  const [devicesLoaded, setDevicesLoaded] = useState(false);
  const [deviceError, setDeviceError] = useState<string | null>(null);

  const urlDongle = params.get("device") ?? "";
  const [selectedDongle, setSelectedDongle] = useState<string>(urlDongle);

  const urlLevel = (params.get("level") ?? "") as "" | Level;
  const urlLimitRaw = parseInt(params.get("limit") ?? "", 10);
  const urlLimit = LIMIT_OPTIONS.includes(urlLimitRaw as (typeof LIMIT_OPTIONS)[number])
    ? (urlLimitRaw as (typeof LIMIT_OPTIONS)[number])
    : DEFAULT_LIMIT;
  const urlOffsetRaw = parseInt(params.get("offset") ?? "", 10);
  const urlOffset = Number.isFinite(urlOffsetRaw) && urlOffsetRaw >= 0 ? urlOffsetRaw : 0;

  const [selectedLevel, setSelectedLevel] = useState<"" | Level>(urlLevel);
  const [limit, setLimit] = useState<number>(urlLimit);
  const [offset, setOffset] = useState<number>(urlOffset);

  const [crashes, setCrashes] = useState<CrashListItem[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Load devices for the optional device picker.
  useEffect(() => {
    let cancelled = false;
    async function load() {
      setDeviceError(null);
      try {
        const data = await apiFetch<Device[]>("/v1/devices");
        if (cancelled) return;
        setDevices(data);
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
    void load();
    return () => {
      cancelled = true;
    };
  }, []);

  // Sync filters to URL.
  useEffect(() => {
    const sp = new URLSearchParams();
    if (selectedDongle) sp.set("device", selectedDongle);
    if (selectedLevel) sp.set("level", selectedLevel);
    if (limit !== DEFAULT_LIMIT) sp.set("limit", String(limit));
    if (offset > 0) sp.set("offset", String(offset));
    const query = sp.toString();
    router.replace(query ? `/crashes?${query}` : "/crashes", { scroll: false });
  }, [selectedDongle, selectedLevel, limit, offset, router]);

  const fetchCrashes = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const sp = new URLSearchParams();
      sp.set("limit", String(limit));
      sp.set("offset", String(offset));
      if (selectedDongle) sp.set("device", selectedDongle);
      const data = await apiFetch<CrashListResponse>(`/v1/crashes?${sp.toString()}`);
      // Backend doesn't filter by level (the column is text and the level
      // filter is client-side only). This keeps the SQL simple and lets
      // operators flip between levels without re-paginating.
      const filtered = selectedLevel
        ? data.crashes.filter((c) => c.level === selectedLevel)
        : data.crashes;
      setCrashes(filtered);
      setTotal(data.total);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load crashes");
    } finally {
      setLoading(false);
    }
  }, [selectedDongle, selectedLevel, limit, offset]);

  useEffect(() => {
    void fetchCrashes();
  }, [fetchCrashes]);

  // Reset offset whenever filters change.
  useEffect(() => {
    setOffset(0);
  }, [selectedDongle, selectedLevel, limit]);

  const clearFilters = useCallback(() => {
    setSelectedLevel("");
    setLimit(DEFAULT_LIMIT);
    setOffset(0);
  }, []);

  const pageStart = total === 0 ? 0 : offset + 1;
  const pageEnd = Math.min(offset + crashes.length, offset + limit);
  const hasPrev = offset > 0;
  const hasNext = offset + crashes.length < total;

  const goPrev = useCallback(() => {
    setOffset((o) => Math.max(0, o - limit));
  }, [limit]);
  const goNext = useCallback(() => {
    setOffset((o) => o + limit);
  }, [limit]);

  return (
    <PageWrapper
      title="Crashes"
      description="Exceptions and tombstones forwarded by sentry_sdk on connected devices."
    >
      {devicesLoaded && devices.length > 1 && (
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
              <option value="">All devices</option>
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
          <div className="flex flex-wrap items-center gap-4">
            <div className="flex items-center gap-2">
              <label
                htmlFor="level-select"
                className="text-xs text-[var(--text-secondary)]"
              >
                Level
              </label>
              <select
                id="level-select"
                value={selectedLevel}
                onChange={(e) =>
                  setSelectedLevel(e.target.value as "" | Level)
                }
                className="rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-2 py-1 text-xs text-[var(--text-primary)] focus:border-[var(--accent)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]"
              >
                <option value="">Any</option>
                {KNOWN_LEVELS.map((l) => (
                  <option key={l} value={l}>
                    {l}
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
        </CardBody>
      </Card>

      {loading && (
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading crashes" />
        </div>
      )}

      {error && (
        <ErrorMessage
          title="Failed to load crashes"
          message={error}
          retry={fetchCrashes}
        />
      )}

      {!loading && !error && crashes.length === 0 && (
        <Card>
          <CardBody>
            <p className="py-8 text-center text-caption">
              No crashes recorded. Configure your device&apos;s Sentry DSN to
              point at this backend to start collecting exceptions.
            </p>
          </CardBody>
        </Card>
      )}

      {!loading && !error && crashes.length > 0 && (
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
          <div className="overflow-auto">
            <div className="grid grid-cols-[6rem_8rem_1fr_12rem] gap-3 border-b border-[var(--border-primary)] px-4 py-2 text-xs font-medium uppercase tracking-wide text-[var(--text-secondary)]">
              <span>Level</span>
              <span>Device</span>
              <span>Message</span>
              <span>Received</span>
            </div>
            {crashes.map((c) => (
              <Link
                key={c.id}
                href={`/crashes/${c.id}`}
                className="grid grid-cols-[6rem_8rem_1fr_12rem] items-center gap-3 border-b border-[var(--border-primary)] px-4 py-2 text-sm transition-colors hover:bg-[var(--bg-tertiary)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]"
              >
                <span>
                  <Badge variant={levelBadgeVariant(c.level)}>{c.level}</Badge>
                </span>
                <span
                  className="truncate font-mono text-xs text-[var(--text-secondary)]"
                  title={c.dongle_id ?? "(unknown)"}
                >
                  {c.dongle_id ?? "—"}
                </span>
                <span className="flex flex-col overflow-hidden">
                  <span className="truncate text-xs text-[var(--text-primary)]">
                    {c.message || summarizeException(c.exception) || "(no message)"}
                  </span>
                  {c.message && (
                    <span
                      className="truncate font-mono text-[10px] text-[var(--text-tertiary)]"
                      title={summarizeException(c.exception)}
                    >
                      {summarizeException(c.exception)}
                    </span>
                  )}
                </span>
                <span className="truncate text-xs text-[var(--text-secondary)]">
                  {formatTimestamp(c.received_at)}
                </span>
              </Link>
            ))}
          </div>
        </Card>
      )}
    </PageWrapper>
  );
}

export default function CrashesPage() {
  return (
    <Suspense
      fallback={
        <PageWrapper title="Crashes">
          <div className="flex items-center justify-center py-16">
            <Spinner size="lg" label="Loading" />
          </div>
        </PageWrapper>
      }
    >
      <CrashesPageInner />
    </Suspense>
  );
}
