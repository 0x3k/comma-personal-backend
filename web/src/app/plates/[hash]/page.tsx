"use client";

import { use, useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardBody } from "@/components/ui/Card";
import { Spinner } from "@/components/ui/Spinner";
import { Toast } from "@/components/ui/Toast";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import { useAlprSettings } from "@/lib/useAlprSettings";
import {
  ackPlateAlert,
  addPlateToWhitelist,
  fetchPlateDetail,
  fetchTripBriefForRoute,
  findRepresentativeDetectionId,
  removePlateFromWhitelist,
  unackPlateAlert,
  type PlateDetailResponse,
} from "@/lib/plateDetail";
import { classifyAlertSeverity } from "@/components/alerts/severity";
import { PlateHeader } from "@/components/plates/PlateHeader";
import { PlateStatsCard } from "@/components/plates/PlateStatsCard";
import { PlateEncounterList } from "@/components/plates/PlateEncounterList";
import { PlateEvidenceAccordion } from "@/components/plates/PlateEvidenceAccordion";
import {
  PlateSightingsMap,
  type PlateSightingPoint,
} from "@/components/plates/PlateSightingsMap";
import { EditPlateModal } from "@/components/plates/EditPlateModal";
import { MergePlateModal } from "@/components/plates/MergePlateModal";

interface PageParams {
  hash: string;
}

interface PlateDetailPageProps {
  // Next 15 makes params a Promise that callers unwrap with React's
  // `use` hook. Keeping the param shape as Promise<{hash: string}>
  // matches the framework's typing without needing a manual cast.
  params: Promise<PageParams>;
}

interface ToastState {
  message: string;
  variant: "success" | "error" | "info";
  /**
   * Optional action label + callback. When set, the toast renders an
   * inline button that, when clicked, invokes the action and dismisses
   * the toast. Used for the "merge?" hint flow.
   */
  action?: { label: string; onClick: () => void };
}

/**
 * Per-plate detail page. The page coordinates:
 *   - the GET /v1/plates/:hash fetch (with 404 -> empty state)
 *   - per-encounter trip lookups so the map can plot each first_seen
 *   - watchlist mutations (ack, unack, whitelist add/remove)
 *   - the edit + merge modals
 *
 * Behind the alpr feature flag: when ALPR is disabled in Settings,
 * the page renders a stub explanation rather than empty cards. The
 * shell still renders so a shared link does not 404 just because the
 * recipient hasn't enabled the feature.
 */
