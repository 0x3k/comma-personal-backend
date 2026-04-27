"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { BASE_URL } from "@/lib/api";
import { Badge } from "@/components/ui/Badge";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";

// Polling cadence: the backend cache TTL is 5 minutes, but the UI wants the
// freshest-available data while mounted, so every 5s is the target. The first
// fetch runs immediately.
const POLL_INTERVAL_MS = 5000;

// Network-type codes come from openpilot's cereal enum (NetworkType). We map
// the common ones to human-readable strings; unknown codes fall back to their
// numeric value so operators can still see the raw signal.
const NETWORK_TYPE_NAMES: Record<number, string> = {
  0: "None",
  1: "WiFi",
  2: "Cell 2G",
  3: "Cell 3G",
  4: "Cell 4G",
  5: "Cell 5G",
  6: "Ethernet",
};

// Thermal status codes mirror cereal's ThermalStatus enum.
const THERMAL_STATUS_NAMES: Record<number, { label: string; variant: "success" | "warning" | "error" }> = {
  0: { label: "Green", variant: "success" },
  1: { label: "Yellow", variant: "warning" },
  2: { label: "Red", variant: "error" },
  3: { label: "Danger", variant: "error" },
};

// NetworkStrength enum, mirroring cereal's enum order. Index aligns with the
// number of filled signal bars (0 = unknown -> all dim, 4 = great -> all on).
const NETWORK_STRENGTH_NAMES: Record<number, string> = {
  0: "unknown",
  1: "poor",
  2: "moderate",
  3: "good",
  4: "great",
};
const NETWORK_STRENGTH_BY_NAME: Record<string, number> = {
  unknown: 0,
  poor: 1,
  moderate: 2,
  good: 3,
  great: 4,
};

export interface DeviceLiveStatus {
  online: boolean;
  network_type: unknown;
  metered: boolean | null;
  sim: unknown;
  free_space_gb: number | null;
  thermal_status: number | null;
  cpu_usage_percent: number | null;
  memory_usage_percent: number | null;
  max_temp_c: number | null;
  network_strength: unknown;
  power_draw_w: number | null;
  upload_speed_mbps: number | null;
  immediate_queue_count: number | null;
  immediate_queue_size_bytes: number | null;
  raw_queue_count: number | null;
  raw_queue_size_bytes: number | null;
  fetched_at: string;
  cached_at?: string | null;
}

interface DeviceStatusPanelProps {
  dongleId: string;
}

