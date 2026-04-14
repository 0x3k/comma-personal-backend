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
}

/**
 * Leaflet GPS track map with OpenStreetMap tiles.
 *
 * Dynamically imported with ssr: false because Leaflet requires the
 * browser window object to function.
 *
 * When no coordinates are provided, a placeholder message is shown
 * instead of the map.
 */
export default function TripMap({ coordinates, className }: TripMapProps) {
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
      <TripMapInner coordinates={coordinates} className="h-full w-full" />
    </div>
  );
}
