"use client";

import { useEffect, useRef } from "react";
import {
  MapContainer,
  TileLayer,
  Polyline,
  Marker,
  useMap,
  useMapEvents,
} from "react-leaflet";
import L, { type LatLngTuple, type Map as LeafletMap } from "leaflet";
import "leaflet/dist/leaflet.css";
import { nearestVertexTime } from "@/lib/routePosition";

interface FitBoundsProps {
  coordinates: LatLngTuple[];
}

// MAX_SEEK_PX bounds the "click on the polyline" affordance: clicks farther
// than this many pixels from the nearest vertex are treated as "user clicked
// empty map" and ignored. 50px is large enough to forgive imprecise clicks
// on a 4px-wide polyline while still rejecting clicks miles away.
const MAX_SEEK_PX = 50;

// Module-scope so the icon is built once per page rather than per render.
// Plain <div> with inline styles avoids a global stylesheet dependency for
// a single-use marker. ~18px circle with a white border + drop shadow so it
// reads cleanly against both light street tiles and dark park areas.
const carIcon = L.divIcon({
  className: "comma-car-marker",
  html: '<div style="width:18px;height:18px;border-radius:9999px;background:#ef4444;border:2px solid white;box-shadow:0 0 6px rgba(0,0,0,0.55);"></div>',
  iconSize: [22, 22],
  iconAnchor: [11, 11],
});

/**
 * Helper component that fits the map view to the polyline bounds
 * whenever the coordinates change.
 */
function FitBounds({ coordinates }: FitBoundsProps) {
  const map = useMap();
  const prevLength = useRef(coordinates.length);

  useEffect(() => {
    if (coordinates.length < 2) return;

    const bounds = coordinates.map(
      (c) => [c[0], c[1]] as [number, number],
    );
    map.fitBounds(bounds, { padding: [32, 32] });
    prevLength.current = coordinates.length;
  }, [map, coordinates]);

  return null;
}

interface ClickToSeekProps {
  coordinates: LatLngTuple[];
  geometryTimes: number[] | null;
  totalDurationSec: number;
  onSeek: (routeRelativeSec: number) => void;
}

/**
 * Inner component that wires the map's click handler to the seek callback.
 * Snaps the click to the nearest polyline vertex (Euclidean lat/lng distance
 * is fine at city scale and avoids a haversine call per click), then
 * rejects the seek if the click landed too far from the polyline in pixel
 * space -- so clicking on empty map doesn't randomly teleport the video.
 */
function ClickToSeek({
  coordinates,
  geometryTimes,
  totalDurationSec,
  onSeek,
}: ClickToSeekProps) {
  const map = useMapEvents({
    click(e) {
      const target: [number, number] = [e.latlng.lat, e.latlng.lng];
      const hit = nearestVertexTime(
        coordinates,
        geometryTimes,
        totalDurationSec,
        target,
      );
      if (!hit) return;
      const clickPx = map.latLngToContainerPoint(e.latlng);
      const vertexPx = map.latLngToContainerPoint(hit.latLng as LatLngTuple);
      const dx = clickPx.x - vertexPx.x;
      const dy = clickPx.y - vertexPx.y;
      if (dx * dx + dy * dy > MAX_SEEK_PX * MAX_SEEK_PX) return;
      onSeek(hit.routeRelativeSec);
    },
  });
  return null;
}

export interface TripMapInnerProps {
  coordinates: LatLngTuple[];
  className?: string;
  currentPosition: [number, number] | null;
  geometryTimes: number[] | null;
  totalDurationSec: number;
  onSeek?: (routeRelativeSec: number) => void;
}

export default function TripMapInner({
  coordinates,
  className,
  currentPosition,
  geometryTimes,
  totalDurationSec,
  onSeek,
}: TripMapInnerProps) {
  const mapRef = useRef<LeafletMap | null>(null);
  const center: LatLngTuple =
    coordinates.length > 0 ? coordinates[0] : [37.7749, -122.4194];

  // Crosshair cursor when the map is interactive (onSeek wired) so the
  // click-to-seek affordance is discoverable without trial-and-error.
  const wrapperClass = onSeek
    ? `${className ?? ""} cursor-crosshair`
    : className;

  return (
    <MapContainer
      ref={mapRef}
      center={center}
      zoom={13}
      className={wrapperClass}
      style={{ height: "100%", width: "100%" }}
      scrollWheelZoom={true}
    >
      <TileLayer
        attribution='&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'
        url="https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png"
      />
      {coordinates.length >= 2 && (
        <>
          <Polyline
            positions={coordinates}
            pathOptions={{
              color: "#4c1d95",
              weight: 4,
              opacity: 0.9,
            }}
          />
          <FitBounds coordinates={coordinates} />
        </>
      )}
      {currentPosition && (
        // interactive=false lets clicks pass through the marker to the
        // map's click handler -- otherwise clicks landing on the dot would
        // be swallowed and click-to-seek would feel broken right where
        // the user is most likely to aim.
        <Marker
          position={currentPosition}
          icon={carIcon}
          interactive={false}
        />
      )}
      {onSeek && coordinates.length >= 2 && (
        <ClickToSeek
          coordinates={coordinates}
          geometryTimes={geometryTimes}
          totalDurationSec={totalDurationSec}
          onSeek={onSeek}
        />
      )}
    </MapContainer>
  );
}