function formatTimestamp(iso: string | null | undefined): string {
  if (!iso) return "never";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "unknown";
  return d.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function describeNetworkType(value: unknown): string {
  if (value == null) return "-";
  if (typeof value === "number") {
    return NETWORK_TYPE_NAMES[value] ?? `#${value}`;
  }
  if (typeof value === "string") return value;
  if (typeof value === "object") {
    const obj = value as Record<string, unknown>;
    const nt = obj["network_type"] ?? obj["networkType"];
    if (typeof nt === "number") {
      return NETWORK_TYPE_NAMES[nt] ?? `#${nt}`;
    }
    if (typeof nt === "string") return nt;
  }
  return "unknown";
}

function describeSim(value: unknown): string {
  if (value == null) return "-";
  if (typeof value === "string") return value;
  if (typeof value === "object") {
    const obj = value as Record<string, unknown>;
    const state = obj["state"] ?? obj["sim_state"];
    const id = obj["sim_id"] ?? obj["simId"];
    const parts: string[] = [];
    if (typeof state === "string" && state !== "") parts.push(state);
    if (typeof id === "string" && id !== "") parts.push(id);
    if (typeof id === "number") parts.push(String(id));
    if (parts.length > 0) return parts.join(" / ");
  }
  return "unknown";
}

function describeThermal(value: number | null): { label: string; variant: "success" | "warning" | "error" | "neutral" } {
  if (value == null) return { label: "-", variant: "neutral" };
  const entry = THERMAL_STATUS_NAMES[value];
  if (!entry) return { label: `#${value}`, variant: "neutral" };
  return entry;
}

// resolveNetworkStrength normalises the cereal enum into a 0..4 integer plus a
// label. athenad may serialise the enum as either a name string or a numeric
// index, so we accept both. Returns null when we cannot interpret the value.
function resolveNetworkStrength(value: unknown): { index: number; label: string } | null {
  if (value == null) return null;
  if (typeof value === "number" && Number.isFinite(value)) {
    const i = Math.max(0, Math.min(4, Math.trunc(value)));
    return { index: i, label: NETWORK_STRENGTH_NAMES[i] ?? `#${i}` };
  }
  if (typeof value === "string") {
    const i = NETWORK_STRENGTH_BY_NAME[value.toLowerCase()];
    if (typeof i === "number") return { index: i, label: value };
  }
  return null;
}

// Format raw bytes as a short human string. Used for upload-queue sizes which
// span <1 KB through several GB.
function formatBytes(n: number | null): string {
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

// PercentBar renders a 0-100 percent value as a small horizontal bar plus the
// numeric label. value === null renders a muted dash.
function PercentBar({ value, danger }: { value: number | null; danger?: boolean }) {
  if (value == null) {
    return <span className="text-[var(--text-secondary)]">-</span>;
  }
  const clamped = Math.max(0, Math.min(100, value));
  const fillClass = danger
    ? "bg-danger-500"
    : clamped >= 90
      ? "bg-danger-500"
      : clamped >= 70
        ? "bg-warning-500"
        : "bg-success-500";
  return (
    <div className="flex items-center gap-2">
      <div
        className="h-1.5 w-16 overflow-hidden rounded-full bg-[var(--bg-tertiary)]"
        role="progressbar"
        aria-valuenow={clamped}
        aria-valuemin={0}
        aria-valuemax={100}
      >
        <div
          className={`h-full ${fillClass}`}
          style={{ width: `${clamped}%` }}
        />
      </div>
      <span className="text-xs tabular-nums text-[var(--text-primary)]">
        {Math.round(clamped)}%
      </span>
    </div>
  );
}

// SignalBars renders a 4-bar cellular-strength indicator. index in [0..4];
// 0 means unknown / no signal so all bars are dim.
function SignalBars({ index, label }: { index: number; label: string }) {
  return (
    <div className="flex items-center gap-2" title={label}>
      <div className="flex items-end gap-0.5">
        {[1, 2, 3, 4].map((bar) => (
          <span
            key={bar}
            className={`block w-1 rounded-sm ${bar <= index ? "bg-success-500" : "bg-[var(--bg-tertiary)]"}`}
            style={{ height: `${bar * 3 + 2}px` }}
          />
        ))}
      </div>
      <span className="text-xs text-[var(--text-primary)]">{label}</span>
    </div>
  );
}

export function DeviceStatusPanel({ dongleId }: DeviceStatusPanelProps) {
  const [status, setStatus] = useState<DeviceLiveStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [initialLoading, setInitialLoading] = useState(true);
  const mounted = useRef(true);

  const fetchStatus = useCallback(async () => {
    try {
      const resp = await fetch(`${BASE_URL}/v1/devices/${encodeURIComponent(dongleId)}/live`, {
        credentials: "include",
      });
      if (!resp.ok) {
        throw new Error(`status ${resp.status}`);
      }
      const data = (await resp.json()) as DeviceLiveStatus;
      if (!mounted.current) return;
      setStatus(data);
      setError(null);
    } catch (err) {
      if (!mounted.current) return;
      setError(err instanceof Error ? err.message : "Failed to load device status");
    } finally {
      if (mounted.current) setInitialLoading(false);
    }
  }, [dongleId]);

  useEffect(() => {
    mounted.current = true;
    void fetchStatus();
    const id = window.setInterval(() => {
      void fetchStatus();
    }, POLL_INTERVAL_MS);
    return () => {
      mounted.current = false;
      window.clearInterval(id);
    };
  }, [fetchStatus]);

  const online = status?.online ?? false;
  const thermal = describeThermal(status?.thermal_status ?? null);
  const network = describeNetworkType(status?.network_type);
  const sim = describeSim(status?.sim);
  const freeGB = status?.free_space_gb ?? null;
  const metered = status?.metered ?? null;
  const lastSeen = status?.cached_at ?? null;
  const cpu = status?.cpu_usage_percent ?? null;
  const mem = status?.memory_usage_percent ?? null;
  const maxTemp = status?.max_temp_c ?? null;
  const power = status?.power_draw_w ?? null;
  const strength = resolveNetworkStrength(status?.network_strength ?? null);
  const uploadSpeed = status?.upload_speed_mbps ?? null;
  const immCount = status?.immediate_queue_count ?? null;
  const rawCount = status?.raw_queue_count ?? null;
  const totalQueued =
    immCount != null || rawCount != null
      ? (immCount ?? 0) + (rawCount ?? 0)
      : null;
  const totalQueuedSize =
    status?.immediate_queue_size_bytes != null || status?.raw_queue_size_bytes != null
      ? (status?.immediate_queue_size_bytes ?? 0) + (status?.raw_queue_size_bytes ?? 0)
      : null;

  return (
    <Card className={online ? "" : "opacity-70"}>
      <CardHeader>
        <div className="flex items-center justify-between gap-2">
          <div className="flex items-center gap-2">
            <span
              aria-label={online ? "Online" : "Offline"}
              className={[
                "inline-block h-2.5 w-2.5 rounded-full",
                online ? "bg-success-500" : "bg-[var(--text-secondary)]",
              ].join(" ")}
            />
            <h3 className="text-sm font-medium text-[var(--text-primary)]">
              Live status
            </h3>
          </div>
          {online ? (
            <Badge variant="success">Online</Badge>
          ) : (
            <Badge variant="neutral">Offline</Badge>
          )}
        </div>
      </CardHeader>
      <CardBody>
        {initialLoading && !status && (
          <p className="text-caption">Loading device status...</p>
        )}

        {error && (
          <p className="mb-2 text-xs text-danger-600 dark:text-danger-500">
            {error}
          </p>
        )}

        {status && (
          <>
            <dl className="grid grid-cols-1 gap-y-2 text-sm sm:grid-cols-2 sm:gap-x-4">
              <div className="flex justify-between sm:block">
                <dt className="text-[var(--text-secondary)]">Network</dt>
                <dd className="text-[var(--text-primary)]">{network}</dd>
              </div>
              <div className="flex justify-between sm:block">
                <dt className="text-[var(--text-secondary)]">Signal</dt>
                <dd>
                  {strength == null ? (
                    <span className="text-[var(--text-secondary)]">-</span>
                  ) : (
                    <SignalBars index={strength.index} label={strength.label} />
                  )}
                </dd>
              </div>
              <div className="flex justify-between sm:block">
                <dt className="text-[var(--text-secondary)]">Metered</dt>
                <dd className="text-[var(--text-primary)]">
                  {metered == null ? "-" : metered ? "yes" : "no"}
                </dd>
              </div>
              <div className="flex justify-between sm:block">
                <dt className="text-[var(--text-secondary)]">SIM</dt>
                <dd className="text-[var(--text-primary)]">{sim}</dd>
              </div>
              <div className="flex justify-between sm:block">
                <dt className="text-[var(--text-secondary)]">Free disk</dt>
                <dd className="text-[var(--text-primary)]">
                  {freeGB == null ? "-" : `${freeGB.toFixed(1)} GB`}
                </dd>
              </div>
              <div className="flex justify-between sm:block">
                <dt className="text-[var(--text-secondary)]">Thermal</dt>
                <dd>
                  <Badge variant={thermal.variant === "neutral" ? "neutral" : thermal.variant}>
                    {thermal.label}
                  </Badge>
                  {maxTemp != null && (
                    <span className="ml-2 text-xs text-[var(--text-secondary)] tabular-nums">
                      {maxTemp.toFixed(0)}&deg;C
                    </span>
                  )}
                </dd>
              </div>
              <div className="flex justify-between sm:block">
                <dt className="text-[var(--text-secondary)]">CPU (peak)</dt>
                <dd>
                  <PercentBar value={cpu} />
                </dd>
              </div>
              <div className="flex justify-between sm:block">
                <dt className="text-[var(--text-secondary)]">Memory</dt>
                <dd>
                  <PercentBar value={mem} />
                </dd>
              </div>
              <div className="flex justify-between sm:block">
                <dt className="text-[var(--text-secondary)]">Power draw</dt>
                <dd className="text-[var(--text-primary)] tabular-nums">
                  {power == null ? "-" : `${power.toFixed(1)} W`}
                </dd>
              </div>
              <div className="flex justify-between sm:block">
                <dt className="text-[var(--text-secondary)]">Upload speed</dt>
                <dd className="text-[var(--text-primary)] tabular-nums">
                  {uploadSpeed == null
                    ? "-"
                    : uploadSpeed === 0
                      ? "idle"
                      : `${uploadSpeed.toFixed(2)} MB/s`}
                </dd>
              </div>
              <div className="flex justify-between sm:block sm:col-span-2">
                <dt className="text-[var(--text-secondary)]">Upload queue</dt>
                <dd className="text-[var(--text-primary)] tabular-nums">
                  {totalQueued == null ? (
                    "-"
                  ) : (
                    <>
                      {totalQueued} {totalQueued === 1 ? "file" : "files"}
                      {totalQueuedSize != null && totalQueuedSize > 0 ? (
                        <span className="ml-1 text-[var(--text-secondary)]">
                          ({formatBytes(totalQueuedSize)})
                        </span>
                      ) : null}
                      {immCount != null && rawCount != null ? (
                        <span className="ml-2 text-xs text-[var(--text-secondary)]">
                          {immCount} priority / {rawCount} raw
                        </span>
                      ) : null}
                    </>
                  )}
                </dd>
              </div>
              {!online && lastSeen && (
                <div className="flex justify-between sm:block sm:col-span-2">
                  <dt className="text-[var(--text-secondary)]">Last seen</dt>
                  <dd className="text-[var(--text-primary)]">{formatTimestamp(lastSeen)}</dd>
                </div>
              )}
            </dl>
          </>
        )}
      </CardBody>
    </Card>
  );
}
