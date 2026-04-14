"use client";

import { useCallback, useEffect, useState } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import { apiFetch } from "@/lib/api";
import type { RouteDetailResponse, Segment } from "@/lib/types";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import { Button } from "@/components/ui/Button";

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

export default function RouteDetailPage() {
  const params = useParams<{ dongleId: string; routeName: string }>();
  const dongleId = params.dongleId;
  const routeName = decodeURIComponent(params.routeName);

  const [route, setRoute] = useState<RouteDetailResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

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
              <h1 className="text-subheading text-[var(--text-primary)]">
                {route.routeName}
              </h1>
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

          {/* Segments list */}
          <h2 className="text-subheading mb-3">Segments</h2>

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
              {route.segments.map((segment) => (
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
                  </CardBody>
                </Card>
              ))}
            </div>
          )}
        </>
      )}
    </PageWrapper>
  );
}
