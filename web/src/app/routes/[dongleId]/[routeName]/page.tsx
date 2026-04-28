"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams, useSearchParams } from "next/navigation";
import Link from "next/link";
import { apiFetch, BASE_URL } from "@/lib/api";
import type { LogEntry, RouteDetailResponse, Segment } from "@/lib/types";
import { type CameraType } from "@/components/video/MultiCameraPlayer";
import { SignalTimeline } from "@/components/video/SignalTimeline";
import { PlateTimeline } from "@/components/video/PlateTimeline";
import {
  RoutePlayer,
  type RoutePlayerHandle,
} from "@/components/video/RoutePlayer";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import { Button } from "@/components/ui/Button";
import TripMap from "@/components/map/TripMap";
import { positionForTime } from "@/lib/routePosition";
import { SEGMENT_DURATION_SEC } from "@/components/video/useRoutePlayback";
import { LogViewer } from "@/components/logs/LogViewer";
import { ShareButton } from "@/components/ShareButton";
import { RouteThumbnail } from "@/components/routes/RouteThumbnail";
import { RouteAnnotations } from "@/components/routes/RouteAnnotations";
import { FullDataRequestControl } from "@/components/routes/FullDataRequestControl";
import { ExportMP4Menu } from "@/components/routes/ExportMP4Menu";
import { formatDurationBetween } from "@/lib/format";

const FILE_TYPES: { key: keyof Omit<Segment, "number">; label: string }[] = [
  { key: "fcameraUploaded", label: "fcamera" },
  { key: "ecameraUploaded", label: "ecamera" },
  { key: "dcameraUploaded", label: "dcamera" },
  { key: "qcameraUploaded", label: "qcamera" },
  { key: "rlogUploaded", label: "rlog" },
  { key: "qlogUploaded", label: "qlog" },
];

