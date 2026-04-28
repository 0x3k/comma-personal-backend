"use client";

import { useEffect, useState } from "react";
import { apiFetch } from "@/lib/api";

/**
 * Subset of GET /v1/settings/alpr we surface to consumers. We keep the
 * shape close to the wire format (snake_case keys translated to camel)
 * so adding a new field on the backend only needs the type widened
 * here. Fields outside the dashboard's needs (retention windows,
 * notify thresholds) are also returned so future callers do not need
 * a second hook.
 */
export interface AlprSettings {
  enabled: boolean;
  engineUrl: string;
  region: string;
  framesPerSecond: number;
  confidenceMin: number;
  retentionDaysUnflagged: number;
  retentionDaysFlagged: number;
  notifyMinSeverity: number;
  encryptionKeyConfigured: boolean;
  engineReachable: boolean;
  disclaimerRequired: boolean;
  disclaimerVersion: string;
  disclaimerAckedAt: string | null;
  disclaimerAckedJurisdiction: string | null;
}

interface AlprSettingsWire {
  enabled: boolean;
  engine_url: string;
  region: string;
  frames_per_second: number;
  confidence_min: number;
  retention_days_unflagged: number;
  retention_days_flagged: number;
  notify_min_severity: number;
  encryption_key_configured: boolean;
  engine_reachable: boolean;
  disclaimer_required: boolean;
  disclaimer_version: string;
  disclaimer_acked_at: string | null;
  disclaimer_acked_jurisdiction: string | null;
}

/**
 * Return shape of useAlprSettings. `enabled` is undefined while the
 * fetch is in flight so consumers can render nothing during loading
 * (rather than flashing a "disabled" UI for one frame). After the
 * first successful fetch it stays defined as true/false.
 *
 * `settings` carries the full effective settings response when
 * available; consumers that only care about the master flag can read
 * `enabled` directly.
 */
export interface UseAlprSettingsResult {
  enabled: boolean | undefined;
  engineReachable: boolean | undefined;
  settings: AlprSettings | null;
  loading: boolean;
  error: string | null;
}

/**
 * Module-level cache: a single in-flight promise + the last resolved
 * value, keyed implicitly by "the alpr settings endpoint" (the URL is
 * fixed). The first hook to mount triggers the fetch; later mounts
 * within the same session reuse the resolved value without hitting
 * the API again. Mutations (e.g. the settings page flipping `enabled`)
 * call invalidateAlprSettings() to clear the cache so subsequent
 * mounts re-read.
 */
let cached: AlprSettings | null = null;
let inflight: Promise<AlprSettings> | null = null;

function fromWire(w: AlprSettingsWire): AlprSettings {
  return {
    enabled: w.enabled,
    engineUrl: w.engine_url,
    region: w.region,
    framesPerSecond: w.frames_per_second,
    confidenceMin: w.confidence_min,
    retentionDaysUnflagged: w.retention_days_unflagged,
    retentionDaysFlagged: w.retention_days_flagged,
    notifyMinSeverity: w.notify_min_severity,
    encryptionKeyConfigured: w.encryption_key_configured,
    engineReachable: w.engine_reachable,
    disclaimerRequired: w.disclaimer_required,
    disclaimerVersion: w.disclaimer_version,
    disclaimerAckedAt: w.disclaimer_acked_at,
    disclaimerAckedJurisdiction: w.disclaimer_acked_jurisdiction,
  };
}

function loadAlprSettings(): Promise<AlprSettings> {
  if (cached !== null) return Promise.resolve(cached);
  if (inflight !== null) return inflight;
  inflight = apiFetch<AlprSettingsWire>("/v1/settings/alpr")
    .then((wire) => {
      const value = fromWire(wire);
      cached = value;
      return value;
    })
    .finally(() => {
      inflight = null;
    });
  return inflight;
}

/**
 * invalidateAlprSettings clears the cached settings so the next mount
 * (or the next call to useAlprSettings) re-fetches. Settings-page
 * mutators should invoke this after a successful PUT/POST so the
 * dashboard reflects the new flag immediately.
 */
export function invalidateAlprSettings(): void {
  cached = null;
  inflight = null;
}

/**
 * Test-only helper to seed the module-level cache. Lets tests render
 * components that depend on useAlprSettings without standing up a
 * fetch mock for the settings endpoint. Production code should never
 * call this.
 */
export function __setAlprSettingsCacheForTests(
  value: AlprSettings | null,
): void {
  cached = value;
  inflight = null;
}

/**
 * useAlprSettings reads the runtime ALPR settings (master flag, engine
 * reachability, etc.) from the backend. The first hook to mount in a
 * session triggers a single fetch; later mounts reuse the cached
 * value, so adding the hook to a new component is effectively free.
 *
 * During the first load `enabled` is `undefined` -- callers should
 * gate rendering on `=== true` so the UI does not flash a
 * "disabled"-looking state for one frame on first mount. The cache is
 * populated synchronously on subsequent mounts, so post-load consumers
 * see the resolved value on their first render.
 */
export function useAlprSettings(): UseAlprSettingsResult {
  const [settings, setSettings] = useState<AlprSettings | null>(cached);
  const [loading, setLoading] = useState<boolean>(cached === null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (cached !== null) {
      setSettings(cached);
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    loadAlprSettings()
      .then((value) => {
        if (cancelled) return;
        setSettings(value);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : "Failed to load alpr settings");
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return {
    enabled: settings?.enabled,
    engineReachable: settings?.engineReachable,
    settings,
    loading,
    error,
  };
}
