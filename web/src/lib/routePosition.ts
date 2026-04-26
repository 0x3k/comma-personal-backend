/**
 * Pure helpers for mapping between a playback timestamp and a position on a
 * route's GPS polyline (and back). The map UI uses these to render a live
 * "current car position" dot that tracks the video, and to translate a click
 * on the polyline back into a route-relative second so the video can seek.
 *
 * Two paths are supported because routes that predate the geometry_times
 * column return without timestamps until the worker backfills them. When
 * timestamps are present we bisect by time (accurate, and the dot lingers
 * at stops); otherwise we index by fraction of total duration (approximate,
 * jumps through long stops where dedupe collapsed many samples to one).
 */

// Vertex shape: callers pass [lat, lng] tuples or leaflet's LatLngTuple
// (which has an optional altitude slot). We only read indices 0 and 1, so a
// readonly number array of any length works.
// Structural shape (only the lat/lng slots are read). Compatible with both
// our own `[number, number]` tuples and leaflet's `LatLngTuple`, which has
// an optional third altitude slot that the helpers ignore.
export type LatLngLike = { readonly 0: number; readonly 1: number };

export function positionForTime(
  geometry: ReadonlyArray<LatLngLike> | null | undefined,
  geometryTimes: ReadonlyArray<number> | null | undefined,
  routeRelativeSec: number,
  totalDurationSec: number,
): [number, number] | null {
  if (!geometry || geometry.length === 0) return null;
  if (geometry.length === 1) return [geometry[0][0], geometry[0][1]];

  if (geometryTimes && geometryTimes.length === geometry.length) {
    const targetMs = Math.max(0, routeRelativeSec * 1000);
    if (targetMs <= geometryTimes[0]) return [geometry[0][0], geometry[0][1]];
    const lastIdx = geometry.length - 1;
    if (targetMs >= geometryTimes[lastIdx]) {
      return [geometry[lastIdx][0], geometry[lastIdx][1]];
    }
    // Largest i where times[i] <= targetMs.
    let lo = 0;
    let hi = lastIdx;
    while (lo < hi) {
      const mid = (lo + hi + 1) >>> 1;
      if (geometryTimes[mid] <= targetMs) lo = mid;
      else hi = mid - 1;
    }
    const i0 = lo;
    const i1 = Math.min(i0 + 1, lastIdx);
    const t0 = geometryTimes[i0];
    const t1 = geometryTimes[i1];
    const f = t1 === t0 ? 0 : (targetMs - t0) / (t1 - t0);
    const lat0 = geometry[i0][0];
    const lng0 = geometry[i0][1];
    const lat1 = geometry[i1][0];
    const lng1 = geometry[i1][1];
    return [lat0 + (lat1 - lat0) * f, lng0 + (lng1 - lng0) * f];
  }

  if (totalDurationSec <= 0) return [geometry[0][0], geometry[0][1]];
  const t = Math.max(0, Math.min(1, routeRelativeSec / totalDurationSec));
  const exact = t * (geometry.length - 1);
  const i0 = Math.floor(exact);
  const i1 = Math.min(i0 + 1, geometry.length - 1);
  const f = exact - i0;
  const lat0 = geometry[i0][0];
  const lng0 = geometry[i0][1];
  const lat1 = geometry[i1][0];
  const lng1 = geometry[i1][1];
  return [lat0 + (lat1 - lat0) * f, lng0 + (lng1 - lng0) * f];
}

/**
 * nearestVertexTime returns the vertex closest in lat/lng to `target`,
 * along with the route-relative second that vertex corresponds to. Used by
 * the click-to-seek path: the map handles the click, calls this, and seeks
 * the video to the returned `routeRelativeSec`.
 *
 * Squared Euclidean distance on raw lat/lng is fine at city scale (the
 * scaling distortion vs. haversine is negligible at this zoom and clicks
 * are rare, so spending haversine cycles is unnecessary). The caller is
 * expected to additionally check pixel distance to reject clicks that
 * happen to be far from the polyline.
 */
export function nearestVertexTime(
  geometry: ReadonlyArray<LatLngLike> | null | undefined,
  geometryTimes: ReadonlyArray<number> | null | undefined,
  totalDurationSec: number,
  target: LatLngLike,
): { index: number; latLng: [number, number]; routeRelativeSec: number } | null {
  if (!geometry || geometry.length === 0) return null;
  let bestIdx = 0;
  let bestD2 = Infinity;
  const tLat = target[0];
  const tLng = target[1];
  for (let i = 0; i < geometry.length; i++) {
    const dLat = geometry[i][0] - tLat;
    const dLng = geometry[i][1] - tLng;
    const d2 = dLat * dLat + dLng * dLng;
    if (d2 < bestD2) {
      bestD2 = d2;
      bestIdx = i;
    }
  }
  const lastIdx = geometry.length - 1;
  let sec: number;
  if (geometryTimes && geometryTimes.length === geometry.length) {
    sec = Math.max(0, geometryTimes[bestIdx] / 1000);
  } else if (lastIdx === 0) {
    sec = 0;
  } else {
    sec = (bestIdx / lastIdx) * Math.max(0, totalDurationSec);
  }
  return {
    index: bestIdx,
    latLng: [geometry[bestIdx][0], geometry[bestIdx][1]],
    routeRelativeSec: sec,
  };
}
