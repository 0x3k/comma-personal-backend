"use client";

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
} from "react";
import {
  getFullRouteDataRequest,
  requestFullRouteData,
} from "@/lib/api";
import type {
  RouteDataRequest,
  RouteDataRequestKind,
  RouteDataRequestPostResponse,
  RouteDataRequestStatusResponse,
} from "@/lib/types";
import { Button } from "@/components/ui/Button";

/**
 * Polling interval for the status endpoint while a request is in
 * flight. Five seconds matches the criterion in the feature spec --
 * device upload bursts are tens of seconds, so this is fast enough to
 * feel live without spamming the backend.
 */
const POLL_INTERVAL_MS = 5_000;

/**
 * Human-readable labels for each kind. Order matters: the menu walks
 * this array so users see the options in the same order the spec uses
 * (video first, then logs, then everything).
 */
const KIND_OPTIONS: ReadonlyArray<{ kind: RouteDataRequestKind; label: string }> =
  [
    { kind: "full_video", label: "Full video" },
    { kind: "full_logs", label: "Full logs" },
    { kind: "all", label: "Everything" },
  ];

function labelForKind(kind: RouteDataRequestKind): string {
  return KIND_OPTIONS.find((o) => o.kind === kind)?.label ?? kind;
}

/** Statuses for which polling is no longer useful. */
function isTerminalStatus(status: RouteDataRequest["status"]): boolean {
  return status === "complete" || status === "failed";
}

/** Statuses we consider "in-flight" -- not yet terminal. */
function isInFlightStatus(status: RouteDataRequest["status"]): boolean {
  return !isTerminalStatus(status);
}

export interface FullDataRequestControlProps {
  dongleId: string;
  routeName: string;
  /**
   * Called once a tracked request transitions into a `complete` status,
   * so the parent page can re-fetch the route detail (upload flags +
   * thumbnail + video player) and pick up the new files.
   */
  onComplete?: () => void;
}

/**
 * FullDataRequestControl is the "Get full quality" entry point on the
 * route detail page. It owns the menu of kinds (Full video / Full logs
 * / Everything), kicks off POSTs to the request_full_data endpoint, and
 * polls the GET endpoint at a fixed cadence while the tab is visible.
 *
 * Idempotency in the backend (1h window per route+kind) means clicking
 * the same option twice in quick succession will return the existing
 * row -- the UI surfaces that as the in-flight indicator rather than
 * showing a duplicate row, which matches operator expectations.
 *
 * No analytics are wired here on purpose: this is an admin-only
 * control, not a product-surface CTA.
 */
