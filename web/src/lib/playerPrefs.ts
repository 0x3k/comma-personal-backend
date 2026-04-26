/**
 * Persists the route player's layout + camera-source choices in localStorage so
 * the user gets the same multi-camera setup the next time they open any route.
 * SSR-safe: read/write are no-ops on the server.
 */

import type { CameraType } from "@/components/video/MultiCameraPlayer";

export type PlayerLayout =
  | "single"
  | "pip"
  | "split"
  | "grid3"
  | "main2pip";

export interface PlayerPrefs {
  layout: PlayerLayout;
  sources: CameraType[];
}

const STORAGE_KEY = "comma.routePlayerPrefs.v1";

const DEFAULT_PREFS: PlayerPrefs = {
  layout: "single",
  sources: ["fcamera"],
};

const VALID_LAYOUTS: ReadonlySet<PlayerLayout> = new Set([
  "single",
  "pip",
  "split",
  "grid3",
  "main2pip",
]);

const VALID_CAMERAS: ReadonlySet<CameraType> = new Set([
  "fcamera",
  "ecamera",
  "dcamera",
  "qcamera",
]);

export function slotCount(layout: PlayerLayout): number {
  switch (layout) {
    case "single":
      return 1;
    case "pip":
    case "split":
      return 2;
    case "grid3":
    case "main2pip":
      return 3;
  }
}

export function loadPlayerPrefs(): PlayerPrefs {
  if (typeof window === "undefined") return { ...DEFAULT_PREFS };
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return { ...DEFAULT_PREFS };
    const parsed = JSON.parse(raw) as unknown;
    if (!parsed || typeof parsed !== "object") return { ...DEFAULT_PREFS };
    const obj = parsed as Record<string, unknown>;
    const layout =
      typeof obj.layout === "string" && VALID_LAYOUTS.has(obj.layout as PlayerLayout)
        ? (obj.layout as PlayerLayout)
        : DEFAULT_PREFS.layout;
    const sourcesRaw = Array.isArray(obj.sources) ? obj.sources : [];
    const sources = sourcesRaw.filter(
      (s): s is CameraType =>
        typeof s === "string" && VALID_CAMERAS.has(s as CameraType),
    );
    const expected = slotCount(layout);
    if (sources.length !== expected) {
      return { layout, sources: padSources(sources, expected) };
    }
    return { layout, sources };
  } catch {
    return { ...DEFAULT_PREFS };
  }
}

export function savePlayerPrefs(prefs: PlayerPrefs): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(prefs));
  } catch {
    // Quota or disabled storage; not fatal.
  }
}

const FILL_ORDER: CameraType[] = ["fcamera", "dcamera", "ecamera", "qcamera"];

export function padSources(existing: CameraType[], n: number): CameraType[] {
  const out = existing.slice(0, n);
  for (const cam of FILL_ORDER) {
    if (out.length >= n) break;
    if (!out.includes(cam)) out.push(cam);
  }
  while (out.length < n) out.push("fcamera");
  return out;
}
