"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button } from "@/components/ui/Button";
import { Card, CardBody } from "@/components/ui/Card";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import { Toast } from "@/components/ui/Toast";
import {
  ackAlert,
  listAlerts,
  unackAlert,
  type AlertItem,
} from "./api";
import { AlertsList, type AlertRowControl } from "./AlertsList";
import {
  alertWithinDateRange,
  type AlertsFilterState,
} from "./filters";

interface AlertsTabProps {
  /** Active tab -- "open" or "acked". */
  mode: "open" | "acked";
  filters: AlertsFilterState;
  /**
   * Empty-state messages shown when zero rows. Distinct strings per
   * tab per the spec ("No open alerts" vs "No acknowledged alerts
   * yet"); the parent supplies them so the page owns the copy.
   */
  emptyTitle: string;
  emptyDescription?: string;
}

/**
 * AlertsTab is the Open / Acknowledged tab body. Owns the row state
 * map (selection, in-flight, failed, optimistic ack) so toggling
 * tabs preserves a clean list per tab. The parent page swaps the
 * mode prop; we re-derive everything from filters + mode.
 */
export function AlertsTab({
  mode,
  filters,
  emptyTitle,
  emptyDescription,
}: AlertsTabProps) {
  const [alerts, setAlerts] = useState<AlertItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [rowState, setRowState] = useState<Record<string, AlertRowControl | undefined>>(
    {},
  );
  const [bulkInFlight, setBulkInFlight] = useState(false);
  const [toast, setToast] = useState<{
    message: string;
    variant: "success" | "error" | "info";
  } | null>(null);

  // refetchSeq lets handlers force a refresh after a mutation
  // without coupling to the filter effect; bumping the ref triggers
  // the effect that fetches.
  const refetchSeq = useRef(0);
  const [refetchTick, setRefetchTick] = useState(0);

  const triggerRefetch = useCallback(() => {
    refetchSeq.current += 1;
    setRefetchTick(refetchSeq.current);
  }, []);

  // Fetch the alert list whenever mode/filters change. The backend
  // applies severity + dongle_id; date range is filtered client-side
  // because the listing endpoint doesn't accept a date range yet.
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    listAlerts({
      status: mode,
      severity: filters.severity.slice(),
      dongle_id: filters.dongle_id || undefined,
      // Intentionally request a generous limit -- the spec asks for a
      // virtualized list that handles the full feed without paging.
      limit: 100,
      offset: 0,
    })
      .then((data) => {
        if (cancelled) return;
        const filtered = data.alerts.filter((a) =>
          alertWithinDateRange(a.last_alert_at, filters.from, filters.to),
        );
        setAlerts(filtered);
        // Reset row state on new data so a stale "in flight" mark
        // from a prior request doesn't leak into the new list.
        setRowState({});
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : "Failed to load alerts");
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [mode, filters, refetchTick]);

  const onToggleSelect = useCallback((hash: string) => {
    setRowState((prev) => {
      const cur = prev[hash];
      return {
        ...prev,
        [hash]: {
          selected: !(cur?.selected ?? false),
          ackInFlight: cur?.ackInFlight ?? false,
          failed: cur?.failed ?? false,
          ackedOptimistic: cur?.ackedOptimistic ?? null,
        },
      };
    });
  }, []);

  const onSelectAll = useCallback(
    (next: boolean) => {
      setRowState((prev) => {
        const out: Record<string, AlertRowControl> = {};
        for (const a of alerts) {
          const cur = prev[a.plate_hash_b64];
          out[a.plate_hash_b64] = {
            selected: next,
            ackInFlight: cur?.ackInFlight ?? false,
            failed: cur?.failed ?? false,
            ackedOptimistic: cur?.ackedOptimistic ?? null,
          };
        }
        return out;
      });
    },
    [alerts],
  );

  const onAckSingle = useCallback(
    async (item: AlertItem) => {
      const hash = item.plate_hash_b64;
      // Optimistic ack: flip the row state immediately so the button
      // re-labels without waiting for the round-trip. On failure we
      // roll back AND mark the row as failed (red border).
      setRowState((prev) => ({
        ...prev,
        [hash]: {
          selected: prev[hash]?.selected ?? false,
          ackInFlight: true,
          failed: false,
          ackedOptimistic: true,
        },
      }));
      try {
        await ackAlert(hash);
        setRowState((prev) => ({
          ...prev,
          [hash]: {
            selected: false,
            ackInFlight: false,
            failed: false,
            ackedOptimistic: true,
          },
        }));
        // Refetch so the row drops out of the open tab (or appears in
        // the acked tab when the user switches).
        triggerRefetch();
      } catch (err) {
        setRowState((prev) => ({
          ...prev,
          [hash]: {
            selected: prev[hash]?.selected ?? false,
            ackInFlight: false,
            failed: true,
            ackedOptimistic: null,
          },
        }));
        setToast({
          message: err instanceof Error ? err.message : "Failed to ack alert",
          variant: "error",
        });
      }
    },
    [triggerRefetch],
  );

  const onUnackSingle = useCallback(
    async (item: AlertItem) => {
      const hash = item.plate_hash_b64;
      setRowState((prev) => ({
        ...prev,
        [hash]: {
          selected: prev[hash]?.selected ?? false,
          ackInFlight: true,
          failed: false,
          ackedOptimistic: false,
        },
      }));
      try {
        await unackAlert(hash);
        setRowState((prev) => ({
          ...prev,
          [hash]: {
            selected: false,
            ackInFlight: false,
            failed: false,
            ackedOptimistic: false,
          },
        }));
        triggerRefetch();
      } catch (err) {
        setRowState((prev) => ({
          ...prev,
          [hash]: {
            selected: prev[hash]?.selected ?? false,
            ackInFlight: false,
            failed: true,
            ackedOptimistic: null,
          },
        }));
        setToast({
          message:
            err instanceof Error ? err.message : "Failed to unack alert",
          variant: "error",
        });
      }
    },
    [triggerRefetch],
  );

  const selectedHashes = useMemo(
    () =>
      Object.entries(rowState)
        .filter(([, v]) => v?.selected)
        .map(([k]) => k),
    [rowState],
  );

  const onAckSelected = useCallback(async () => {
    if (selectedHashes.length === 0 || bulkInFlight) return;
    if (mode !== "open") return;
    setBulkInFlight(true);
    setToast({
      message: `Acknowledging ${selectedHashes.length} alert${selectedHashes.length === 1 ? "" : "s"}...`,
      variant: "info",
    });

    // Mark every selected row in-flight + optimistic acked.
    setRowState((prev) => {
      const next = { ...prev };
      for (const h of selectedHashes) {
        const cur = next[h];
        next[h] = {
          selected: true,
          ackInFlight: true,
          failed: false,
          ackedOptimistic: true,
        };
      }
      return next;
    });

    // Promise.allSettled so a partial failure doesn't reject the whole
    // batch. Each result is mapped back to its hash by index.
    const results = await Promise.allSettled(
      selectedHashes.map((h) => ackAlert(h)),
    );

    let successCount = 0;
    let failCount = 0;
    setRowState((prev) => {
      const next = { ...prev };
      for (let i = 0; i < selectedHashes.length; i++) {
        const h = selectedHashes[i];
        const r = results[i];
        if (r.status === "fulfilled") {
          successCount += 1;
          next[h] = {
            selected: false,
            ackInFlight: false,
            failed: false,
            ackedOptimistic: true,
          };
        } else {
          failCount += 1;
          next[h] = {
            selected: true,
            ackInFlight: false,
            failed: true,
            ackedOptimistic: null,
          };
        }
      }
      return next;
    });

    setBulkInFlight(false);

    if (failCount === 0) {
      setToast({
        message: `Acknowledged ${successCount} alert${successCount === 1 ? "" : "s"}`,
        variant: "success",
      });
      // Refetch so the rows drop out of the open tab.
      triggerRefetch();
    } else if (successCount === 0) {
      // Full failure: rollback the optimistic state. The rows stay in
      // the list with the failed marker so the operator can retry.
      setToast({
        message: `Failed to ack ${failCount} alert${failCount === 1 ? "" : "s"}`,
        variant: "error",
      });
    } else {
      setToast({
        message: `Acknowledged ${successCount}, ${failCount} failed`,
        variant: "error",
      });
      // Partial: refetch so the successful ones drop out, the failed
      // ones remain marked red.
      triggerRefetch();
    }
  }, [bulkInFlight, mode, selectedHashes, triggerRefetch]);

  // ----- Render branches ---------------------------------------------

  if (loading) {
    return (
      <div className="flex items-center justify-center py-16">
        <Spinner size="lg" label="Loading alerts" />
      </div>
    );
  }

  if (error) {
    return (
      <ErrorMessage
        title="Failed to load alerts"
        message={error}
        retry={triggerRefetch}
      />
    );
  }

  if (alerts.length === 0) {
    return (
      <Card>
        <CardBody>
          <div className="py-8 text-center" data-testid="alerts-empty">
            <p className="text-sm font-medium text-[var(--text-primary)]">
              {emptyTitle}
            </p>
            {emptyDescription && (
              <p className="mt-1 text-xs text-[var(--text-secondary)]">
                {emptyDescription}
              </p>
            )}
          </div>
        </CardBody>
      </Card>
    );
  }

  return (
    <div className="flex flex-col gap-3">
      {toast && (
        <Toast
          message={toast.message}
          variant={toast.variant}
          onDismiss={() => setToast(null)}
        />
      )}

      {/* Bulk-ack CTA. Only visible on the Open tab; the Acked tab
          doesn't need a bulk-unack workflow per the spec. */}
      {mode === "open" && (
        <div
          className="flex items-center justify-between rounded-lg border border-[var(--border-primary)] bg-[var(--bg-secondary)] px-3 py-2"
          data-testid="alerts-bulk-bar"
        >
          <span className="text-xs text-[var(--text-secondary)]">
            {selectedHashes.length === 0
              ? `${alerts.length} alert${alerts.length === 1 ? "" : "s"}`
              : `${selectedHashes.length} selected`}
          </span>
          <Button
            variant="primary"
            size="sm"
            disabled={selectedHashes.length === 0 || bulkInFlight}
            onClick={() => {
              void onAckSelected();
            }}
            data-testid="alerts-bulk-ack"
          >
            {bulkInFlight ? "Acknowledging..." : "Ack selected"}
          </Button>
        </div>
      )}

      <AlertsList
        alerts={alerts}
        mode={mode}
        rowState={rowState}
        onToggleSelect={onToggleSelect}
        onSelectAll={onSelectAll}
        onAckSingle={(item) => {
          void onAckSingle(item);
        }}
        onUnackSingle={(item) => {
          void onUnackSingle(item);
        }}
      />
    </div>
  );
}
