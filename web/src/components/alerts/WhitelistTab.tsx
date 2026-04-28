"use client";

import { useCallback, useEffect, useState } from "react";
import { Button } from "@/components/ui/Button";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import { Toast } from "@/components/ui/Toast";
import {
  addWhitelist,
  listWhitelist,
  removeWhitelist,
  type WhitelistItem,
} from "./api";

/**
 * EmptyWhitelistMessage matches the spec's empty-state text. Pulled
 * out as a constant so the test asserts the same string the user
 * sees, without copy-paste drift.
 */
const EMPTY_TEXT = "No whitelisted plates";

/**
 * formatCreatedAt renders the row timestamp. Both date and time are
 * shown so the operator can spot stale or freshly-added entries; the
 * existing format helpers don't have a "compact-with-time" variant
 * so we inline a toLocaleString call.
 */
function formatCreatedAt(iso: string | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

/**
 * normalizePlate runs the same client-side check the backend applies:
 * strip spaces / dashes / dots / tabs, uppercase, and reject empty
 * strings. Mirrors normalizePlateForCheck in alpr_watchlist.go so the
 * UI surfaces the validation error before the round-trip.
 */
function normalizePlate(raw: string): string {
  return raw
    .toUpperCase()
    .replace(/[\s\-.]+/g, "");
}

/**
 * WhitelistTab is the simple non-virtualized list + add form. The
 * spec caps the expected size at <100 entries, so pagination and
 * virtualization are deliberately omitted -- a flat list is denser
 * and easier to scan for the operator.
 */
export function WhitelistTab() {
  const [items, setItems] = useState<WhitelistItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const [plate, setPlate] = useState("");
  const [label, setLabel] = useState("");
  const [adding, setAdding] = useState(false);
  const [addError, setAddError] = useState<string | null>(null);
  const [toast, setToast] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await listWhitelist();
      // The API returns items in update-order DESC (newest first). We
      // surface them verbatim; if the server changes ordering the page
      // tracks it without a client-side sort.
      setItems(data.whitelist);
    } catch (err) {
      setError(
        err instanceof Error ? err.message : "Failed to load whitelist",
      );
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const handleAdd = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      const normalized = normalizePlate(plate);
      if (!normalized) {
        // Surface validation inline before round-tripping. Matches the
        // backend's 400 response so the user sees the same message
        // regardless of whether the JS check or the server caught it.
        setAddError("Plate is empty after normalization");
        return;
      }
      setAdding(true);
      setAddError(null);
      try {
        await addWhitelist(plate, label || undefined);
        setPlate("");
        setLabel("");
        setToast(`Added ${normalized} to whitelist`);
        // Refresh to pick up the new row + the canonical decrypted
        // plate text. The list is small so a full refetch is cheap.
        await refresh();
      } catch (err) {
        setAddError(err instanceof Error ? err.message : "Failed to add plate");
      } finally {
        setAdding(false);
      }
    },
    [plate, label, refresh],
  );

  const handleRemove = useCallback(
    async (item: WhitelistItem) => {
      // Optimistic remove: take the row out of the list immediately,
      // then put it back if the server rejects. The whitelist is
      // small, so a full refetch on success keeps the displayed
      // metadata (timestamps) accurate.
      const before = items;
      setItems((prev) =>
        prev.filter((i) => i.plate_hash_b64 !== item.plate_hash_b64),
      );
      try {
        await removeWhitelist(item.plate_hash_b64);
        setToast(`Removed ${item.plate || item.label || "entry"}`);
      } catch (err) {
        // Roll back the optimistic remove and surface the error.
        setItems(before);
        setError(
          err instanceof Error
            ? err.message
            : "Failed to remove whitelist entry",
        );
      }
    },
    [items],
  );

  return (
    <div className="flex flex-col gap-4">
      {/* Add form. Renders above the list so the most common action
          (adding a new entry) is the first thing the user sees on
          this tab. */}
      <Card>
        <CardHeader>
          <h2 className="text-sm font-medium text-[var(--text-primary)]">
            Add to whitelist
          </h2>
        </CardHeader>
        <CardBody>
          <form
            onSubmit={handleAdd}
            data-testid="whitelist-add-form"
            className="flex flex-col gap-2 sm:flex-row sm:items-end"
          >
            <div className="flex flex-1 flex-col gap-1">
              <label
                htmlFor="whitelist-plate"
                className="text-xs text-[var(--text-secondary)]"
              >
                Plate
              </label>
              <input
                id="whitelist-plate"
                type="text"
                required
                value={plate}
                onChange={(e) => {
                  setPlate(e.target.value);
                  if (addError) setAddError(null);
                }}
                placeholder="e.g. ABC123"
                className="w-full rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-2 py-1 font-mono text-sm text-[var(--text-primary)] focus:border-[var(--accent)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]"
              />
            </div>
            <div className="flex flex-1 flex-col gap-1">
              <label
                htmlFor="whitelist-label"
                className="text-xs text-[var(--text-secondary)]"
              >
                Label (optional)
              </label>
              <input
                id="whitelist-label"
                type="text"
                value={label}
                onChange={(e) => setLabel(e.target.value)}
                placeholder="e.g. Family minivan"
                className="w-full rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-2 py-1 text-sm text-[var(--text-primary)] focus:border-[var(--accent)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]"
              />
            </div>
            <Button
              type="submit"
              variant="primary"
              size="md"
              disabled={adding}
              data-testid="whitelist-add-submit"
            >
              {adding ? "Adding..." : "Add"}
            </Button>
          </form>
          {addError && (
            <p
              className="mt-2 text-xs text-danger-600 dark:text-danger-500"
              role="alert"
            >
              {addError}
            </p>
          )}
        </CardBody>
      </Card>

      {/* Toast notifications. We render inline (no portal) and let
          the Toast component handle auto-dismissal. */}
      {toast && (
        <Toast
          message={toast}
          variant="success"
          onDismiss={() => setToast(null)}
        />
      )}

      {/* List body. Spinner / error / empty / list. */}
      {loading && (
        <div className="flex items-center justify-center py-12">
          <Spinner size="md" label="Loading whitelist" />
        </div>
      )}

      {error && (
        <ErrorMessage
          title="Failed to load whitelist"
          message={error}
          retry={() => {
            void refresh();
          }}
        />
      )}

      {!loading && !error && items.length === 0 && (
        <Card>
          <CardBody>
            <div className="py-8 text-center">
              <p className="text-sm font-medium text-[var(--text-primary)]">
                {EMPTY_TEXT}
              </p>
              <p className="mt-1 text-xs text-[var(--text-secondary)]">
                Add a plate above to suppress alerts for that vehicle.
              </p>
            </div>
          </CardBody>
        </Card>
      )}

      {!loading && !error && items.length > 0 && (
        <Card>
          {/* Custom non-virtualized table. The list is small (<100)
              so HTML table semantics + flex are simpler than reaching
              for TanStack Table. */}
          <div className="grid grid-cols-12 gap-2 border-b border-[var(--border-primary)] bg-[var(--bg-secondary)] px-3 py-2 text-xs font-medium text-[var(--text-secondary)]">
            <span className="col-span-3">Plate</span>
            <span className="col-span-4">Label</span>
            <span className="col-span-3">Added</span>
            <span className="col-span-2 text-right">Action</span>
          </div>
          <ul data-testid="whitelist-items">
            {items.map((item) => (
              <li
                key={item.plate_hash_b64}
                data-testid={`whitelist-row-${item.plate_hash_b64}`}
                className="grid grid-cols-12 items-center gap-2 border-b border-[var(--border-primary)] px-3 py-2 text-xs last:border-b-0"
              >
                <span className="col-span-3 truncate font-mono text-[var(--text-primary)]">
                  {item.plate || (
                    <span className="text-[var(--text-tertiary)]">
                      {item.plate_hash_b64.slice(0, 10)}
                    </span>
                  )}
                </span>
                <span className="col-span-4 truncate text-[var(--text-secondary)]">
                  {item.label || (
                    <span className="text-[var(--text-tertiary)]">—</span>
                  )}
                </span>
                <span className="col-span-3 text-[var(--text-secondary)]">
                  {formatCreatedAt(item.created_at)}
                </span>
                <span className="col-span-2 text-right">
                  <Button
                    variant="ghost"
                    size="sm"
                    data-testid={`whitelist-remove-${item.plate_hash_b64}`}
                    onClick={() => {
                      void handleRemove(item);
                    }}
                  >
                    Remove
                  </Button>
                </span>
              </li>
            ))}
          </ul>
        </Card>
      )}
    </div>
  );
}

export { EMPTY_TEXT as WHITELIST_EMPTY_TEXT };
