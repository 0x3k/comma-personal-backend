"use client";

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/Button";
import { mergePlates } from "@/lib/plateDetail";

interface MergePlateModalProps {
  /**
   * The "from" plate hash -- always the currently-viewed plate. The
   * destination is taken from the input field. Merging this plate
   * means folding it into the destination's identity.
   */
  fromHashB64: string;
  /**
   * Optional pre-filled destination hash, supplied by the edit
   * modal's hint flow. The user can still edit it before submitting.
   */
  prefillToHashB64?: string | null;
  open: boolean;
  onClose: () => void;
  /**
   * Called with the destination hash after a successful merge so the
   * page can navigate. We do not navigate from the modal directly
   * because the page owns routing context.
   */
  onMerged: (toHashB64: string) => void;
}

/**
 * Trim and strip surrounding /plates/ URL prefix so the user can
 * paste either a raw hash or the full plate URL. The merge endpoint
 * accepts both base64-url forms (with or without padding); we leave
 * that normalization to the server.
 */
function normalizeHashInput(raw: string): string {
  const trimmed = raw.trim();
  // Accept "/plates/<hash>" or absolute URLs ending in that path.
  const match = trimmed.match(/\/plates\/([^/?#]+)/);
  if (match) return match[1];
  return trimmed;
}

/**
 * MergePlateModal folds two plate identities together. Submission
 * calls POST /v1/alpr/plates/merge with from = current plate, to =
 * the value typed in the input. On 200 we hand the destination back
 * to the page via onMerged so it can navigate.
 *
 * The plate-text-search shortcut described in the spec ("type a
 * plate, resolve to hash") is omitted: the backend has no
 * plate-text -> hash lookup endpoint exposed today, and adding one
 * would be its own feature. The hash-paste path is the documented
 * primary surface, and the edit modal's hint flow already provides
 * a one-click pre-fill.
 */
export function MergePlateModal({
  fromHashB64,
  prefillToHashB64,
  open,
  onClose,
  onMerged,
}: MergePlateModalProps) {
  const [value, setValue] = useState<string>(prefillToHashB64 ?? "");
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState<boolean>(false);

  useEffect(() => {
    if (open) {
      setValue(prefillToHashB64 ?? "");
      setError(null);
      setSubmitting(false);
    }
  }, [open, prefillToHashB64]);

  if (!open) return null;

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (submitting) return;
    const target = normalizeHashInput(value);
    if (!target) {
      setError("Paste the destination plate's hash (or its /plates/<hash> URL).");
      return;
    }
    if (target === fromHashB64) {
      setError("Source and destination must differ.");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await mergePlates(fromHashB64, target);
      onMerged(target);
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to merge plates.");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="merge-plate-modal-title"
      data-testid="merge-plate-modal"
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4"
      onClick={onClose}
    >
      <div
        className="w-full max-w-md rounded-lg border border-[var(--border-primary)] bg-[var(--bg-surface)] shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <form onSubmit={onSubmit}>
          <div className="border-b border-[var(--border-primary)] px-4 py-3">
            <h2
              id="merge-plate-modal-title"
              className="text-sm font-semibold text-[var(--text-primary)]"
            >
              Merge into another plate
            </h2>
            <p className="mt-1 text-xs text-[var(--text-secondary)]">
              Folds this plate&apos;s detections, encounters, and
              watchlist row into the destination. The destination is
              kept; this plate&apos;s history disappears from the
              feed.
            </p>
          </div>
          <div className="px-4 py-4">
            <label
              htmlFor="merge-plate-input"
              className="block text-xs font-medium text-[var(--text-secondary)]"
            >
              Destination hash (or paste a /plates/&lt;hash&gt; URL)
            </label>
            <input
              id="merge-plate-input"
              data-testid="merge-plate-input"
              type="text"
              value={value}
              onChange={(e) => setValue(e.target.value)}
              spellCheck={false}
              autoComplete="off"
              className="mt-1 block w-full rounded-md border border-[var(--border-primary)] bg-[var(--bg-secondary)] px-3 py-2 font-mono text-xs text-[var(--text-primary)] focus:border-[var(--accent)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]"
              autoFocus
            />
            {error && (
              <p
                className="mt-2 text-xs text-[var(--color-danger-500)]"
                data-testid="merge-plate-error"
              >
                {error}
              </p>
            )}
          </div>
          <div className="flex items-center justify-end gap-2 border-t border-[var(--border-primary)] px-4 py-3">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={onClose}
              disabled={submitting}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              variant="primary"
              size="sm"
              disabled={submitting}
              data-testid="merge-plate-submit"
            >
              {submitting ? "Merging..." : "Merge"}
            </Button>
          </div>
        </form>
      </div>
    </div>
  );
}

// Exported for tests so they can validate the URL-parsing branch
// without spinning up a full DOM.
export { normalizeHashInput };
