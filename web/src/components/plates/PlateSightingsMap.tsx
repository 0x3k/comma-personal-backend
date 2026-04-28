"use client";

import dynamic from "next/dynamic";
import type { SeverityBucket } from "@/components/alerts/severity";

/**
 * Single point on the plate sightings map: one encounter, plotted at
 * the trip's reverse-geocoded start lat/lng. The severity bucket
 * paints the marker colour to match the rest of the alert UI -- gray
 * for unflagged, amber for sev 2-3, red for sev 4-5.
 */
export interface PlateSightingPoint {
  /** Stable key, typically "<dongle_id>|<route>". */
  key: string;
  lat: number;
  lng: number;
  severity: SeverityBucket;
  /**
   * Hover label rendered as the marker's title. Currently set to the
   * route name + first-seen timestamp; the format is opaque to the
   * map and decided by the caller.
   */
  label: string;
}

export interface PlateSightingsMapProps {
  points: PlateSightingPoint[];
  /** Optional CSS class for the outer container. */
  className?: string;
}

const PlateSightingsMapInner = dynamic(
  () => import("./PlateSightingsMapInner"),
  {
    ssr: false,
    loading: () => (
      <div className="flex items-center justify-center h-full w-full bg-[var(--bg-tertiary)] rounded-lg">
        <span className="text-caption">Loading map...</span>
      </div>
    ),
  },
);

/**
 * PlateSightingsMap renders a Leaflet map with one marker per
 * encounter. We keep the public surface deliberately small (just
 * points + className) so the page does not have to know anything
 * about Leaflet's lifecycle.
 *
 * The component wraps a dynamically-imported inner component so
 * Leaflet (which touches `window` at import time) does not poison
 * SSR; the route page is a client component already, but the dynamic
 * boundary mirrors TripMap and keeps the import graph predictable.
 *
 * Empty state: when zero plottable points exist (e.g. all encounters
 * lack GPS), the wrapper renders a placeholder instead of a blank
 * map. This matches TripMap's behaviour.
 */
export function PlateSightingsMap({
  points,
  className = "",
}: PlateSightingsMapProps) {
  if (points.length === 0) {
    return (
      <div
        className={`flex items-center justify-center bg-[var(--bg-tertiary)] rounded-lg ${className}`}
        style={{ minHeight: "240px" }}
        data-testid="plate-map-empty"
      >
        <p className="text-caption">
          No GPS data available for this plate&apos;s sightings.
        </p>
      </div>
    );
  }

  return (
    <div
      className={`rounded-lg overflow-hidden ${className}`}
      style={{ minHeight: "300px" }}
      data-testid="plate-map"
    >
      <PlateSightingsMapInner points={points} />
    </div>
  );
}
