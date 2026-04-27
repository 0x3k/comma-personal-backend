"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { useParams } from "next/navigation";
import { BASE_URL } from "@/lib/api";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import type { DeviceLiveStatus } from "@/components/DeviceStatusPanel";

/**
 * UploadItem mirrors the backend's ws.UploadItem shape. Optional current_byte
 * and total_byte fields are tolerated in case a custom device firmware
 * exposes byte-level progress; the canonical athenad upstream exposes only
 * a 0-1 `progress` float along with a `current` boolean.
 */
interface UploadItem {
  id: string;
  path: string;
  url: string;
  priority: number;
  retry_count: number;
  created_at: number;
  current: boolean;
  progress: number;
  allow_cellular: boolean;
  current_byte?: number;
  total_byte?: number;
}

/** Polling cadence for the GET endpoint. */
const POLL_INTERVAL_MS = 10_000;

/** Live-status polling cadence (matches DeviceStatusPanel). */
const LIVE_POLL_INTERVAL_MS = 5_000;

/** Format raw bytes as a short human string. */
function formatBytes(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n) || n < 0) return "-";
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let u = 0;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return `${v.toFixed(v >= 10 ? 0 : 1)} ${units[u]}`;
}

/**
 * formatProgress renders the progress cell. When the device exposes
 * current_byte and total_byte we use the ratio directly and append the
 * transferred / total byte counts; otherwise we fall back to the 0-1
 * progress float reported by athenad, labelling the item "queued" when not
 * yet current and "in-flight" when current but not reporting progress.
 */
function formatProgress(item: UploadItem): string {
  if (
    typeof item.current_byte === "number" &&
    typeof item.total_byte === "number" &&
    item.total_byte > 0
  ) {
    const pct = (item.current_byte / item.total_byte) * 100;
    return `${pct.toFixed(1)}%  (${formatBytes(item.current_byte)} / ${formatBytes(item.total_byte)})`;
  }
  if (item.current) {
    if (item.progress > 0) {
      return `${(item.progress * 100).toFixed(1)}%`;
    }
    return "in-flight";
  }
  return "queued";
}

/**
 * fetchQueue issues an authenticated GET against the upload-queue endpoint.
 * credentials: "include" is required so the signed session cookie rides
 * along with the cross-origin request during local development.
 */
async function fetchQueue(dongleId: string): Promise<UploadItem[]> {
  const resp = await fetch(
    `${BASE_URL}/v1/devices/${encodeURIComponent(dongleId)}/upload-queue`,
    { credentials: "include" },
  );
  if (!resp.ok) {
    const body = (await resp.json().catch(() => ({}))) as { error?: string };
    throw new Error(body.error ?? `HTTP ${resp.status}`);
  }
  return (await resp.json()) as UploadItem[];
}

/**
 * fetchLive pulls the same /live payload that DeviceStatusPanel polls. We use
 * it here to render an aggregate "x in queue at y MB/s" header strip without
 * adding a new endpoint or poll loop.
 */
async function fetchLive(dongleId: string): Promise<DeviceLiveStatus> {
  const resp = await fetch(
    `${BASE_URL}/v1/devices/${encodeURIComponent(dongleId)}/live`,
    { credentials: "include" },
  );
  if (!resp.ok) {
    throw new Error(`HTTP ${resp.status}`);
  }
  return (await resp.json()) as DeviceLiveStatus;
}

/**
 * cancelUploads sends a POST with the given ids to the cancel endpoint and
 * throws on a non-2xx response. The returned value is the decoded result
 * the device sent back (counts keyed by per-ID status). The UI does not
 * depend on the exact shape -- callers check only that no error was
 * thrown before refetching the queue.
 */
async function cancelUploads(dongleId: string, ids: string[]): Promise<unknown> {
  const resp = await fetch(
    `${BASE_URL}/v1/devices/${encodeURIComponent(dongleId)}/upload-queue/cancel`,
    {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ids }),
    },
  );
  if (!resp.ok) {
    const body = (await resp.json().catch(() => ({}))) as { error?: string };
    throw new Error(body.error ?? `HTTP ${resp.status}`);
  }
  return resp.json();
}