function formatDate(iso: string | null): string {
  if (!iso) return "Unknown";
  const d = new Date(iso);
  return d.toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function uploadBadgeVariant(uploaded: boolean): BadgeVariant {
  return uploaded ? "success" : "neutral";
}

function computeUploadProgress(segment: Segment): string {
  let uploaded = 0;
  for (const ft of FILE_TYPES) {
    if (segment[ft.key]) uploaded++;
  }
  return `${uploaded}/${FILE_TYPES.length}`;
}

function getAvailableCameras(segment: Segment): CameraType[] {
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

function buildSegmentBaseUrl(
  dongleId: string,
  routeName: string,
  segmentNumber: number,
): string {
  return `${BASE_URL}/storage/${dongleId}/${encodeURIComponent(routeName)}/${segmentNumber}`;
}

type DetailTab = "segments" | "logs";

export default function RouteDetailPage() {
  const params = useParams<{ dongleId: string; routeName: string }>();
  const searchParams = useSearchParams();
  const dongleId = params.dongleId;
  const routeName = decodeURIComponent(params.routeName);

  const [route, setRoute] = useState<RouteDetailResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<DetailTab>("segments");
  const [logs, setLogs] = useState<LogEntry[]>([]);
  // Deep-link support for /moments: when the URL carries ?t=<seconds>, we
  // forward the route-relative target to the player handle once it mounts.
  const [pendingDeepLinkSec, setPendingDeepLinkSec] = useState<number | null>(
    null,
  );
  // RoutePlayer publishes route-relative time via onTimeUpdate; SignalTimeline
  // and the segment list highlight derive from this single source of truth.
  const playerRef = useRef<RoutePlayerHandle | null>(null);
  const [routeRelativeTime, setRouteRelativeTime] = useState(0);
  const [currentSegmentNum, setCurrentSegmentNum] = useState<number | null>(
    null,
  );

  const fetchRoute = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await apiFetch<RouteDetailResponse>(
        `/v1/route/${dongleId}/${encodeURIComponent(routeName)}`,
      );
      setRoute(data);
    } catch (err) {
      setError(
        err instanceof Error ? err.message : "Failed to load route details",
      );
    } finally {
      setLoading(false);
    }
  }, [dongleId, routeName]);

  useEffect(() => {
    void fetchRoute();
  }, [fetchRoute]);

  // Populate placeholder log entries when the route loads.
  // Replace with a real API call once the backend exposes parsed log data.
  useEffect(() => {
    if (!route) {
      setLogs([]);
      return;
    }
    const sample: LogEntry[] = generatePlaceholderLogs(route);
    setLogs(sample);
  }, [route]);

  // Read ?t=<seconds> once the route loads and forward the route-relative
  // target to the RoutePlayer handle. RoutePlayer owns segment switching, so
  // we just hand it the absolute time and let it resolve segment + local seek.
  useEffect(() => {
    if (!route) return;
    const tRaw = searchParams.get("t");
    if (tRaw === null) return;
    const t = parseFloat(tRaw);
    if (!Number.isFinite(t) || t < 0) return;
    setPendingDeepLinkSec(t);
  }, [route, searchParams]);

  useEffect(() => {
    if (pendingDeepLinkSec === null) return;
    let cancelled = false;
    let attempts = 0;
    const tryFlush = () => {
      if (cancelled) return;
      if (playerRef.current) {
        playerRef.current.seekRoute(pendingDeepLinkSec);
        setPendingDeepLinkSec(null);
        return;
      }
      if (attempts++ < 40) {
        window.setTimeout(tryFlush, 100);
      } else {
        setPendingDeepLinkSec(null);
      }
    };
    tryFlush();
    return () => {
      cancelled = true;
    };
  }, [pendingDeepLinkSec]);

  // Live "current car position" for the map dot, derived from the playback
  // head. Recomputes at the same ~4Hz cadence as routeRelativeTime; the
  // helper is pure so the cost is a single binary search per tick.
  const currentMapPosition = useMemo(() => {
    if (!route) return null;
    return positionForTime(
      route.geometry ?? null,
      route.geometryTimes ?? null,
      routeRelativeTime,
      route.segmentCount * SEGMENT_DURATION_SEC,
    );
  }, [route, routeRelativeTime]);

  return (
    <PageWrapper>
      <div className="mb-4">
        <Link href="/routes">
          <Button variant="ghost" size="sm">
            &larr; Back to routes
          </Button>
        </Link>
      </div>

      {loading && (
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading route details" />
        </div>
      )}

      {error && (
        <ErrorMessage
          title="Failed to load route"
          message={error}
          retry={fetchRoute}
        />
      )}

      {!loading && !error && route && (
        <>
          {/* Route info header */}
          <Card className="mb-6">
            <CardHeader>
              <div className="flex flex-wrap items-start justify-between gap-3">
                <h1 className="text-subheading text-[var(--text-primary)]">
                  {route.routeName}
                </h1>
                <div className="flex flex-wrap items-start gap-2">
                  <FullDataRequestControl
                    dongleId={route.dongleId}
                    routeName={route.routeName}
                    onComplete={fetchRoute}
                  />
                  <ExportMP4Menu
                    dongleId={route.dongleId}
                    routeName={route.routeName}
                  />
                  <ShareButton
                    dongleId={route.dongleId}
                    routeName={route.routeName}
                  />
                </div>
              </div>
            </CardHeader>
            <CardBody>
              <dl className="grid gap-x-6 gap-y-2 sm:grid-cols-2 lg:grid-cols-4 text-sm">
                <div>
                  <dt className="text-[var(--text-secondary)]">Device</dt>
                  <dd className="font-mono text-xs">{route.dongleId}</dd>
                </div>
                <div>
                  <dt className="text-[var(--text-secondary)]">Start</dt>
                  <dd>{formatDate(route.startTime)}</dd>
                </div>
                <div>
                  <dt className="text-[var(--text-secondary)]">End</dt>
                  <dd>{formatDate(route.endTime)}</dd>
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

          {/* Hero thumbnail. Sits above the map + video player as a
              visual anchor for the route. The component stays visible
              when a segment is selected (see RouteThumbnail docs) so
              the page layout does not reshuffle when the user opens
              the player. */}
          <div className="mb-6 flex justify-center">
            <RouteThumbnail
              dongleId={route.dongleId}
              routeName={route.routeName}
              variant="hero"
              alt={`Preview of route ${route.routeName}`}
            />
          </div>

          {/* Annotations: star toggle, tag chips, and collapsible note.
              Mutations ride the session-only endpoints in
              internal/api/route_annotations.go, so readers without a
              session cookie (JWT-only, share-link) render in read-only
              mode -- but the detail page itself is session-gated in
              practice so that branch is mostly defensive. */}
          <RouteAnnotations
            dongleId={route.dongleId}
            routeName={route.routeName}
            starred={route.starred}
            note={route.note}
            tags={route.tags}
          />

          {/* GPS track map */}
          <Card className="mb-6">
            <CardHeader>
              <div className="flex flex-wrap items-baseline justify-between gap-2">
                <h2 className="text-subheading text-[var(--text-primary)]">
                  GPS Track
                </h2>
                {(route.geometry?.length ?? 0) >= 2 && (
                  <span className="text-caption">
                    Click the route to seek
                  </span>
                )}
              </div>
            </CardHeader>
            <CardBody>
              <TripMap
                coordinates={route.geometry ?? []}
                className="h-[400px] w-full"
                currentPosition={currentMapPosition}
                geometryTimes={route.geometryTimes ?? null}
                totalDurationSec={route.segmentCount * SEGMENT_DURATION_SEC}
                onSeek={(sec) => {
                  playerRef.current?.seekRoute(sec);
                }}
              />
            </CardBody>
          </Card>

          {/* Continuous, multi-source video player + route-wide signal
              timeline. The player auto-advances across segments and exposes
              a seekRoute handle that the timeline + segment list use to
              jump anywhere in the route. */}
          {route.segments.length > 0 && (
            <Card className="mb-6">
              <CardHeader>
                <h2 className="text-subheading text-[var(--text-primary)]">
                  Trip playback
                </h2>
              </CardHeader>
              <CardBody>
                <div className="flex flex-col gap-4">
                  <RoutePlayer
                    ref={playerRef}
                    segments={route.segments}
                    buildSegmentBaseUrl={(segNum) =>
                      buildSegmentBaseUrl(dongleId, routeName, segNum)
                    }
                    onTimeUpdate={setRouteRelativeTime}
                    onCurrentSegmentChange={setCurrentSegmentNum}
                  />
                  <SignalTimeline
                    dongleId={dongleId}
                    routeName={routeName}
                    currentTime={routeRelativeTime}
                    segmentOffsetSec={0}
                    onSeek={(routeRelativeSec) => {
                      playerRef.current?.seekRoute(routeRelativeSec);
                    }}
                  />
                  {/* Plate sightings rail. Renders nothing unless
                      alpr_enabled is true and the route has at least
                      one encounter, so it sits silently below the
                      signal timeline on routes without ALPR data. */}
                  <PlateTimeline
                    dongleId={dongleId}
                    routeName={routeName}
                    routeStartTs={route.startTime}
                    routeDurationSec={route.segmentCount * SEGMENT_DURATION_SEC}
                    onSeek={(routeRelativeSec) => {
                      playerRef.current?.seekRoute(routeRelativeSec);
                    }}
                  />
                </div>
              </CardBody>
            </Card>
          )}

          {/* Tabs */}
          <div className="mb-4 flex gap-1 border-b border-[var(--border-primary)]">
            {(["segments", "logs"] as const).map((tab) => (
              <button
                key={tab}
                type="button"
                onClick={() => setActiveTab(tab)}
                className={[
                  "px-4 py-2 text-sm font-medium capitalize transition-colors",
                  "border-b-2 -mb-px",
                  activeTab === tab
                    ? "border-[var(--accent)] text-[var(--text-primary)]"
                    : "border-transparent text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:border-[var(--border-secondary)]",
                ].join(" ")}
              >
                {tab}
              </button>
            ))}
          </div>

          {/* Segments list */}
          {activeTab === "segments" && (
            <>
              {route.segments.length === 0 ? (
                <Card>
                  <CardBody>
                    <p className="text-center text-caption py-4">
                      No segments recorded for this route.
                    </p>
                  </CardBody>
                </Card>
              ) : (
                <div className="space-y-2">
                  {route.segments.map((segment) => {
                    const cameras = getAvailableCameras(segment);
                    const hasVideo = cameras.length > 0;
                    const isCurrent = currentSegmentNum === segment.number;
                    return (
                      <Card
                        key={segment.number}
                        className={
                          isCurrent
                            ? "ring-2 ring-[var(--accent)]"
                            : undefined
                        }
                      >
                        <CardBody className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
                          <div className="flex items-center gap-3">
                            <span className="text-sm font-medium text-[var(--text-primary)] tabular-nums">
                              Segment {segment.number}
                            </span>
                            {isCurrent && (
                              <Badge variant="success">Now playing</Badge>
                            )}
                            <Badge variant="info">
                              {computeUploadProgress(segment)} files
                            </Badge>
                          </div>
                          <div className="flex items-center gap-2">
                            <div className="flex flex-wrap gap-1.5">
                              {FILE_TYPES.map((ft) => (
                                <Badge
                                  key={ft.key}
                                  variant={uploadBadgeVariant(segment[ft.key])}
                                >
                                  {ft.label}
                                </Badge>
                              ))}
                            </div>
                            {hasVideo && (
                              <Button
                                variant={isCurrent ? "primary" : "secondary"}
                                size="sm"
                                onClick={() =>
                                  playerRef.current?.seekRoute(
                                    segment.number * 60,
                                  )
                                }
                              >
                                Jump to
                              </Button>
                            )}
                          </div>
                        </CardBody>
                      </Card>
                    );
                  })}
                </div>
              )}
            </>
          )}

          {/* Log viewer */}
          {activeTab === "logs" && <LogViewer logs={logs} />}
        </>
      )}
    </PageWrapper>
  );
}

