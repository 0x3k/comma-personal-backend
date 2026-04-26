"use client";

import { useCallback, useEffect, useState } from "react";
import { useParams } from "next/navigation";
import { apiFetch, BASE_URL } from "@/lib/api";
import type { ShareRouteResponse, ShareSegment } from "@/lib/types";
import {
  MultiCameraPlayer,
  type CameraType,
} from "@/components/video/MultiCameraPlayer";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Spinner } from "@/components/ui/Spinner";
import TripMap from "@/components/map/TripMap";
import { formatDurationBetween } from "@/lib/format";

/**
 * Public read-only viewer for a shared route. The token in the URL is the
 * only auth; no session cookie is required, and a 401 from the backend
 * is surfaced as an inline error message rather than redirecting the
 * viewer to the login page (they may not even have an account).
 */

function formatDateTime(iso: string | null): string {
  if (!iso) return "Unknown";
  const d = new Date(iso);
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function getAvailableCameras(segment: ShareSegment): CameraType[] {
  const cameras: CameraType[] = [];
  if (segment.fcameraUploaded) cameras.push("fcamera");
  if (segment.ecameraUploaded) cameras.push("ecamera");
  if (segment.dcameraUploaded) cameras.push("dcamera");
  // qcamera is the low-res preview that openpilot/sunnypilot upload by
  // default; the player uses it as a fallback when none of the HEVC
  // streams are available.
  if (segment.qcameraUploaded) cameras.push("qcamera");
  return cameras;
}

function buildSegmentBaseUrl(mediaBaseUrl: string, segmentNumber: number): string {
  // mediaBaseUrl is a relative path; the backend origin is BASE_URL.
  // Per-camera HLS playlists live at {base}/{seg}/{camera}/index.m3u8.
  return `${BASE_URL}${mediaBaseUrl}/${segmentNumber}`;
}

export default function SharePage() {
  const params = useParams<{ token: string }>();
  const token = params.token;

  const [route, setRoute] = useState<ShareRouteResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [errorStatus, setErrorStatus] = useState<number | null>(null);
  const [selectedSegment, setSelectedSegment] = useState<number | null>(null);

  const fetchRoute = useCallback(async () => {
    setLoading(true);
    setError(null);
    setErrorStatus(null);
    try {
      const data = await apiFetch<ShareRouteResponse>(
        `/v1/share/${encodeURIComponent(token)}`,
        { skipAuthRedirect: true, credentials: "omit" },
      );
      setRoute(data);
    } catch (err) {
      const msg = err instanceof Error ? err.message : "Failed to load shared route";
      const status = (err as { status?: number }).status ?? null;
      setError(msg);
      setErrorStatus(status);
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    void fetchRoute();
  }, [fetchRoute]);

  // Auto-select the first segment that has any available camera so the
  // viewer lands on a playing video rather than a grid of buttons.
  useEffect(() => {
    if (!route || selectedSegment !== null) return;
    for (const seg of route.segments) {
      if (getAvailableCameras(seg).length > 0) {
        setSelectedSegment(seg.number);
        return;
      }
    }
  }, [route, selectedSegment]);

  return (
    <main className="mx-auto flex w-full max-w-5xl flex-1 flex-col gap-6 px-4 py-6 sm:px-6">
      {loading && (
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading shared route" />
        </div>
      )}

      {error && !loading && (
        <Card>
          <CardHeader>
            <h1 className="text-subheading text-[var(--text-primary)]">
              {shareErrorTitle(errorStatus)}
            </h1>
          </CardHeader>
          <CardBody>
            <p className="text-sm text-[var(--text-secondary)]">
              {shareErrorMessage(errorStatus, error)}
            </p>
          </CardBody>
        </Card>
      )}

      {!loading && !error && route && (
        <>
          <Card>
            <CardHeader>
              <div className="flex flex-wrap items-baseline justify-between gap-2">
                <h1 className="text-subheading text-[var(--text-primary)]">
                  Shared route
                </h1>
                <span className="text-xs text-[var(--text-secondary)]">
                  link expires {formatDateTime(route.expiresAt)}
                </span>
              </div>
            </CardHeader>
            <CardBody>
              <dl className="grid gap-x-6 gap-y-2 sm:grid-cols-2 lg:grid-cols-4 text-sm">
                <div>
                  <dt className="text-[var(--text-secondary)]">Route</dt>
                  <dd className="font-mono text-xs">{route.routeName}</dd>
                </div>
                <div>
                  <dt className="text-[var(--text-secondary)]">Start</dt>
                  <dd>{formatDateTime(route.startTime)}</dd>
                </div>
                <div>
                  <dt className="text-[var(--text-secondary)]">End</dt>
                  <dd>{formatDateTime(route.endTime)}</dd>
                </div>
                <div>
                  <dt className="text-[var(--text-secondary)]">Duration</dt>
                  <dd>{formatDurationBetween(route.startTime, route.endTime)}</dd>
                </div>
                <div>
                  <dt className="text-[var(--text-secondary)]">Segments</dt>
                  <dd>{route.segmentCount}</dd>
                </div>
              </dl>
            </CardBody>
          </Card>

          <Card>
            <CardHeader>
              <h2 className="text-subheading text-[var(--text-primary)]">
                GPS Track
              </h2>
            </CardHeader>
            <CardBody>
              <TripMap
                coordinates={route.geometry ?? []}
                className="h-[400px] w-full"
              />
            </CardBody>
          </Card>

          {selectedSegment !== null && (() => {
            const seg = route.segments.find((s) => s.number === selectedSegment);
            if (!seg) return null;
            const cameras = getAvailableCameras(seg);
            if (cameras.length === 0) return null;
            return (
              <Card>
                <CardHeader>
                  <h2 className="text-subheading text-[var(--text-primary)]">
                    Segment {seg.number}
                  </h2>
                </CardHeader>
                <CardBody>
                  <MultiCameraPlayer
                    segmentBaseUrl={buildSegmentBaseUrl(
                      route.mediaBaseUrl,
                      seg.number,
                    )}
                    availableCameras={cameras}
                  />
                </CardBody>
              </Card>
            );
          })()}

          {route.segments.length > 0 && (
            <Card>
              <CardHeader>
                <h2 className="text-subheading text-[var(--text-primary)]">
                  Segments
                </h2>
              </CardHeader>
              <CardBody>
                <div className="flex flex-wrap gap-2">
                  {route.segments.map((segment) => {
                    const cameras = getAvailableCameras(segment);
                    const hasVideo = cameras.length > 0;
                    const isSelected = selectedSegment === segment.number;
                    return (
                      <button
                        key={segment.number}
                        type="button"
                        disabled={!hasVideo}
                        onClick={() => setSelectedSegment(segment.number)}
                        className={[
                          "rounded px-3 py-1.5 text-sm font-medium transition-colors",
                          "disabled:pointer-events-none disabled:opacity-40",
                          isSelected
                            ? "bg-[var(--accent)] text-[var(--text-inverse)]"
                            : "bg-[var(--bg-tertiary)] text-[var(--text-primary)] hover:bg-[var(--border-secondary)]",
                        ].join(" ")}
                        aria-pressed={isSelected}
                      >
                        {segment.number}
                      </button>
                    );
                  })}
                </div>
              </CardBody>
            </Card>
          )}

          <p className="text-center text-xs text-[var(--text-secondary)]">
            This is a read-only share link. Media and data are scoped to
            this route only.
          </p>
        </>
      )}
    </main>
  );
}

function shareErrorTitle(status: number | null): string {
  switch (status) {
    case 410:
      return "Share link expired";
    case 404:
      return "Route not available";
    case 401:
      return "Invalid share link";
    case 501:
      return "Sharing is disabled";
    default:
      return "Could not load shared route";
  }
}

function shareErrorMessage(status: number | null, fallback: string): string {
  switch (status) {
    case 410:
      return "This share link has expired. Ask the person who sent it for a new link.";
    case 404:
      return "The route referenced by this link no longer exists.";
    case 401:
      return "This share link is invalid or has been tampered with.";
    case 501:
      return "This backend does not have sharing enabled.";
    default:
      return fallback;
  }
}
