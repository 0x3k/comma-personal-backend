"use client";

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { apiFetch } from "@/lib/api";
import { Spinner } from "@/components/ui/Spinner";

/**
 * Response shape of GET /v1/routes/:dongle_id/:route_name/signals. All arrays
 * are the same length; entry i across every array describes the same instant.
 * Times are unix milliseconds at the source, but the component uses them only
 * for relative positioning (time[0] is treated as t=0s).
 */
interface SignalsResponse {
  times: number[];
  speed_mps: number[];
  steering_deg: number[];
  engaged: boolean[];
  alerts: string[];
}

/**
 * An event marker surfaced from another data source (e.g. the events
 * detector). The SignalTimeline draws a small tick at `offsetSec` -- if the
 * caller does not pass any markers the component renders none.
 */
interface EventMarker {
  offsetSec: number;
  type: string;
  severity: string;
}

interface SignalTimelineProps {
  /** Device dongle id (route identifier, used to fetch signals). */
  dongleId: string;
  /** Route name (e.g. YYYY-MM-DD--HH-MM-SS). Unencoded -- this component encodes. */
  routeName: string;
  /**
   * Current video playback time in seconds, relative to the start of the
   * selected segment (the VideoPlayer's native currentTime).
   */
  currentTime: number;
  /**
   * Offset (in seconds) of the currently-playing segment from the start of
   * the route. Added to `currentTime` to map the playhead into the
   * route-relative timeline. Defaults to 0 for single-segment players.
   */
  segmentOffsetSec?: number;
  /**
   * Called with a route-relative time (in seconds) when the user clicks on
   * the timeline. The parent is responsible for translating that back into a
   * segment + segment-relative seek on the video element.
   */
  onSeek: (routeRelativeSec: number) => void;
  /**
   * Optional event markers to draw as ticks along the timeline. No-op when
   * omitted or empty.
   */
  eventMarkers?: EventMarker[];
  /** Additional CSS classes for the wrapper. */
  className?: string;
}

/** Total drawable height of the canvas (CSS pixels). */
const TIMELINE_HEIGHT = 120;
/** Heights of the three stacked tracks (engaged band, speed, steering). */
const ENGAGED_TRACK_HEIGHT = 16;
const SPEED_TRACK_HEIGHT = 44;
const STEERING_TRACK_HEIGHT = 44;
/** Gap (in CSS pixels) between the three tracks. */
const TRACK_GAP = 8;

/** Colors for the engaged state band. */
const COLOR_ENGAGED = "#22c55e"; // success-500
const COLOR_ALERT = "#ef4444"; // danger-500
const COLOR_DISENGAGED = "#52525b"; // neutral-600

/** Line colors for the signal traces. */
const COLOR_SPEED = "#3b82f6"; // info-500
const COLOR_STEERING = "#f59e0b"; // warning-500

/**
 * SignalTimeline fetches the route's driving signals and renders a compact,
 * three-track canvas that stays in sync with the video player. Clicking
 * anywhere on the canvas reports the corresponding route-relative time back
 * to the parent via `onSeek`.
 */