export function FullDataRequestControl({
  dongleId,
  routeName,
  onComplete,
}: FullDataRequestControlProps) {
  // Map from kind to the most-recent tracked request for that kind.
  // We key by kind (not request id) so a click on "Full video" while
  // "Full logs" is in-flight doesn't disturb the logs row.
  const [requests, setRequests] = useState<
    Partial<Record<RouteDataRequestKind, RouteDataRequest>>
  >({});

  // Per-kind error string for the "Try again" branch. Populated when
  // either a POST fails or a GET poll surfaces status=failed.
  const [errors, setErrors] = useState<
    Partial<Record<RouteDataRequestKind, string>>
  >({});

  // Per-kind in-flight POST flag. Used to disable buttons during the
  // network round-trip even before we have a request row to track.
  const [submitting, setSubmitting] = useState<
    Partial<Record<RouteDataRequestKind, boolean>>
  >({});

  // Most-recent progress block per kind. The POST endpoint does not
  // return progress, so we initialize this from the first GET poll.
  const [progress, setProgress] = useState<
    Partial<Record<RouteDataRequestKind, { uploaded: number; total: number }>>
  >({});

  // Open/closed state of the kind picker.
  const [menuOpen, setMenuOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement | null>(null);
  const triggerRef = useRef<HTMLButtonElement | null>(null);

  // Track which onComplete callbacks have already fired so we only
  // re-fetch the route once per request id.
  const completedSeenRef = useRef<Set<number>>(new Set());

  // -----------------------------------------------------------------
  // Polling
  // -----------------------------------------------------------------

  /** Fire-and-forget poll for a single request id. */
  const pollOnce = useCallback(
    async (kind: RouteDataRequestKind, requestId: number) => {
      let resp: RouteDataRequestStatusResponse;
      try {
        resp = await getFullRouteDataRequest(dongleId, routeName, requestId);
      } catch (err) {
        // Network blip: leave the row alone, the next tick will try
        // again. Surface the message in the inline status so the user
        // sees something is wrong instead of silently freezing on the
        // last good progress count.
        const msg =
          err instanceof Error ? err.message : "Failed to refresh status";
        setErrors((prev) => ({ ...prev, [kind]: msg }));
        return;
      }
      setErrors((prev) => {
        if (prev[kind] === undefined) return prev;
        const { [kind]: _, ...rest } = prev;
        return rest;
      });
      setRequests((prev) => ({ ...prev, [kind]: resp.request }));
      setProgress((prev) => ({
        ...prev,
        [kind]: {
          uploaded: resp.progress.filesUploaded,
          total: resp.progress.filesRequested,
        },
      }));
      if (
        resp.request.status === "complete" &&
        !completedSeenRef.current.has(resp.request.id)
      ) {
        completedSeenRef.current.add(resp.request.id);
        if (onComplete) onComplete();
      }
      if (resp.request.status === "failed") {
        // Stamp the error from the row so the inline banner can render
        // the reason the dispatcher recorded.
        setErrors((prev) => ({
          ...prev,
          [kind]:
            resp.request.error ??
            "Request failed -- the device returned an error",
        }));
      }
    },
    [dongleId, routeName, onComplete],
  );

  // Compute the kinds that are currently in flight so we know what to
  // poll. useMemo keeps the deps array stable across renders.
  const inFlightKinds = useMemo<RouteDataRequestKind[]>(() => {
    const out: RouteDataRequestKind[] = [];
    for (const opt of KIND_OPTIONS) {
      const r = requests[opt.kind];
      if (r && isInFlightStatus(r.status)) out.push(opt.kind);
    }
    return out;
  }, [requests]);

  // Polling effect. Fires immediately on mount-or-change, then every
  // POLL_INTERVAL_MS while the tab is visible. document.visibilityState
  // is consulted on each tick so a backgrounded tab pauses without
  // tearing down the timer.
  useEffect(() => {
    if (inFlightKinds.length === 0) return;
    let cancelled = false;

    const tick = () => {
      if (cancelled) return;
      if (
        typeof document === "undefined" ||
        document.visibilityState === "visible"
      ) {
        for (const kind of inFlightKinds) {
          const req = requests[kind];
          if (!req) continue;
          void pollOnce(kind, req.id);
        }
      }
    };
    // Kick off an immediate poll so the UI doesn't wait the full
    // interval to reflect server-side progress after a click.
    tick();
    const handle = window.setInterval(tick, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(handle);
    };
    // We intentionally exclude `requests` from the deps -- it gets
    // mutated by pollOnce inside the interval, and adding it would
    // restart the timer on every poll. inFlightKinds (which is
    // memoized off requests) is sufficient to detect the kinds going
    // in/out of flight.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [inFlightKinds, pollOnce]);

  // -----------------------------------------------------------------
  // Click handlers
  // -----------------------------------------------------------------

  const submitRequest = useCallback(
    async (kind: RouteDataRequestKind) => {
      // Block double-clicks: if a POST is already in flight for this
      // kind, or the existing row is in flight, do nothing.
      if (submitting[kind]) return;
      const existing = requests[kind];
      if (existing && isInFlightStatus(existing.status)) return;

      setSubmitting((prev) => ({ ...prev, [kind]: true }));
      setErrors((prev) => {
        if (prev[kind] === undefined) return prev;
        const { [kind]: _, ...rest } = prev;
        return rest;
      });
      try {
        const resp: RouteDataRequestPostResponse = await requestFullRouteData(
          dongleId,
          routeName,
          kind,
        );
        setRequests((prev) => ({ ...prev, [kind]: resp.request }));
        setProgress((prev) => ({
          ...prev,
          [kind]: { uploaded: 0, total: resp.request.filesRequested },
        }));
        // If the server immediately marked it failed (online + RPC
        // error path), surface the message right away so we don't
        // wait for a poll round-trip.
        if (resp.request.status === "failed") {
          setErrors((prev) => ({
            ...prev,
            [kind]:
              resp.request.error ??
              "Request failed -- the device returned an error",
          }));
        }
      } catch (err) {
        const msg =
          err instanceof Error ? err.message : "Failed to submit request";
        setErrors((prev) => ({ ...prev, [kind]: msg }));
      } finally {
        setSubmitting((prev) => ({ ...prev, [kind]: false }));
      }
    },
    [dongleId, routeName, requests, submitting],
  );

  const onPickKind = useCallback(
    (kind: RouteDataRequestKind) => {
      setMenuOpen(false);
      // Return focus to the trigger so screen readers and keyboard
      // users land somewhere predictable after the menu collapses.
      triggerRef.current?.focus();
      void submitRequest(kind);
    },
    [submitRequest],
  );

  const onRetry = useCallback(
    (kind: RouteDataRequestKind) => {
      // Clear the failed row so the disabled-while-in-flight check
      // doesn't accidentally suppress the retry click. The POST will
      // produce a fresh row regardless because the backend's
      // idempotency window does NOT apply once status=failed.
      setRequests((prev) => {
        const { [kind]: _, ...rest } = prev;
        return rest;
      });
      void submitRequest(kind);
    },
    [submitRequest],
  );

  // -----------------------------------------------------------------
  // Menu keyboard handling and outside-click close
  // -----------------------------------------------------------------

  useEffect(() => {
    if (!menuOpen) return;
    function onDocClick(e: MouseEvent) {
      if (!menuRef.current) return;
      if (e.target instanceof Node && menuRef.current.contains(e.target)) {
        return;
      }
      setMenuOpen(false);
    }
    document.addEventListener("mousedown", onDocClick);
    return () => document.removeEventListener("mousedown", onDocClick);
  }, [menuOpen]);

  const onMenuKeyDown = useCallback(
    (e: KeyboardEvent<HTMLDivElement>) => {
      if (e.key === "Escape") {
        e.preventDefault();
        setMenuOpen(false);
        triggerRef.current?.focus();
      }
    },
    [],
  );

  // -----------------------------------------------------------------
  // Render helpers
  // -----------------------------------------------------------------

  /**
   * Compose the inline status string for a given kind. Returns null
   * when there is nothing to show (no row tracked, no error). The
   * string is what the aria-live region announces, so keep it short
   * and punctuated.
   */
  function statusText(kind: RouteDataRequestKind): string | null {
    const err = errors[kind];
    const req = requests[kind];
    if (req && req.status === "failed") {
      return `${labelForKind(kind)} failed: ${err ?? req.error ?? "unknown error"}`;
    }
    if (err && !req) {
      return `${labelForKind(kind)} request failed: ${err}`;
    }
    if (!req) return null;
    if (req.status === "complete") {
      return `${labelForKind(kind)}: complete`;
    }
    if (req.status === "pending") {
      return `${labelForKind(kind)}: device offline -- queued`;
    }
    const p = progress[kind];
    if (p && p.total > 0) {
      return `${labelForKind(kind)}: uploading ${p.uploaded} / ${p.total} files`;
    }
    return `${labelForKind(kind)}: dispatched`;
  }

  // The aria-live region concatenates every kind's status so a single
  // polite announcement covers both kinds when the user has, for
  // example, full_video and full_logs running side by side.
  const liveText = KIND_OPTIONS.map((o) => statusText(o.kind))
    .filter((s): s is string => s !== null)
    .join(" -- ");

  // The trigger is disabled only when ALL three kinds are in flight,
  // since the menu still has nothing useful to show in that state.
  // Per-option disabling happens inside the menu items.
  const allInFlight = KIND_OPTIONS.every((opt) => {
    const r = requests[opt.kind];
    return r ? isInFlightStatus(r.status) : false;
  });

  return (
    <div className="flex flex-col gap-2">
      <div ref={menuRef} className="relative" onKeyDown={onMenuKeyDown}>
        <Button
          ref={triggerRef}
          variant="secondary"
          size="sm"
          onClick={() => setMenuOpen((v) => !v)}
          aria-haspopup="menu"
          aria-expanded={menuOpen}
          aria-label="Request full-resolution data for this drive"
          disabled={allInFlight}
        >
          Get full quality
        </Button>
        {menuOpen && (
          <ul
            role="menu"
            aria-label="Choose what to pull from the device"
            className="absolute right-0 top-full z-10 mt-1 w-48 overflow-hidden rounded border border-[var(--border-secondary)] bg-[var(--bg-surface)] shadow-sm"
          >
            {KIND_OPTIONS.map((opt) => {
              const req = requests[opt.kind];
              const inFlight = req ? isInFlightStatus(req.status) : false;
              const isSubmitting = submitting[opt.kind] ?? false;
              const disabled = inFlight || isSubmitting;
              return (
                <li key={opt.kind} role="none">
                  <button
                    type="button"
                    role="menuitem"
                    onClick={() => onPickKind(opt.kind)}
                    disabled={disabled}
                    className={[
                      "block w-full px-3 py-2 text-left text-sm",
                      "text-[var(--text-primary)] hover:bg-[var(--bg-tertiary)]",
                      "disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-transparent",
                      "focus:outline-none focus:bg-[var(--bg-tertiary)]",
                    ].join(" ")}
                  >
                    {opt.label}
                    {disabled && (
                      <span className="ml-2 text-xs text-[var(--text-secondary)]">
                        in progress
                      </span>
                    )}
                  </button>
                </li>
              );
            })}
          </ul>
        )}
      </div>

      {/* Per-kind inline status rows. We render one row per kind that
          has a tracked request OR a residual error so the UI is
          self-describing without needing to open a separate panel. */}
      <div
        aria-live="polite"
        aria-atomic="true"
        className="flex flex-col gap-1 text-xs"
      >
        {/*
         * The visually-hidden text is the canonical SR announcement
         * (single sentence per kind, easy to parse). The visible rows
         * below carry the same information but with a Try-again
         * affordance for failed kinds.
         */}
        <span className="sr-only">{liveText}</span>
        {KIND_OPTIONS.map((opt) => {
          const text = statusText(opt.kind);
          const req = requests[opt.kind];
          const failed =
            (req && req.status === "failed") ||
            (errors[opt.kind] !== undefined && req === undefined);
          if (text === null) return null;
          return (
            <div
              key={opt.kind}
              className={[
                "flex items-center justify-between gap-2 rounded border px-2 py-1",
                failed
                  ? "border-danger-500/25 bg-danger-500/5 text-danger-600 dark:text-danger-500"
                  : "border-[var(--border-secondary)] bg-[var(--bg-primary)] text-[var(--text-secondary)]",
              ].join(" ")}
            >
              <span>{text}</span>
              {failed && (
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => onRetry(opt.kind)}
                  disabled={submitting[opt.kind] ?? false}
                  aria-label={`Try ${labelForKind(opt.kind).toLowerCase()} again`}
                >
                  Try again
                </Button>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