export default function UploadQueuePage() {
  const params = useParams<{ dongleId: string }>();
  const dongleId = params?.dongleId ?? "";

  const [items, setItems] = useState<UploadItem[]>([]);
  const [live, setLive] = useState<DeviceLiveStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [cancelling, setCancelling] = useState(false);
  const [lastRefresh, setLastRefresh] = useState<Date | null>(null);

  // Track whether a fetch is in-flight so the poller does not stack
  // requests when the backend is slow (e.g. the device is on a flaky
  // cellular link).
  const inFlight = useRef(false);
  const liveInFlight = useRef(false);

  const refresh = useCallback(
    async (showSpinner: boolean) => {
      if (!dongleId) return;
      if (inFlight.current) return;
      inFlight.current = true;
      if (showSpinner) setLoading(true);
      try {
        const data = await fetchQueue(dongleId);
        setItems(data);
        setLastRefresh(new Date());
        setError(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : "Failed to load queue");
      } finally {
        inFlight.current = false;
        if (showSpinner) setLoading(false);
      }
    },
    [dongleId],
  );

  // refreshLive runs on its own cadence so the header strip can show fresh
  // throughput between queue polls. Errors are swallowed: a transient /live
  // failure should not steal focus from the queue table itself.
  const refreshLive = useCallback(async () => {
    if (!dongleId) return;
    if (liveInFlight.current) return;
    liveInFlight.current = true;
    try {
      const data = await fetchLive(dongleId);
      setLive(data);
    } catch {
      // Intentionally ignored -- the queue table remains usable, and the
      // header just shows a dash on transient failures.
    } finally {
      liveInFlight.current = false;
    }
  }, [dongleId]);

  useEffect(() => {
    void refresh(true);
    const id = setInterval(() => void refresh(false), POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [refresh]);

  useEffect(() => {
    void refreshLive();
    const id = setInterval(() => void refreshLive(), LIVE_POLL_INTERVAL_MS);
    return () => clearInterval(id);
  }, [refreshLive]);

  async function handleCancel(ids: string[]) {
    if (ids.length === 0) return;
    setCancelling(true);
    try {
      await cancelUploads(dongleId, ids);
      // Optimistically remove cancelled items so the user sees an
      // immediate effect; the next poll will reconcile against reality.
      setItems((prev) => prev.filter((item) => !ids.includes(item.id)));
      void refresh(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to cancel upload");
    } finally {
      setCancelling(false);
    }
  }

  // Aggregate queue stats from /live. Falls back to a dash when the device is
  // offline and no cache is available; otherwise reports the device's own
  // view of the queue (which may differ slightly from `items` because the
  // /live and queue endpoints poll on independent cadences).
  const liveImmediate = live?.immediate_queue_count ?? null;
  const liveRaw = live?.raw_queue_count ?? null;
  const liveTotalCount = live?.upload_queue_count ?? null;
  const liveUploadingNow = live?.uploading_now ?? null;
  const liveUploadingPath = live?.uploading_path ?? null;
  const liveUploadingProgress = live?.uploading_progress ?? null;
  const liveOffline = live ? !live.online : false;

  return (
    <PageWrapper
      title="Upload Queue"
      description={`Pending uploads for ${dongleId || "device"}`}
    >
      {live && (
        <div
          className={`mb-4 flex flex-wrap items-center gap-x-6 gap-y-2 rounded-md border border-[var(--border-primary)] bg-[var(--bg-secondary)] px-4 py-2 text-sm ${liveOffline ? "opacity-70" : ""}`}
        >
          <div className="flex items-center gap-2">
            <span className="text-[var(--text-secondary)]">Device queue:</span>
            <span className="tabular-nums text-[var(--text-primary)]">
              {liveTotalCount == null ? "-" : `${liveTotalCount} file${liveTotalCount === 1 ? "" : "s"}`}
            </span>
          </div>
          <div className="flex items-center gap-2">
            <span className="text-[var(--text-secondary)]">Uploading:</span>
            <span className="tabular-nums text-[var(--text-primary)]">
              {liveUploadingNow == null ? (
                "-"
              ) : !liveUploadingNow ? (
                "idle"
              ) : (
                <span className="flex items-baseline gap-2">
                  <span
                    title={liveUploadingPath ?? undefined}
                    className="max-w-[14rem] truncate font-mono text-xs"
                  >
                    {liveUploadingPath ?? "in-flight"}
                  </span>
                  {liveUploadingProgress != null && (
                    <span className="text-xs text-[var(--text-secondary)]">
                      {(liveUploadingProgress * 100).toFixed(0)}%
                    </span>
                  )}
                </span>
              )}
            </span>
          </div>
          {liveImmediate != null && liveRaw != null && (
            <div className="text-xs text-[var(--text-secondary)]">
              {liveImmediate} priority &middot; {liveRaw} raw
            </div>
          )}
          {liveOffline && (
            <Badge variant="neutral">device offline (cached)</Badge>
          )}
        </div>
      )}

      <div className="mb-4 flex items-center justify-between gap-3">
        <div className="flex items-center gap-3">
          <Badge variant="info">{items.length} item{items.length === 1 ? "" : "s"}</Badge>
          {lastRefresh && (
            <span className="text-xs text-[var(--text-secondary)]">
              Last refreshed {lastRefresh.toLocaleTimeString()}
            </span>
          )}
        </div>
        <div className="flex gap-2">
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void refresh(true)}
            disabled={loading || cancelling}
          >
            Refresh
          </Button>
          <Button
            variant="danger"
            size="sm"
            onClick={() => void handleCancel(items.map((i) => i.id))}
            disabled={cancelling || items.length === 0}
          >
            Cancel all
          </Button>
        </div>
      </div>

      {error && (
        <div className="mb-4">
          <ErrorMessage
            title="Upload queue error"
            message={error}
            retry={() => {
              setError(null);
              void refresh(true);
            }}
          />
        </div>
      )}

      {loading && items.length === 0 && (
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading upload queue" />
        </div>
      )}

      {!loading && items.length === 0 && !error && (
        <Card>
          <CardBody>
            <p className="py-8 text-center text-caption">
              No pending uploads. Everything on the device has been uploaded.
            </p>
          </CardBody>
        </Card>
      )}

      {items.length > 0 && (
        <Card>
          <CardHeader>
            <h2 className="text-subheading">Queue</h2>
          </CardHeader>
          <CardBody className="p-0">
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-[var(--border-primary)]">
                    <th className="px-4 py-2 text-left font-medium text-[var(--text-secondary)]">
                      Path
                    </th>
                    <th className="px-4 py-2 text-left font-medium text-[var(--text-secondary)]">
                      URL
                    </th>
                    <th className="px-4 py-2 text-right font-medium text-[var(--text-secondary)]">
                      Priority
                    </th>
                    <th className="px-4 py-2 text-right font-medium text-[var(--text-secondary)]">
                      Retries
                    </th>
                    <th className="px-4 py-2 text-left font-medium text-[var(--text-secondary)]">
                      Progress
                    </th>
                    <th className="px-4 py-2 text-center font-medium text-[var(--text-secondary)]">
                      Cellular
                    </th>
                    <th className="px-4 py-2 text-right font-medium text-[var(--text-secondary)]">
                      Action
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {items.map((item) => (
                    <tr
                      key={item.id}
                      className="border-b border-[var(--border-primary)] last:border-b-0"
                    >
                      <td className="px-4 py-2 font-mono text-xs text-[var(--text-primary)]">
                        <span title={item.path} className="block max-w-xs truncate">
                          {item.path}
                        </span>
                      </td>
                      <td className="px-4 py-2 font-mono text-xs text-[var(--text-secondary)]">
                        <span title={item.url} className="block max-w-xs truncate">
                          {item.url}
                        </span>
                      </td>
                      <td className="px-4 py-2 text-right text-xs text-[var(--text-primary)]">
                        {item.priority}
                      </td>
                      <td className="px-4 py-2 text-right text-xs text-[var(--text-primary)]">
                        {item.retry_count}
                      </td>
                      <td className="px-4 py-2 text-xs text-[var(--text-primary)]">
                        {formatProgress(item)}
                      </td>
                      <td className="px-4 py-2 text-center">
                        {item.allow_cellular ? (
                          <Badge variant="warning">yes</Badge>
                        ) : (
                          <Badge variant="neutral">no</Badge>
                        )}
                      </td>
                      <td className="px-4 py-2 text-right">
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => void handleCancel([item.id])}
                          disabled={cancelling}
                        >
                          Cancel
                        </Button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </CardBody>
        </Card>
      )}
    </PageWrapper>
  );
}