export function SignalTimeline({
  dongleId,
  routeName,
  currentTime,
  segmentOffsetSec = 0,
  onSeek,
  eventMarkers,
  className = "",
}: SignalTimelineProps) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [signals, setSignals] = useState<SignalsResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [size, setSize] = useState<{ width: number; height: number }>({
    width: 0,
    height: TIMELINE_HEIGHT,
  });

  // --- Fetch signals ------------------------------------------------------
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    apiFetch<SignalsResponse>(
      `/v1/routes/${dongleId}/${encodeURIComponent(routeName)}/signals`,
    )
      .then((data) => {
        if (cancelled) return;
        setSignals(data);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : "Failed to load signals");
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [dongleId, routeName]);

  // --- Derived series -----------------------------------------------------
  /**
   * Pre-computes route-relative time offsets (seconds) and signal
   * min/max bounds used by the canvas renderer. `null` until the fetch
   * completes or when the response contains no samples.
   */
  const series = useMemo(() => {
    if (!signals || signals.times.length === 0) return null;
    const n = signals.times.length;
    const t0 = signals.times[0];
    const tEnd = signals.times[n - 1];
    const totalSec = Math.max(1, (tEnd - t0) / 1000);
    let speedMin = Infinity;
    let speedMax = -Infinity;
    let steerMin = Infinity;
    let steerMax = -Infinity;
    for (let i = 0; i < n; i++) {
      const s = signals.speed_mps[i];
      const a = signals.steering_deg[i];
      if (s < speedMin) speedMin = s;
      if (s > speedMax) speedMax = s;
      if (a < steerMin) steerMin = a;
      if (a > steerMax) steerMax = a;
    }
    if (!Number.isFinite(speedMin)) speedMin = 0;
    if (!Number.isFinite(speedMax)) speedMax = 1;
    if (!Number.isFinite(steerMin)) steerMin = -1;
    if (!Number.isFinite(steerMax)) steerMax = 1;
    // Pad speed up so its top line has breathing room.
    if (speedMax - speedMin < 1) speedMax = speedMin + 1;
    // Steering is symmetric around zero; use the larger absolute bound.
    const steerAbs = Math.max(Math.abs(steerMin), Math.abs(steerMax), 1);
    return {
      t0,
      totalSec,
      speedMin,
      speedMax,
      steerAbs,
    };
  }, [signals]);

  // --- Resize tracking ----------------------------------------------------
  // ResizeObserver keeps the canvas width in sync with its container so the
  // graph stays sharp when the page layout changes. Height is fixed.
  useLayoutEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const update = () => {
      const rect = el.getBoundingClientRect();
      setSize({
        width: Math.max(0, Math.floor(rect.width)),
        height: TIMELINE_HEIGHT,
      });
    };
    update();
    const ro = new ResizeObserver(update);
    ro.observe(el);
    window.addEventListener("resize", update);
    return () => {
      ro.disconnect();
      window.removeEventListener("resize", update);
    };
  }, []);

  // --- Drawing ------------------------------------------------------------
  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const { width, height } = size;
    if (width <= 0) return;

    const dpr = typeof window !== "undefined" ? window.devicePixelRatio || 1 : 1;
    // Size the backing store for the device pixel ratio so lines stay crisp
    // on high-density displays, then scale the draw transform back down.
    canvas.width = Math.floor(width * dpr);
    canvas.height = Math.floor(height * dpr);
    canvas.style.width = `${width}px`;
    canvas.style.height = `${height}px`;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, width, height);

    if (!signals || !series) return;

    // Track vertical layout: engaged band on top, then speed, then steering.
    const engagedY = 0;
    const speedY = engagedY + ENGAGED_TRACK_HEIGHT + TRACK_GAP;
    const steeringY = speedY + SPEED_TRACK_HEIGHT + TRACK_GAP;

    const { t0, totalSec, speedMin, speedMax, steerAbs } = series;
    const n = signals.times.length;

    const xForSec = (sec: number) => (sec / totalSec) * width;
    const xForIdx = (i: number) =>
      xForSec((signals.times[i] - t0) / 1000);

    // -- Engaged band: colored spans between adjacent sample indices.
    for (let i = 0; i < n - 1; i++) {
      const isAlert = signals.alerts[i] && signals.alerts[i].length > 0;
      const isEngaged = signals.engaged[i];
      const color = isAlert
        ? COLOR_ALERT
        : isEngaged
          ? COLOR_ENGAGED
          : COLOR_DISENGAGED;
      ctx.fillStyle = color;
      const x0 = xForIdx(i);
      const x1 = xForIdx(i + 1);
      ctx.fillRect(x0, engagedY, Math.max(1, x1 - x0), ENGAGED_TRACK_HEIGHT);
    }

    // -- Track background (speed / steering) for visual separation.
    ctx.fillStyle = "rgba(255,255,255,0.04)";
    ctx.fillRect(0, speedY, width, SPEED_TRACK_HEIGHT);
    ctx.fillRect(0, steeringY, width, STEERING_TRACK_HEIGHT);

    // -- Steering baseline (zero line) to make sign obvious.
    ctx.strokeStyle = "rgba(255,255,255,0.15)";
    ctx.lineWidth = 1;
    const steeringZeroY = steeringY + STEERING_TRACK_HEIGHT / 2;
    ctx.beginPath();
    ctx.moveTo(0, steeringZeroY);
    ctx.lineTo(width, steeringZeroY);
    ctx.stroke();

    // -- Speed line.
    ctx.strokeStyle = COLOR_SPEED;
    ctx.lineWidth = 1.5;
    ctx.lineJoin = "round";
    ctx.beginPath();
    const speedRange = Math.max(1e-6, speedMax - speedMin);
    for (let i = 0; i < n; i++) {
      const x = xForIdx(i);
      const norm = (signals.speed_mps[i] - speedMin) / speedRange;
      const y = speedY + SPEED_TRACK_HEIGHT - norm * SPEED_TRACK_HEIGHT;
      if (i === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    }
    ctx.stroke();

    // -- Steering line (centered around the zero baseline).
    ctx.strokeStyle = COLOR_STEERING;
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    for (let i = 0; i < n; i++) {
      const x = xForIdx(i);
      const norm = signals.steering_deg[i] / steerAbs; // in [-1, 1]
      const y = steeringZeroY - norm * (STEERING_TRACK_HEIGHT / 2);
      if (i === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    }
    ctx.stroke();

    // -- Event markers (small vertical ticks spanning the full canvas).
    if (eventMarkers && eventMarkers.length > 0) {
      ctx.lineWidth = 1;
      for (const m of eventMarkers) {
        if (
          !Number.isFinite(m.offsetSec) ||
          m.offsetSec < 0 ||
          m.offsetSec > totalSec
        ) {
          continue;
        }
        const x = xForSec(m.offsetSec);
        ctx.strokeStyle =
          m.severity === "critical" || m.severity === "error"
            ? COLOR_ALERT
            : m.severity === "warning"
              ? COLOR_STEERING
              : "rgba(255,255,255,0.5)";
        ctx.beginPath();
        ctx.moveTo(x, 0);
        ctx.lineTo(x, height);
        ctx.stroke();
      }
    }

    // -- Playhead. Drawn last so it sits on top of every track.
    const playheadSec = Math.max(
      0,
      Math.min(totalSec, currentTime + segmentOffsetSec),
    );
    const playheadX = xForSec(playheadSec);
    ctx.strokeStyle = "#ffffff";
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(playheadX + 0.5, 0);
    ctx.lineTo(playheadX + 0.5, height);
    ctx.stroke();
  }, [signals, series, size, currentTime, segmentOffsetSec, eventMarkers]);

  // --- Click-to-seek ------------------------------------------------------
  const handleClick = useCallback(
    (event: React.MouseEvent<HTMLCanvasElement>) => {
      if (!series) return;
      const canvas = canvasRef.current;
      if (!canvas) return;
      const rect = canvas.getBoundingClientRect();
      if (rect.width <= 0) return;
      const frac = (event.clientX - rect.left) / rect.width;
      const clamped = Math.max(0, Math.min(1, frac));
      onSeek(clamped * series.totalSec);
    },
    [onSeek, series],
  );

  // --- Render -------------------------------------------------------------
  const hasData = !!signals && signals.times.length > 0;

  return (
    <div
      ref={containerRef}
      className={["relative w-full select-none", className]
        .filter(Boolean)
        .join(" ")}
      style={{ height: TIMELINE_HEIGHT }}
    >
      <canvas
        ref={canvasRef}
        onClick={handleClick}
        className="block h-full w-full cursor-pointer rounded-md bg-[var(--bg-tertiary)]"
      />
      {loading && (
        <div className="absolute inset-0 flex items-center justify-center rounded-md bg-black/40">
          <Spinner size="sm" label="Loading signals" />
        </div>
      )}
      {!loading && error && (
        <div className="absolute inset-0 flex items-center justify-center rounded-md bg-black/40 text-sm text-[var(--text-secondary)]">
          {error}
        </div>
      )}
      {!loading && !error && !hasData && (
        <div className="absolute inset-0 flex items-center justify-center rounded-md bg-black/20 text-sm text-[var(--text-secondary)]">
          No log data
        </div>
      )}
    </div>
  );
}

export type { SignalTimelineProps, EventMarker, SignalsResponse };
