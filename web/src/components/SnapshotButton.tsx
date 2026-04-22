"use client";

import { useCallback, useEffect, useState } from "react";
import { BASE_URL } from "@/lib/api";
import { Button } from "@/components/ui/Button";
import { Spinner } from "@/components/ui/Spinner";

interface SnapshotResponse {
  jpeg_back: string;
  jpeg_front: string;
}

interface SnapshotButtonProps {
  dongleId: string;
}

// SNAPSHOT_TIMEOUT_MS caps how long the UI will wait for the device to
// respond before surfacing a timeout error. The backend's own RPC timeout is
// 30s, but we prefer to fail sooner from the UI so operators are not left
// staring at a spinner.
const SNAPSHOT_TIMEOUT_MS = 20_000;

type SnapshotState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "success"; data: SnapshotResponse }
  | { kind: "error"; message: string };

/**
 * SnapshotButton renders a "Take Snapshot" button that posts to the backend's
 * per-device snapshot endpoint and displays the returned JPEGs in a modal
 * overlay. Errors (device offline, timeout, RPC failure) are surfaced as a
 * friendly message inside the modal so the operator can retry.
 */
export function SnapshotButton({ dongleId }: SnapshotButtonProps) {
  const [state, setState] = useState<SnapshotState>({ kind: "idle" });
  const [modalOpen, setModalOpen] = useState(false);

  const takeSnapshot = useCallback(async () => {
    setState({ kind: "loading" });
    setModalOpen(true);

    const controller = new AbortController();
    const timeoutId = window.setTimeout(
      () => controller.abort(),
      SNAPSHOT_TIMEOUT_MS,
    );

    try {
      const response = await fetch(
        `${BASE_URL}/v1/devices/${encodeURIComponent(dongleId)}/snapshot`,
        {
          method: "POST",
          credentials: "include",
          signal: controller.signal,
          headers: { "Content-Type": "application/json" },
        },
      );
      window.clearTimeout(timeoutId);

      if (!response.ok) {
        const message = await readErrorMessage(response);
        setState({ kind: "error", message });
        return;
      }

      const data: SnapshotResponse = await response.json();
      setState({ kind: "success", data });
    } catch (err) {
      window.clearTimeout(timeoutId);
      if (err instanceof DOMException && err.name === "AbortError") {
        setState({
          kind: "error",
          message:
            "Snapshot timed out. The device may be offline or the camera is busy.",
        });
        return;
      }
      setState({
        kind: "error",
        message: err instanceof Error ? err.message : "Failed to take snapshot",
      });
    }
  }, [dongleId]);

  const closeModal = useCallback(() => {
    setModalOpen(false);
    setState({ kind: "idle" });
  }, []);

  // Close on Escape whenever the modal is open.
  useEffect(() => {
    if (!modalOpen) return;
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") closeModal();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [modalOpen, closeModal]);

  return (
    <>
      <Button
        type="button"
        variant="primary"
        size="md"
        onClick={() => void takeSnapshot()}
        disabled={state.kind === "loading"}
      >
        {state.kind === "loading" ? "Taking snapshot..." : "Take Snapshot"}
      </Button>

      {modalOpen && (
        <SnapshotModal state={state} onClose={closeModal} onRetry={takeSnapshot} />
      )}
    </>
  );
}

interface SnapshotModalProps {
  state: SnapshotState;
  onClose: () => void;
  onRetry: () => void;
}

function SnapshotModal({ state, onClose, onRetry }: SnapshotModalProps) {
  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label="Snapshot"
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      onClick={(e) => {
        // Clicking the backdrop closes the modal; clicks on the inner panel
        // stop propagation below so they do not dismiss it.
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        className="w-full max-w-4xl rounded-lg bg-[var(--bg-surface)] p-6 shadow-2xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="mb-4 flex items-center justify-between">
          <h2 className="text-lg font-semibold text-[var(--text-primary)]">
            Camera snapshot
          </h2>
          <Button variant="ghost" size="sm" onClick={onClose} aria-label="Close">
            Close
          </Button>
        </div>

        {state.kind === "loading" && (
          <div className="flex min-h-[320px] items-center justify-center">
            <Spinner size="lg" label="Waiting for device..." />
          </div>
        )}

        {state.kind === "error" && (
          <SnapshotErrorState message={state.message} onRetry={onRetry} />
        )}

        {state.kind === "success" && <SnapshotImages data={state.data} />}
      </div>
    </div>
  );
}

interface SnapshotErrorStateProps {
  message: string;
  onRetry: () => void;
}

function SnapshotErrorState({ message, onRetry }: SnapshotErrorStateProps) {
  return (
    <div
      role="alert"
      className="flex min-h-[320px] flex-col items-center justify-center gap-4 rounded-md border border-danger-500/25 bg-danger-500/5 p-6 text-center"
    >
      <p className="text-sm font-medium text-danger-600 dark:text-danger-500">
        Could not take snapshot
      </p>
      <p className="text-sm text-[var(--text-secondary)]">{message}</p>
      <Button variant="secondary" size="sm" onClick={onRetry}>
        Try again
      </Button>
    </div>
  );
}

interface SnapshotImagesProps {
  data: SnapshotResponse;
}

function SnapshotImages({ data }: SnapshotImagesProps) {
  const hasBack = data.jpeg_back !== "";
  const hasFront = data.jpeg_front !== "";

  if (!hasBack && !hasFront) {
    return (
      <div className="flex min-h-[200px] items-center justify-center text-sm text-[var(--text-secondary)]">
        Device returned no images.
      </div>
    );
  }

  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <SnapshotImageCard
        label="Rear camera"
        dataUrl={hasBack ? data.jpeg_back : null}
      />
      <SnapshotImageCard
        label="Front camera"
        dataUrl={hasFront ? data.jpeg_front : null}
      />
    </div>
  );
}

interface SnapshotImageCardProps {
  label: string;
  dataUrl: string | null;
}

function SnapshotImageCard({ label, dataUrl }: SnapshotImageCardProps) {
  return (
    <figure className="flex flex-col gap-2">
      <figcaption className="text-xs uppercase tracking-wide text-[var(--text-secondary)]">
        {label}
      </figcaption>
      {dataUrl ? (
        // eslint-disable-next-line @next/next/no-img-element -- data URLs cannot be served through next/image.
        <img
          src={dataUrl}
          alt={label}
          className="w-full rounded border border-[var(--border-primary)] bg-black object-contain"
        />
      ) : (
        <div className="flex aspect-video items-center justify-center rounded border border-dashed border-[var(--border-primary)] text-xs text-[var(--text-secondary)]">
          Not available
        </div>
      )}
    </figure>
  );
}

// readErrorMessage extracts a human-readable message from an error response.
// The backend uses the {error, code} envelope consistently; fall back to the
// HTTP status text when the body is unreadable. 503 is rewritten to a
// clearer "device is offline" message in case some future code path returns
// 503 without the envelope.
async function readErrorMessage(response: Response): Promise<string> {
  try {
    const body = (await response.json()) as { error?: string };
    if (typeof body?.error === "string" && body.error !== "") {
      return body.error;
    }
  } catch {
    // fall through to the status-based message
  }
  if (response.status === 503) {
    return "The device is offline.";
  }
  if (response.status === 429) {
    return "Too many snapshots in a short time. Please wait a few seconds.";
  }
  return `Request failed: ${response.status} ${response.statusText}`;
}

