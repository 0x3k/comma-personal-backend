"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { MapContainer, Marker, TileLayer, Tooltip, useMap } from "react-leaflet";
import L, { type LatLngBoundsExpression, type Map as LeafletMap } from "leaflet";
import "leaflet/dist/leaflet.css";
import { SEVERITY_COLOR } from "@/components/alerts/severity";
import type { PlateSightingPoint } from "./PlateSightingsMap";

/**
 * Module-scope HTML strings: building a divIcon per render would
 * leak DOM nodes inside Leaflet's marker cache, so we memoize one
 * HTML body per severity bucket and instantiate icons on demand.
 */
const PIN_HTML_BY_BUCKET: Record<PlateSightingPoint["severity"], string> = {
  none: pinHtml("var(--color-neutral-500)"),
  amber: pinHtml("var(--color-warning-500)"),
  red: pinHtml("var(--color-danger-500)"),
};

function pinHtml(color: string): string {
  return [
    `<div style="width:14px;height:14px;border-radius:9999px;`,
    `background:${color};`,
    `border:2px solid white;`,
    `box-shadow:0 0 4px rgba(0,0,0,0.45);"></div>`,
  ].join("");
}

function clusterHtml(count: number, color: string): string {
  return [
    `<div style="display:flex;align-items:center;justify-content:center;`,
    `width:28px;height:28px;border-radius:9999px;`,
    `background:${color};color:white;font-size:11px;font-weight:600;`,
    `border:2px solid white;box-shadow:0 0 6px rgba(0,0,0,0.45);">`,
    `${count}</div>`,
  ].join("");
}

function makePinIcon(bucket: PlateSightingPoint["severity"]): L.DivIcon {
  return L.divIcon({
    className: "plate-sighting-pin",
    html: PIN_HTML_BY_BUCKET[bucket] ?? PIN_HTML_BY_BUCKET.none,
    iconSize: [16, 16],
    iconAnchor: [8, 8],
  });
}

function makeClusterIcon(
  count: number,
  bucket: PlateSightingPoint["severity"],
): L.DivIcon {
  return L.divIcon({
    className: "plate-sighting-cluster",
    html: clusterHtml(count, SEVERITY_COLOR[bucket]),
    iconSize: [32, 32],
    iconAnchor: [16, 16],
  });
}

interface FitBoundsProps {
  positions: [number, number][];
}

/**
 * FitBounds re-fits the viewport whenever the set of plotted points
 * changes meaningfully. We compare by length + first/last keys to
 * avoid re-fitting on every render when the points array reference
 * changes but the content does not.
 */
function FitBounds({ positions }: FitBoundsProps) {
  const map = useMap();
  const lastSig = useRef<string>("");

  useEffect(() => {
    if (positions.length === 0) return;
    const sig = `${positions.length}|${positions[0]?.join(",")}|${positions[positions.length - 1]?.join(",")}`;
    if (sig === lastSig.current) return;
    lastSig.current = sig;

    if (positions.length === 1) {
      map.setView(positions[0], 13, { animate: false });
      return;
    }
    const bounds: LatLngBoundsExpression = positions;
    map.fitBounds(bounds, { padding: [40, 40] });
  }, [map, positions]);

  return null;
}

/**
 * shouldCluster decides whether the map should fold nearby points
 * into single count glyphs. Activates when the user has zoomed out
 * far enough that individual pins overlap visibly. We require at
 * least 4 points before clustering at all -- a 2-pin map looks
 * broken if collapsed.
 */
function shouldCluster(zoom: number | undefined, count: number): boolean {
  if (count < 4) return false;
  if (zoom === undefined) return false;
  return zoom < 11;
}

interface ClusterGateProps {
  points: PlateSightingPoint[];
  children: (clusterMode: boolean) => React.ReactNode;
}

