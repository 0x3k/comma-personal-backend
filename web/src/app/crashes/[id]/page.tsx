"use client";

import { use, useEffect, useState } from "react";
import Link from "next/link";
import { apiFetch } from "@/lib/api";
import type { CrashDetail } from "@/lib/types";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";
import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";

function levelBadgeVariant(level: string): BadgeVariant {
  switch (level) {
    case "fatal":
    case "error":
      return "error";
    case "warning":
      return "warning";
    case "info":
      return "info";
    default:
      return "neutral";
  }
}

function formatTimestamp(iso: string | null): string {
  if (!iso) return "Unknown";
  const d = new Date(iso);
  return d.toLocaleString();
}

// renderJSON pretty-prints arbitrary JSON for the operator. We collapse to
// "(empty)" for empty objects/arrays so the panel stays informative
// without the noise of "{}" placeholders.
function renderJSON(value: unknown): string {
  if (value === null || value === undefined) return "(empty)";
  if (Array.isArray(value) && value.length === 0) return "(empty)";
  if (typeof value === "object" && Object.keys(value as object).length === 0) {
    return "(empty)";
  }
  return JSON.stringify(value, null, 2);
}

interface CrashDetailPageProps {
  params: Promise<{ id: string }>;
}

export default function CrashDetailPage({ params }: CrashDetailPageProps) {
  const { id } = use(params);

  const [crash, setCrash] = useState<CrashDetail | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    async function load() {
      setLoading(true);
      setError(null);
      try {
        const data = await apiFetch<CrashDetail>(`/v1/crashes/${encodeURIComponent(id)}`);
        if (!cancelled) setCrash(data);
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : "Failed to load crash");
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    }
    void load();
    return () => {
      cancelled = true;
    };
  }, [id]);

  return (
    <PageWrapper title={crash ? `Crash #${crash.id}` : "Crash"}>
      <div className="mb-4">
        <Link
          href="/crashes"
          className="text-sm text-[var(--text-secondary)] hover:text-[var(--accent)]"
        >
          &larr; Back to crashes
        </Link>
      </div>

      {loading && (
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading crash" />
        </div>
      )}

      {error && (
        <ErrorMessage title="Failed to load crash" message={error} />
      )}

      {crash && !loading && !error && (
        <div className="flex flex-col gap-4">
          <Card>
            <CardHeader>
              <div className="flex items-center gap-3">
                <Badge variant={levelBadgeVariant(crash.level)}>{crash.level}</Badge>
                <span className="font-mono text-xs text-[var(--text-secondary)]">
                  {crash.event_id}
                </span>
              </div>
            </CardHeader>
            <CardBody>
              <p className="font-medium text-[var(--text-primary)]">
                {crash.message || "(no message)"}
              </p>
              <dl className="mt-3 grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
                <dt className="text-[var(--text-secondary)]">Device</dt>
                <dd className="font-mono text-[var(--text-primary)]">
                  {crash.dongle_id ?? "—"}
                </dd>
                <dt className="text-[var(--text-secondary)]">Occurred</dt>
                <dd>{formatTimestamp(crash.occurred_at)}</dd>
                <dt className="text-[var(--text-secondary)]">Received</dt>
                <dd>{formatTimestamp(crash.received_at)}</dd>
              </dl>
            </CardBody>
          </Card>

          <Card>
            <CardHeader>
              <h2 className="text-sm font-medium">Exception</h2>
            </CardHeader>
            <CardBody>
              <pre className="overflow-x-auto text-xs">
                {renderJSON(crash.exception)}
              </pre>
            </CardBody>
          </Card>

          <Card>
            <CardHeader>
              <h2 className="text-sm font-medium">Tags</h2>
            </CardHeader>
            <CardBody>
              <pre className="overflow-x-auto text-xs">{renderJSON(crash.tags)}</pre>
            </CardBody>
          </Card>

          <Card>
            <CardHeader>
              <h2 className="text-sm font-medium">Breadcrumbs</h2>
            </CardHeader>
            <CardBody>
              <pre className="overflow-x-auto text-xs">
                {renderJSON(crash.breadcrumbs)}
              </pre>
            </CardBody>
          </Card>

          <Card>
            <CardHeader>
              <h2 className="text-sm font-medium">Raw event</h2>
            </CardHeader>
            <CardBody>
              <pre className="max-h-96 overflow-auto text-xs">
                {renderJSON(crash.raw_event)}
              </pre>
            </CardBody>
          </Card>
        </div>
      )}
    </PageWrapper>
  );
}
