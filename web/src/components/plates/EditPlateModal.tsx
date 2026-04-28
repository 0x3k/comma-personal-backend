"use client";

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/Button";
import { editDetection, type EditDetectionResponse } from "@/lib/plateDetail";

interface EditPlateModalProps {
  /**
   * Detection id of the representative encounter the modal is editing.
   * The page resolves this from the most-recent encounter's route via
   * findRepresentativeDetectionId before opening the modal.
   *
   * `null` puts the modal in a degraded "no editable detection found"
   * state. The page only opens the modal once it has an id, but a
   * race between resolution and the user clicking Edit is possible.
   */
  detectionId: number | null;
  /**
   * Current plate text -- prefilled into the input. May be the empty
   * string when every encounter ciphertext failed to decrypt; in that
   * case the modal still allows the operator to type a fresh value.
   */
  currentPlate: string;
  open: boolean;
  onClose: () => void;
  /**
   * Called after a successful PATCH. Receives the response so the
   * page can refetch + decide whether to surface a merge hint.
   */
  onSaved: (resp: EditDetectionResponse) => void;
}

/**
 * EditPlateModal lets the operator manually correct a misread plate.
 * Renders an inline overlay (no portal -- the project does not have
 * a portal pattern yet, and a fixed-position overlay is sufficient
 * for the few modals on this page).
 *
 * The form is intentionally minimal: a single text input and a save
 * button. Empty submissions are blocked client-side; the server also
 * rejects them with a 400, but the inline guard is the friendlier
 * surface.
 *
 * On save success we close the modal AND hand the response up via
 * onSaved. Surfacing the hint is the page's job because the merge
 * modal lives on the page, not here.
 */
export function EditPlateModal({
  detectionId,
  currentPlate,
  open,
  onClose,
  onSaved,
}: EditPlateModalProps) {
  const [value, setValue] = useState<string>(currentPlate);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState<boolean>(false);

  // Reset the form on open/close + when the seed plate changes; the
  // modal can be opened multiple times for the same plate, and we
  // want a fresh state each time.
  useEffect(() => {
    if (open) {
      setValue(currentPlate);
      setError(null);
      setSubmitting(false);
    }
  }, [open, currentPlate]);

  if (!open) return null;

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (submitting) return;
    if (detectionId === null) {
      setError("No editable detection available for this plate.");
      return;
    }
    const trimmed = value.trim();
    if (!trimmed) {
      setError("Plate text cannot be empty.");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const resp = await editDetection(detectionId, trimmed);
      onSaved(resp);
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to edit plate.");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="edit-plate-modal-title"
      data-testid="edit-plate-modal"
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
              id="edit-plate-modal-title"
              className="text-sm font-semibold text-[var(--text-primary)]"
            >
              Edit plate text
            </h2>
            <p className="mt-1 text-xs text-[var(--text-secondary)]">
              Rewrites the most-recent detection. The hash is recomputed
              and other encounters of the same plate continue to match.
            </p>
          </div>
          <div className="px-4 py-4">
            <label
              htmlFor="edit-plate-input"
              className="block text-xs font-medium text-[var(--text-secondary)]"
            >
              Plate text
            </label>
            <input
              id="edit-plate-input"
              data-testid="edit-plate-input"
              type="text"
              value={value}
              onChange={(e) => setValue(e.target.value.toUpperCase())}
              spellCheck={false}
              autoComplete="off"
              className="mt-1 block w-full rounded-md border border-[var(--border-primary)] bg-[var(--bg-secondary)] px-3 py-2 font-mono text-sm text-[var(--text-primary)] focus:border-[var(--accent)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]"
              autoFocus
            />
            {detectionId === null && (
              <p
                className="mt-2 text-xs text-[var(--text-tertiary)]"
                data-testid="edit-plate-no-detection"
              >
                No editable detection was found for this plate. Try
                opening the most recent route directly.
              </p>
            )}
            {error && (
              <p
                className="mt-2 text-xs text-[var(--color-danger-500)]"
                data-testid="edit-plate-error"
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
              disabled={submitting || detectionId === null}
              data-testid="edit-plate-submit"
            >
              {submitting ? "Saving..." : "Save"}
            </Button>
          </div>
        </form>
      </div>
    </div>
  );
}
