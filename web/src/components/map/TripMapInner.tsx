"use client";

import { useEffect, useRef } from "react";
import { MapContainer, TileLayer, Polyline, useMap } from "react-leaflet";
import type { LatLngTuple, Map as LeafletMap } from "leaflet";
import "leaflet/dist/leaflet.css";

interface FitBoundsProps {
  coordinates: LatLngTuple[];
}

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

export interface TripMapInnerProps {
  coordinates: LatLngTuple[];
  className?: string;
}

export default function TripMapInner({
  coordinates,
  className,
}: TripMapInnerProps) {
  const mapRef = useRef<LeafletMap | null>(null);
  const center: LatLngTuple =
    coordinates.length > 0 ? coordinates[0] : [37.7749, -122.4194];

  return (
    <MapContainer
      ref={mapRef}
      center={center}
      zoom={13}
      className={className}
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
    </MapContainer>
  );
}
