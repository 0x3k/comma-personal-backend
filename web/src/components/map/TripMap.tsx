"use client";

import dynamic from "next/dynamic";
import type { LatLngTuple } from "leaflet";

const TripMapInner = dynamic(() => import("./TripMapInner"), {
  ssr: false,
  loading: () => (
    <div className="flex items-center justify-center h-full w-full bg-[var(--bg-tertiary)] rounded-lg">
      <span className="text-caption">Loading map...</span>
    </div>
  ),
});

export interface TripMapProps {
  /** Array of [lat, lng] coordinate pairs forming the GPS track. */
  coordinates: LatLngTuple[];
  /** Optional CSS class for the outer container. */
  className?: string;
  /**
   * Live "current car position" along the polyline. Pass null to hide the
   * marker. The route-detail page derives this from the playback head via
   * positionForTime() so the dot tracks the video at ~4Hz.
   */
  currentPosition?: [number, number] | null;
  /**
   * Per-vertex route-relative milliseconds, parallel to `coordinates` (same
   * length when both are present). Forwarded to the click-to-seek path so a
   * click on the polyline can be translated back to a route-relative second.
   */
  geometryTimes?: number[] | null;
  /** Total route duration in seconds, used by the fraction fallback when
   *  `geometryTimes` is missing. */
  totalDurationSec?: number;
  /**
   * Called when the user clicks on (or close to) the polyline. Receives a
   * route-relative second so the caller can hand it to RoutePlayer.seekRoute.
   * When omitted the map is read-only and the click handler is not wired.
   */
  onSeek?: (routeRelativeSec: number) => void;
}

/**
 * Leaflet GPS track map with OpenStreetMap tiles.
 *
 * Dynamically imported with ssr: false because Leaflet requires the
 * browser window object to function.
 *
 * When no coordinates are provided, a placeholder message is shown
 * instead of the map.
 *
 * Dark-mode note: the OpenStreetMap tile server returns a fixed bright
 * basemap. We intentionally leave it untouched in dark mode -- swapping to
 * a third-party dark tile provider would introduce an extra dependency and
 * potential cost. The polyline and surrounding chrome already follow the
 * app's theme via CSS variables, and the tiles sit inside a themed card so
 * the map reads as a bright inset rather than a FOIT. Revisit if a future
 * tile provider becomes part of the stack.
 */
export default function TripMap({
  coordinates,
  className,
  currentPosition,
  geometryTimes,
  totalDurationSec,
  onSeek,
}: TripMapProps) {
  if (!coordinates || coordinates.length === 0) {
    return (
      <div
        className={`flex items-center justify-center bg-[var(--bg-tertiary)] rounded-lg ${className ?? ""}`}
        style={{ minHeight: "200px" }}
      >
        <p className="text-caption">No GPS data available for this route.</p>
      </div>
    );
  }

  return (
    <div
      className={`rounded-lg overflow-hidden ${className ?? ""}`}
      style={{ minHeight: "300px" }}
    >
      <TripMapInner
        coordinates={coordinates}
        className="h-full w-full"
        currentPosition={currentPosition ?? null}
        geometryTimes={geometryTimes ?? null}
        totalDurationSec={totalDurationSec ?? 0}
        onSeek={onSeek}
      />
    </div>
  );
}