/**
 * ClusterGate watches the Leaflet map's zoom level and switches the
 * inner render function between "individual pins" and "cluster
 * glyphs". The gate uses a useState mirror of the zoom flag so a
 * Leaflet event triggers a React re-render through the standard
 * dispatcher rather than directly mutating the DOM.
 */
function ClusterGate({ points, children }: ClusterGateProps) {
  const map = useMap();
  const [clusterMode, setClusterMode] = useState<boolean>(() =>
    shouldCluster(map.getZoom(), points.length),
  );

  useEffect(() => {
    const handler = () => {
      const next = shouldCluster(map.getZoom(), points.length);
      setClusterMode((prev) => (prev === next ? prev : next));
    };
    handler();
    map.on("zoomend", handler);
    return () => {
      map.off("zoomend", handler);
    };
  }, [map, points.length]);

  return <>{children(clusterMode)}</>;
}

interface PlateSightingsMapInnerProps {
  points: PlateSightingPoint[];
}

/**
 * Default export: the actual react-leaflet map. Renders one marker
 * per point at zoom >= 11; below that, points within a coarse 0.05
 * lat/lng cell collapse into a single cluster glyph counting the
 * cell's residents.
 */
export default function PlateSightingsMapInner({
  points,
}: PlateSightingsMapInnerProps) {
  const positions = useMemo<[number, number][]>(
    () => points.map((p) => [p.lat, p.lng]),
    [points],
  );
  const center: [number, number] =
    positions.length > 0 ? positions[0] : [37.7749, -122.4194];

  return (
    <MapContainer
      center={center}
      zoom={13}
      style={{ height: "100%", width: "100%", minHeight: 300 }}
      scrollWheelZoom={true}
    >
      <TileLayer
        attribution='&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'
        url="https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png"
      />
      <FitBounds positions={positions} />
      <ClusterGate points={points}>
        {(clusterMode) =>
          clusterMode ? renderClusters(points) : renderPins(points)
        }
      </ClusterGate>
    </MapContainer>
  );
}

function renderPins(points: PlateSightingPoint[]): React.ReactNode {
  return points.map((p) => (
    <Marker
      key={p.key}
      position={[p.lat, p.lng]}
      icon={makePinIcon(p.severity)}
    >
      <Tooltip>{p.label}</Tooltip>
    </Marker>
  ));
}

/**
 * renderClusters folds points into 0.05x0.05 lat/lng cells. The
 * cluster's severity is the highest severity of any point in the
 * cell ("red beats amber beats none") so the operator's eye lands on
 * dangerous neighbourhoods first.
 */
function renderClusters(points: PlateSightingPoint[]): React.ReactNode {
  const cellSize = 0.05;
  const cells = new Map<
    string,
    {
      lat: number;
      lng: number;
      count: number;
      bucket: PlateSightingPoint["severity"];
    }
  >();
  const bucketRank: Record<PlateSightingPoint["severity"], number> = {
    none: 0,
    amber: 1,
    red: 2,
  };
  for (const p of points) {
    const cellLat = Math.round(p.lat / cellSize) * cellSize;
    const cellLng = Math.round(p.lng / cellSize) * cellSize;
    const key = `${cellLat.toFixed(3)}|${cellLng.toFixed(3)}`;
    const cur = cells.get(key);
    if (!cur) {
      cells.set(key, {
        lat: cellLat,
        lng: cellLng,
        count: 1,
        bucket: p.severity,
      });
      continue;
    }
    cur.count += 1;
    if (bucketRank[p.severity] > bucketRank[cur.bucket]) {
      cur.bucket = p.severity;
    }
  }
  return Array.from(cells.entries()).map(([key, cell]) => (
    <Marker
      key={`cluster:${key}`}
      position={[cell.lat, cell.lng]}
      icon={makeClusterIcon(cell.count, cell.bucket)}
    >
      <Tooltip>
        {cell.count} sighting{cell.count === 1 ? "" : "s"} in this area
      </Tooltip>
    </Marker>
  ));
}