export default function PlateDetailPage({ params }: PlateDetailPageProps) {
  const { hash } = use(params);
  const { enabled, loading: flagLoading } = useAlprSettings();
  const router = useRouter();

  const [data, setData] = useState<PlateDetailResponse | null>(null);
  const [loading, setLoading] = useState<boolean>(true);
  const [notFound, setNotFound] = useState<boolean>(false);
  const [error, setError] = useState<string | null>(null);
  const [refetchTick, setRefetchTick] = useState<number>(0);

  // Per-encounter trip data: map keyed by "<dongle>|<route>". Loaded
  // separately from the plate detail so a slow trip lookup doesn't
  // block the rest of the page from painting.
  const [tripsByKey, setTripsByKey] = useState<
    Record<string, { lat: number; lng: number } | null>
  >({});

  // Watchlist-mutation flag so the action buttons disable in unison.
  const [actionInFlight, setActionInFlight] = useState<boolean>(false);
  const [toast, setToast] = useState<ToastState | null>(null);

  // Modal state.
  const [editOpen, setEditOpen] = useState<boolean>(false);
  const [editDetectionId, setEditDetectionId] = useState<number | null>(null);
  const [mergeOpen, setMergeOpen] = useState<boolean>(false);
  const [mergePrefill, setMergePrefill] = useState<string | null>(null);

  // Fetch the plate detail. We re-issue the fetch on refetchTick
  // bumps so post-mutation state stays consistent without
  // redirecting through a route push.
  useEffect(() => {
    if (enabled !== true) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    setNotFound(false);
    fetchPlateDetail(hash)
      .then((resp) => {
        if (cancelled) return;
        setData(resp);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        const status = (err as { status?: number }).status;
        if (status === 404) {
          setNotFound(true);
          return;
        }
        setError(
          err instanceof Error ? err.message : "Failed to load plate details.",
        );
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [hash, enabled, refetchTick]);

  // Resolve per-encounter trip data. We dedupe by (dongle, route) so
  // a plate seen multiple times on the same drive only triggers one
  // trip fetch. Kept separate from the page's main fetch so a 404
  // on one trip doesn't take down the map.
  useEffect(() => {
    if (!data) return;
    const seen = new Set<string>();
    const work: { dongle: string; route: string; key: string }[] = [];
    for (const enc of data.encounters) {
      const key = `${enc.dongle_id}|${enc.route}`;
      if (seen.has(key)) continue;
      seen.add(key);
      work.push({ dongle: enc.dongle_id, route: enc.route, key });
    }
    let cancelled = false;
    void Promise.all(
      work.map(async (w) => {
        try {
          const trip = await fetchTripBriefForRoute(w.dongle, w.route);
          if (
            trip &&
            trip.start_lat !== null &&
            trip.start_lng !== null &&
            Number.isFinite(trip.start_lat) &&
            Number.isFinite(trip.start_lng)
          ) {
            return { key: w.key, lat: trip.start_lat, lng: trip.start_lng };
          }
          return { key: w.key, lat: null, lng: null };
        } catch {
          // Trip fetch errors collapse to "no point on the map" so a
          // single offline trip doesn't break the whole map.
          return { key: w.key, lat: null, lng: null };
        }
      }),
    ).then((rows) => {
      if (cancelled) return;
      const next: Record<string, { lat: number; lng: number } | null> = {};
      for (const r of rows) {
        if (r.lat !== null && r.lng !== null) {
          next[r.key] = { lat: r.lat, lng: r.lng };
        } else {
          next[r.key] = null;
        }
      }
      setTripsByKey(next);
    });
    return () => {
      cancelled = true;
    };
  }, [data]);

  // ----- Map points (derived) -----
  const mapPoints = useMemo<PlateSightingPoint[]>(() => {
    if (!data) return [];
    const severityBucket = classifyAlertSeverity(
      data.watchlist_status?.severity ?? null,
    );
    const out: PlateSightingPoint[] = [];
    const seenKeys = new Set<string>();
    for (const enc of data.encounters) {
      const key = `${enc.dongle_id}|${enc.route}`;
      if (seenKeys.has(key)) continue;
      seenKeys.add(key);
      const trip = tripsByKey[key];
      if (!trip) continue;
      out.push({
        key,
        lat: trip.lat,
        lng: trip.lng,
        severity: severityBucket,
        label: `${enc.route} · ${formatTooltipTimestamp(enc.first_seen_ts)}`,
      });
    }
    return out;
  }, [data, tripsByKey]);

  // ----- Action handlers -----
  const triggerRefetch = useCallback(() => {
    setRefetchTick((n) => n + 1);
  }, []);

  const handleAck = useCallback(async () => {
    if (actionInFlight) return;
    setActionInFlight(true);
    try {
      await ackPlateAlert(hash);
      setToast({ message: "Alert acknowledged.", variant: "success" });
      triggerRefetch();
    } catch (err) {
      setToast({
        message: err instanceof Error ? err.message : "Failed to acknowledge.",
        variant: "error",
      });
    } finally {
      setActionInFlight(false);
    }
  }, [actionInFlight, hash, triggerRefetch]);

  const handleUnack = useCallback(async () => {
    if (actionInFlight) return;
    setActionInFlight(true);
    try {
      await unackPlateAlert(hash);
      setToast({ message: "Alert unacknowledged.", variant: "success" });
      triggerRefetch();
    } catch (err) {
      setToast({
        message: err instanceof Error ? err.message : "Failed to unacknowledge.",
        variant: "error",
      });
    } finally {
      setActionInFlight(false);
    }
  }, [actionInFlight, hash, triggerRefetch]);

  const handleAddWhitelist = useCallback(async () => {
    if (actionInFlight || !data) return;
    if (!data.plate) {
      // Without decrypted plate text the backend has no value to
      // hash, so we surface a clean error rather than POSTing an
      // empty plate.
      setToast({
        message: "Plate text is unavailable; cannot add to whitelist.",
        variant: "error",
      });
      return;
    }
    setActionInFlight(true);
    try {
      await addPlateToWhitelist(data.plate);
      setToast({ message: "Added to whitelist.", variant: "success" });
      triggerRefetch();
    } catch (err) {
      setToast({
        message: err instanceof Error ? err.message : "Failed to whitelist.",
        variant: "error",
      });
    } finally {
      setActionInFlight(false);
    }
  }, [actionInFlight, data, triggerRefetch]);

  const handleRemoveWhitelist = useCallback(async () => {
    if (actionInFlight) return;
    setActionInFlight(true);
    try {
      await removePlateFromWhitelist(hash);
      setToast({
        message: "Removed from whitelist.",
        variant: "success",
      });
      triggerRefetch();
    } catch (err) {
      setToast({
        message:
          err instanceof Error ? err.message : "Failed to remove from whitelist.",
        variant: "error",
      });
    } finally {
      setActionInFlight(false);
    }
  }, [actionInFlight, hash, triggerRefetch]);

  // Edit modal: resolve the most recent encounter's detection id
  // before opening so the modal can call PATCH directly. We do the
  // lookup lazily so the page doesn't pay for it on every render.
  const handleEdit = useCallback(async () => {
    if (!data || data.encounters.length === 0) {
      setToast({
        message: "No encounter is available to edit.",
        variant: "info",
      });
      return;
    }
    const recent = data.encounters[0];
    try {
      const id = await findRepresentativeDetectionId(
        recent.dongle_id,
        recent.route,
        data.plate_hash_b64,
      );
      setEditDetectionId(id);
      setEditOpen(true);
    } catch {
      // Fall back to opening the modal in its degraded state so the
      // operator can see why the action didn't work.
      setEditDetectionId(null);
      setEditOpen(true);
    }
  }, [data]);

  const handleEditSaved = useCallback(
    (resp: { hint?: string; match_hash_b64?: string }) => {
      triggerRefetch();
      if (resp.match_hash_b64) {
        const targetHash = resp.match_hash_b64;
        setToast({
          message:
            resp.hint ||
            "This plate now matches another in your history -- merge?",
          variant: "info",
          action: {
            label: "Merge",
            onClick: () => {
              setMergePrefill(targetHash);
              setMergeOpen(true);
            },
          },
        });
      } else {
        setToast({
          message: "Plate updated.",
          variant: "success",
        });
      }
    },
    [triggerRefetch],
  );

  const handleMerge = useCallback(() => {
    setMergePrefill(null);
    setMergeOpen(true);
  }, []);

  const handleMerged = useCallback(
    (toHashB64: string) => {
      // Navigate to the destination plate's page. The destination's
      // hash may have a special character (- or _) that must NOT be
      // URL-encoded; Next.js's router handles the path verbatim.
      router.push(`/plates/${toHashB64}`);
    },
    [router],
  );

  // ----- Render branches -----

  if (flagLoading) {
    return (
      <PageWrapper title="Plate detail">
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading" />
        </div>
      </PageWrapper>
    );
  }

  if (enabled !== true) {
    return (
      <PageWrapper title="Plate detail">
        <Card data-testid="plate-feature-disabled">
          <CardBody>
            <div className="py-8 text-center">
              <p className="text-sm font-medium text-[var(--text-primary)]">
                ALPR is disabled
              </p>
              <p className="mt-1 text-xs text-[var(--text-secondary)]">
                Enable ALPR in Settings to view plate history.
              </p>
            </div>
          </CardBody>
        </Card>
      </PageWrapper>
    );
  }

  if (loading && !data) {
    return (
      <PageWrapper title="Plate detail">
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading plate" />
        </div>
      </PageWrapper>
    );
  }

  if (notFound) {
    return (
      <PageWrapper title="Plate detail">
        <Card data-testid="plate-not-found">
          <CardBody>
            <div className="py-8 text-center">
              <p className="text-sm font-medium text-[var(--text-primary)]">
                Plate not found in your history
              </p>
              <p className="mt-1 text-xs text-[var(--text-secondary)]">
                Either the hash is malformed or no encounters exist for it.
              </p>
              <p className="mt-3 text-xs">
                <Link href="/alerts" className="text-[var(--accent)] hover:underline">
                  Back to alerts
                </Link>
              </p>
            </div>
          </CardBody>
        </Card>
      </PageWrapper>
    );
  }

  if (error) {
    return (
      <PageWrapper title="Plate detail">
        <ErrorMessage
          title="Failed to load plate"
          message={error}
          retry={triggerRefetch}
        />
      </PageWrapper>
    );
  }

  if (!data) {
    // Defensive: the loading branch above already covered initial
    // load, so reaching here means a cancelled fetch left state
    // null. Render a placeholder rather than crashing.
    return (
      <PageWrapper title="Plate detail">
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading plate" />
        </div>
      </PageWrapper>
    );
  }

  return (
    <PageWrapper>
      {toast && (
        <div className="mb-3">
          <Toast
            message={toast.message}
            variant={toast.variant}
            onDismiss={() => setToast(null)}
            dismissAfterMs={toast.action ? 0 : 3000}
          />
          {toast.action && (
            <button
              type="button"
              data-testid="plate-toast-action"
              onClick={() => {
                const action = toast.action;
                setToast(null);
                action?.onClick();
              }}
              className="mt-1 inline-flex items-center rounded bg-[var(--accent)] px-2 py-1 text-xs font-medium text-[var(--text-inverse)] hover:bg-[var(--accent-hover)]"
            >
              {toast.action.label}
            </button>
          )}
        </div>
      )}

      <PlateHeader
        plate={data.plate}
        plateHashB64={data.plate_hash_b64}
        signature={data.signature}
        watchlistStatus={data.watchlist_status}
        actions={{
          onAck: handleAck,
          onUnack: handleUnack,
          onAddWhitelist: handleAddWhitelist,
          onRemoveWhitelist: handleRemoveWhitelist,
          onEdit: () => {
            void handleEdit();
          },
          onMerge: handleMerge,
        }}
        busy={actionInFlight}
      />

      <PlateStatsCard stats={data.stats} />

      <Card className="mb-4" data-testid="plate-map-card">
        <CardBody className="px-0 py-0">
          <div className="h-[360px]">
            <PlateSightingsMap points={mapPoints} className="h-full w-full" />
          </div>
        </CardBody>
      </Card>

      <PlateEncounterList
        encounters={data.encounters}
        globalSeverity={data.watchlist_status?.severity ?? null}
      />

      <PlateEvidenceAccordion stats={data.stats} encounters={data.encounters} />

      <EditPlateModal
        open={editOpen}
        detectionId={editDetectionId}
        currentPlate={data.plate}
        onClose={() => setEditOpen(false)}
        onSaved={handleEditSaved}
      />
      <MergePlateModal
        open={mergeOpen}
        fromHashB64={data.plate_hash_b64}
        prefillToHashB64={mergePrefill}
        onClose={() => setMergeOpen(false)}
        onMerged={handleMerged}
      />
    </PageWrapper>
  );
}

/**
 * formatTooltipTimestamp condenses an ISO string to a "Apr 25 12:34"
 * form for the marker tooltip. The tooltip is one of the few
 * places the date is rendered in compact form; the encounter table
 * already shows the full local datetime.
 */
function formatTooltipTimestamp(iso: string): string {
  if (!iso) return "";
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return iso;
  return new Date(t).toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}
