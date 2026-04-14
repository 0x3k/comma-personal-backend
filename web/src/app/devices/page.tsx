"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { apiFetch, BASE_URL } from "@/lib/api";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Badge } from "@/components/ui/Badge";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";

interface Device {
  dongleId: string;
  serial?: string;
  lastSeen?: string | null;
}

function formatLastSeen(iso: string | null | undefined): string {
  if (!iso) return "Never";
  const d = new Date(iso);
  return d.toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export default function DevicesPage() {
  const [devices, setDevices] = useState<Device[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [backendOnline, setBackendOnline] = useState<boolean | null>(null);

  const fetchDevices = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await apiFetch<Device[]>("/v1/devices");
      setDevices(data);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load devices");
    } finally {
      setLoading(false);
    }
  }, []);

  // Check backend health
  useEffect(() => {
    async function checkHealth() {
      try {
        const resp = await fetch(`${BASE_URL}/health`, { method: "GET" });
        setBackendOnline(resp.ok);
      } catch {
        setBackendOnline(false);
      }
    }
    void checkHealth();
  }, []);

  useEffect(() => {
    void fetchDevices();
  }, [fetchDevices]);

  return (
    <PageWrapper
      title="Devices"
      description="Registered comma devices"
    >
      {/* Backend status */}
      <div className="mb-6 flex items-center gap-3">
        <span className="text-sm font-medium text-[var(--text-secondary)]">
          Backend
        </span>
        {backendOnline === null && <Badge variant="neutral">Checking...</Badge>}
        {backendOnline === true && <Badge variant="success">Online</Badge>}
        {backendOnline === false && <Badge variant="error">Offline</Badge>}
      </div>

      {loading && (
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading devices" />
        </div>
      )}

      {error && (
        <ErrorMessage
          title="Failed to load devices"
          message={error}
          retry={fetchDevices}
        />
      )}

      {!loading && !error && devices.length === 0 && (
        <Card>
          <CardBody>
            <p className="py-8 text-center text-caption">
              No devices registered. Connect a comma device to get started.
            </p>
          </CardBody>
        </Card>
      )}

      {!loading && !error && devices.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {devices.map((device) => (
            <Link
              key={device.dongleId}
              href={`/routes?device=${device.dongleId}`}
              className="block rounded-lg transition-transform hover:scale-[1.01] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]"
            >
              <Card className="h-full">
                <CardHeader>
                  <div className="flex items-center justify-between gap-2">
                    <h2 className="text-sm font-medium font-mono text-[var(--text-primary)] truncate">
                      {device.dongleId}
                    </h2>
                    <Badge variant="success">Registered</Badge>
                  </div>
                </CardHeader>
                <CardBody>
                  <dl className="space-y-1 text-sm">
                    {device.serial && (
                      <div className="flex justify-between">
                        <dt className="text-[var(--text-secondary)]">Serial</dt>
                        <dd className="font-mono text-xs text-[var(--text-primary)]">
                          {device.serial}
                        </dd>
                      </div>
                    )}
                    <div className="flex justify-between">
                      <dt className="text-[var(--text-secondary)]">Last seen</dt>
                      <dd className="text-[var(--text-primary)]">
                        {formatLastSeen(device.lastSeen)}
                      </dd>
                    </div>
                  </dl>
                </CardBody>
              </Card>
            </Link>
          ))}
        </div>
      )}
    </PageWrapper>
  );
}