// -- Placeholder log data ----------------------------------------------------
// Generates representative log entries so the viewer is functional before the
// backend exposes a parsed-log endpoint. Replace with a real API call later.

const SAMPLE_MESSAGES: { severity: LogEntry["severity"]; message: string }[] = [
  { severity: "info", message: "boardd: CAN bus initialized" },
  { severity: "info", message: "controlsd: lateral planner ready" },
  { severity: "info", message: "modeld: loaded navigation model" },
  { severity: "warning", message: "pandad: panda not responding, retrying..." },
  { severity: "info", message: "thermald: CPU temp 42C, GPU temp 39C" },
  { severity: "error", message: "loggerd: encoder error on ecamera stream" },
  { severity: "info", message: "uploader: segment 0 upload started" },
  { severity: "info", message: "uploader: segment 0 upload complete (4.2 MB)" },
  { severity: "warning", message: "controlsd: steering torque limited" },
  { severity: "info", message: "camerad: exposure adjusted, gain=1.2" },
  { severity: "info", message: "sensord: IMU calibration applied" },
  { severity: "error", message: "athenad: connection to server lost" },
  { severity: "info", message: "athenad: reconnected to server" },
  { severity: "info", message: "plannerd: route recalculated" },
  { severity: "warning", message: "thermald: battery temp 36C approaching limit" },
];

function generatePlaceholderLogs(route: RouteDetailResponse): LogEntry[] {
  const entries: LogEntry[] = [];
  const base = route.startTime ? new Date(route.startTime).getTime() : Date.now();
  const count = route.segmentCount * 60; // ~60 entries per segment

  for (let i = 0; i < count; i++) {
    const sample = SAMPLE_MESSAGES[i % SAMPLE_MESSAGES.length];
    const ts = new Date(base + i * 1000);
    entries.push({
      id: i,
      timestamp: ts.toISOString().slice(11, 23), // HH:MM:SS.mmm
      severity: sample.severity,
      message: sample.message,
    });
  }

  return entries;
}
