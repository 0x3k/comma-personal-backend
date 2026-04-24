"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { useParams, useSearchParams } from "next/navigation";
import Link from "next/link";
import { apiFetch, BASE_URL } from "@/lib/api";
import type { LogEntry, RouteDetailResponse, Segment } from "@/lib/types";
import {
  MultiCameraPlayer,
  type CameraType,
} from "@/components/video/MultiCameraPlayer";
import { SignalTimeline } from "@/components/video/SignalTimeline";
import type { VideoPlayerHandle } from "@/components/video/VideoPlayer";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import { Button } from "@/components/ui/Button";
import TripMap from "@/components/map/TripMap";
import { LogViewer } from "@/components/logs/LogViewer";
import { ShareButton } from "@/components/ShareButton";
import { RouteThumbnail } from "@/components/routes/RouteThumbnail";

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

function formatDuration(start: string | null, end: string | null): string {
  if (!start || !end) return "--";
  const ms = new Date(end).getTime() - new Date(start).getTime();
  if (ms <= 0) return "--";
  const totalSeconds = Math.floor(ms / 1000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes === 0) return `${seconds}s`;
  return `${minutes}m ${seconds}s`;
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
  const [selectedSegment, setSelectedSegment] = useState<number | null>(null);
  const [activeTab, setActiveTab] = useState<DetailTab>("segments");
  const [logs, setLogs] = useState<LogEntry[]>([]);
  // Deep-link support for /moments: when the URL carries ?t=<seconds>, we
  // auto-select the segment that contains that offset, then use the player
  // handle to seek into it once it mounts. `pendingSeekSec` holds the
  // segment-relative seek target between segment selection and player mount.
  const [pendingSeekSec, setPendingSeekSec] = useState<number | null>(null);
  // Video player -- `playerRef` lets us imperatively seek the selected segment,
  // while `currentTime` is the native (segment-relative) playback position
  // mirrored from the player's timeupdate callback. `setCurrentTime` fires
  // many times per second during playback, so downstream code is kept light.
  const playerRef = useRef<VideoPlayerHandle | null>(null);
  const [currentTime, setCurrentTime] = useState(0);

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

  // Reset the playhead whenever the selected segment changes, so the signal
  // timeline doesn't briefly render a stale position from the previous clip.
  useEffect(() => {
    setCurrentTime(0);
  }, [selectedSegment]);

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

  // Read ?t=<seconds> once the route loads and pick the matching segment.
  // Segments are 1-minute chunks by comma convention so the floor(t/60)
  // computes the segment index. If the exact segment isn't present (e.g.
  // partial upload), fall back to the first segment with video so the
  // user at least lands somewhere watchable.
  useEffect(() => {
    if (!route) return;
    const tRaw = searchParams.get("t");
    if (tRaw === null) return;
    const t = parseFloat(tRaw);
    if (!Number.isFinite(t) || t < 0) return;
    const targetNum = Math.floor(t / 60);
    const preferred = route.segments.find(
      (s) => s.number === targetNum && getAvailableCameras(s).length > 0,
    );
    const fallback = route.segments.find(
      (s) => getAvailableCameras(s).length > 0,
    );
    const chosen = preferred ?? fallback;
    if (!chosen) return;
    setSelectedSegment(chosen.number);
    const segLocal = Math.max(0, Math.min(60, t - chosen.number * 60));
    setPendingSeekSec(segLocal);
  }, [route, searchParams]);

  // Best-effort seek: once the player handle is available, use its seek
  // method. Retries briefly so hls.js has time to attach the media element.
  useEffect(() => {
    if (pendingSeekSec === null) return;
    let cancelled = false;
    let attempts = 0;
    const tryFlush = () => {
      if (cancelled) return;
      if (playerRef.current) {
        playerRef.current.seek(pendingSeekSec);
        setPendingSeekSec(null);
        return;
      }
      if (attempts++ < 40) {
        window.setTimeout(tryFlush, 100);
      } else {
        setPendingSeekSec(null);
      }
    };
    tryFlush();
    return () => {
      cancelled = true;
    };
  }, [pendingSeekSec, selectedSegment]);

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
                <ShareButton
                  dongleId={route.dongleId}
                  routeName={route.routeName}
                />
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
                  <dd>{formatDuration(route.startTime, route.endTime)}</dd>
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

          {/* GPS track map */}
          <Card className="mb-6">
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

          {/* Video player for selected segment */}
          {selectedSegment !== null && (() => {
            const seg = route.segments.find((s) => s.number === selectedSegment);
            if (!seg) return null;
            const cameras = getAvailableCameras(seg);
            if (cameras.length === 0) return null;
            // Segments are 1-minute chunks per the comma convention, so
            // segment N starts at N*60 seconds into the route. The timeline
            // uses that offset to map the segment-relative playhead to a
            // route-relative time.
            const segmentOffsetSec = seg.number * 60;
            return (
              <Card className="mb-6">
                <CardHeader>
                  <div className="flex items-center justify-between">
                    <h2 className="text-subheading text-[var(--text-primary)]">
                      Segment {seg.number}
                    </h2>
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => setSelectedSegment(null)}
                    >
                      Close
                    </Button>
                  </div>
                </CardHeader>
                <CardBody>
                  <div className="flex flex-col gap-4">
                    <MultiCameraPlayer
                      ref={playerRef}
                      segmentBaseUrl={buildSegmentBaseUrl(
                        dongleId,
                        routeName,
                        seg.number,
                      )}
                      availableCameras={cameras}
                      onTimeUpdate={setCurrentTime}
                    />
                    <SignalTimeline
                      dongleId={dongleId}
                      routeName={routeName}
                      currentTime={currentTime}
                      segmentOffsetSec={segmentOffsetSec}
                      onSeek={(routeRelativeSec) => {
                        // Convert route-relative seek target back into the
                        // current segment's local time. If the click lands
                        // outside this segment we just clamp to its own
                        // range -- a multi-segment player would route the
                        // seek to the correct segment instead.
                        const segLocal = routeRelativeSec - segmentOffsetSec;
                        const clamped = Math.max(0, Math.min(segLocal, 60));
                        playerRef.current?.seek(clamped);
                      }}
                    />
                  </div>
                </CardBody>
              </Card>
            );
          })()}

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
                    const isSelected = selectedSegment === segment.number;
                    return (
                      <Card key={segment.number}>
                        <CardBody className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
                          <div className="flex items-center gap-3">
                            <span className="text-sm font-medium text-[var(--text-primary)] tabular-nums">
                              Segment {segment.number}
                            </span>
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
                                variant={isSelected ? "primary" : "secondary"}
                                size="sm"
                                onClick={() =>
                                  setSelectedSegment(
                                    isSelected ? null : segment.number,
                                  )
                                }
                              >
                                {isSelected ? "Playing" : "Play"}
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
