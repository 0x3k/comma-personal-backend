"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { apiFetch } from "@/lib/api";

/**
 * Wire shape of GET /v1/alpr/alerts/summary. Mirrors the
 * alertsSummaryResponse struct in internal/api/alpr_watchlist.go.
 */
export interface AlertSummary {
  open_count: number;
  max_open_severity: number | null;
  last_alert_at: string | null;
}

/**
 * Return shape of useAlertSummary. `summary` is null until the first
 * fetch resolves. `error` is a human-readable message when the most
 * recent fetch failed; transient errors do not clear the previous
 * `summary` so the badge stays visible across a brief outage.
 */
export interface UseAlertSummaryResult {
  summary: AlertSummary | null;
  loading: boolean;
  error: string | null;
  refresh: () => void;
}

const DEFAULT_POLL_MS = 60_000;

/**
 * Test-only override knob. The default 60s poll is too long to assert
 * polling behavior in tests; tests call this to shrink the interval.
 * Production code never calls this.
 */
let pollMsOverride: number | null = null;
export function __setAlertSummaryPollMsForTests(ms: number | null): void {
  pollMsOverride = ms;
}

/**
 * useAlertSummary fetches the cheap dashboard-badge summary and
 * polls every 60s while the tab is visible. Polling pauses when
 * document.visibilityState !== "visible" (i.e. tab is hidden) and
 * resumes (with an immediate fetch) when the tab becomes visible
 * again. The hook returns a no-op shape when `enabled` is false so
 * consumers can call it unconditionally and gate rendering instead.
 */
export function useAlertSummary(enabled: boolean): UseAlertSummaryResult {
  const [summary, setSummary] = useState<AlertSummary | null>(null);
  const [loading, setLoading] = useState<boolean>(false);
  const [error, setError] = useState<string | null>(null);

  const cancelRef = useRef(false);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const fetchOnce = useCallback(async () => {
    if (cancelRef.current) return;
    setLoading(true);
    try {
      const data = await apiFetch<AlertSummary>("/v1/alpr/alerts/summary");
      if (cancelRef.current) return;
      setSummary(data);
      setError(null);
    } catch (err) {
      if (cancelRef.current) return;
      setError(err instanceof Error ? err.message : "Failed to load alert summary");
    } finally {
      if (!cancelRef.current) setLoading(false);
    }
  }, []);

  const refresh = useCallback(() => {
    void fetchOnce();
  }, [fetchOnce]);

  useEffect(() => {
    cancelRef.current = false;
    if (!enabled) {
      // Disabled: clear any prior state so re-enabling fetches fresh.
      setSummary(null);
      setError(null);
      setLoading(false);
      return () => {
        cancelRef.current = true;
      };
    }

    const pollMs = pollMsOverride ?? DEFAULT_POLL_MS;

    function clearTimer() {
      if (timerRef.current) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
    }

    function scheduleNext() {
      clearTimer();
      // Only schedule a follow-up tick when the tab is currently
      // visible; the visibilitychange handler restarts the loop on
      // resume. Server-side render has no document, so guard.
      if (typeof document === "undefined") return;
      if (document.visibilityState !== "visible") return;
      timerRef.current = setTimeout(() => {
        if (cancelRef.current) return;
        void fetchOnce().then(scheduleNext);
      }, pollMs);
    }

    function handleVisibilityChange() {
      if (typeof document === "undefined") return;
      if (document.visibilityState === "visible") {
        // Tab came back: fetch immediately to catch up on any alerts
        // that fired while we were hidden, then resume the cadence.
        void fetchOnce().then(scheduleNext);
      } else {
        // Tab hidden: pause polling. Any pending fetch will still
        // resolve, but we don't queue another one.
        clearTimer();
      }
    }

    // Fire the first fetch right away, then start the cadence.
    void fetchOnce().then(scheduleNext);

    if (typeof document !== "undefined") {
      document.addEventListener("visibilitychange", handleVisibilityChange);
    }

    return () => {
      cancelRef.current = true;
      clearTimer();
      if (typeof document !== "undefined") {
        document.removeEventListener("visibilitychange", handleVisibilityChange);
      }
    };
  }, [enabled, fetchOnce]);

  return { summary, loading, error, refresh };
}

/**
 * Severity buckets used by the dashboard badge color logic. We do not
 * surface "info" because the badge only renders when there are open
 * alerts, and severity 0/1 are not alerted to begin with.
 */
export type AlertSeverityBucket = "amber" | "red";

/**
 * classifySeverity maps a numeric severity (1-5) to the badge's color
 * bucket. Severity 2-3 -> amber, 4-5 -> red. Anything else (null, 0,
 * 1) is treated as amber so the badge has a sensible default if the
 * backend returns a future severity outside the documented range; the
 * caller is expected to hide the badge when open_count == 0 anyway.
 */
export function classifyAlertSeverity(
  sev: number | null | undefined,
): AlertSeverityBucket {
  if (sev != null && sev >= 4) return "red";
  return "amber";
}
