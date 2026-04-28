"use client";

import { Button } from "@/components/ui/Button";
import {
  classifyAlertSeverity,
  SEVERITY_COLOR,
  type SeverityBucket,
} from "@/components/alerts/severity";
import type {
  PlateSignature,
  PlateWatchlistStatus,
} from "@/lib/plateDetail";

/**
 * High-level "what is this plate" labels surfaced as the header pill.
 * The four states mirror the spec exactly so screenshots match the
 * acceptance criteria text.
 */
export type WatchlistPillState =
  | { kind: "open"; severity: number | null }
  | { kind: "acked" }
  | { kind: "whitelist" }
  | { kind: "none" };

/**
 * derivePillState collapses a watchlist row into the header pill's
 * display state. The mapping:
 *   - watchlist row, kind=alerted, acked_at=null  -> open (with sev)
 *   - watchlist row, kind=alerted, acked_at!=null -> acked
 *   - watchlist row, kind=whitelist                -> whitelist
 *   - no watchlist row                             -> none
 */
export function derivePillState(
  status: PlateWatchlistStatus | null,
): WatchlistPillState {
  if (!status) return { kind: "none" };
  if (status.kind === "whitelist") return { kind: "whitelist" };
  // alerted row: open vs acked is determined by acked_at
  if (status.acked_at) return { kind: "acked" };
  return { kind: "open", severity: status.severity };
}

interface PlateHeaderProps {
  plate: string;
  plateHashB64: string;
  signature: PlateSignature | null;
  watchlistStatus: PlateWatchlistStatus | null;
  /**
   * Each action exposes whether it is currently in flight so the
   * button can disable itself while the page handles the click. The
   * page also disables the unrelated buttons via the same flags
   * (e.g. ack while editing) so two mutations cannot overlap.
   */
  actions: {
    onAck: () => void;
    onUnack: () => void;
    onAddWhitelist: () => void;
    onRemoveWhitelist: () => void;
    onEdit: () => void;
    onMerge: () => void;
  };
  /** True while any of the watchlist mutations is in flight. */
  busy?: boolean;
}

/**
 * PlateHeader renders the dense top strip of the plate-detail page:
 * the plate text in big monospace, the vehicle badge with a colour
 * dot, the watchlist status pill, and the action buttons. The pill
 * + buttons are computed from the watchlist status; the page is
 * responsible for wiring the action callbacks.
 */
export function PlateHeader({
  plate,
  plateHashB64,
  signature,
  watchlistStatus,
  actions,
  busy = false,
}: PlateHeaderProps) {
  const pill = derivePillState(watchlistStatus);
  const vehicleLabel = formatVehicleLabel(signature);

  return (
    <div className="mb-6 flex flex-col gap-3" data-testid="plate-header">
      <div className="flex flex-wrap items-center gap-3">
        <h1
          className="font-mono text-3xl font-semibold tracking-wide text-[var(--text-primary)]"
          data-testid="plate-header-text"
        >
          {plate || plateHashB64.slice(0, 12)}
        </h1>
        {vehicleLabel && (
          <VehicleBadge color={signature?.color ?? null} label={vehicleLabel} />
        )}
        <WatchlistPill state={pill} />
      </div>

      <div className="flex flex-wrap items-center gap-2">
        {pill.kind === "open" && (
          <Button
            variant="primary"
            size="sm"
            onClick={actions.onAck}
            disabled={busy}
            data-testid="plate-action-ack"
          >
            Acknowledge
          </Button>
        )}
        {pill.kind === "acked" && (
          <Button
            variant="secondary"
            size="sm"
            onClick={actions.onUnack}
            disabled={busy}
            data-testid="plate-action-unack"
          >
            Unacknowledge
          </Button>
        )}
        {pill.kind === "whitelist" ? (
          <Button
            variant="secondary"
            size="sm"
            onClick={actions.onRemoveWhitelist}
            disabled={busy}
            data-testid="plate-action-remove-whitelist"
          >
            Remove from whitelist
          </Button>
        ) : (
          <Button
            variant="secondary"
            size="sm"
            onClick={actions.onAddWhitelist}
            disabled={busy}
            data-testid="plate-action-add-whitelist"
          >
            Add to whitelist
          </Button>
        )}
        <Button
          variant="ghost"
          size="sm"
          onClick={actions.onEdit}
          disabled={busy}
          data-testid="plate-action-edit"
        >
          Edit plate
        </Button>
        <Button
          variant="ghost"
          size="sm"
          onClick={actions.onMerge}
          disabled={busy}
          data-testid="plate-action-merge"
        >
          Merge into another plate
        </Button>
      </div>
    </div>
  );
}

/**
 * VehicleBadge mirrors the alert-row vehicle badge: a coloured dot
 * (the actual signature colour, when one is known) plus the make /
 * model text. We map the freeform colour string onto a small CSS
 * palette so a typo in the signature doesn't paint an unreadable
 * dot.
 */
function VehicleBadge({ color, label }: { color: string | null; label: string }) {
  const dotColor = vehicleDotColor(color);
  return (
    <span
      className="inline-flex items-center gap-1.5 rounded-full bg-[var(--bg-secondary)] px-2.5 py-1 text-xs font-medium text-[var(--text-secondary)]"
      data-testid="plate-header-vehicle"
    >
      <span
        aria-hidden="true"
        className="inline-block h-2.5 w-2.5 rounded-full ring-1 ring-inset ring-[var(--border-primary)]"
        style={{ background: dotColor }}
      />
      {label}
    </span>
  );
}

interface WatchlistPillProps {
  state: WatchlistPillState;
}

function WatchlistPill({ state }: WatchlistPillProps) {
  if (state.kind === "none") {
    return (
      <span
        data-testid="plate-header-status"
        data-status="none"
        className="inline-flex items-center gap-1.5 rounded-full bg-[var(--bg-secondary)] px-2.5 py-1 text-xs font-medium text-[var(--text-secondary)]"
      >
        No alerts
      </span>
    );
  }
  if (state.kind === "whitelist") {
    return (
      <span
        data-testid="plate-header-status"
        data-status="whitelist"
        className="inline-flex items-center gap-1.5 rounded-full bg-success-500/15 px-2.5 py-1 text-xs font-medium text-success-600"
      >
        Whitelisted
      </span>
    );
  }
  if (state.kind === "acked") {
    return (
      <span
        data-testid="plate-header-status"
        data-status="acked"
        className="inline-flex items-center gap-1.5 rounded-full bg-[var(--bg-secondary)] px-2.5 py-1 text-xs font-medium text-[var(--text-secondary)]"
      >
        Acknowledged
      </span>
    );
  }
  // Open alert: pick severity colour (defensive: classifyAlertSeverity
  // accepts null and yields "none" so we always have a bucket).
  const bucket: SeverityBucket = classifyAlertSeverity(state.severity);
  return (
    <span
      data-testid="plate-header-status"
      data-status="open"
      data-severity-bucket={bucket}
      className="inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-medium"
      style={{
        background: `color-mix(in srgb, ${SEVERITY_COLOR[bucket]} 18%, transparent)`,
        color: SEVERITY_COLOR[bucket],
      }}
    >
      <span
        aria-hidden="true"
        className="inline-block h-2 w-2 rounded-full"
        style={{ background: SEVERITY_COLOR[bucket] }}
      />
      Open alert{state.severity !== null ? ` sev ${state.severity}` : ""}
    </span>
  );
}

/**
 * formatVehicleLabel picks the most useful one-line vehicle label
 * for the badge. Mirrors the alert-row formatter so the two surfaces
 * agree on style.
 */
function formatVehicleLabel(sig: PlateSignature | null): string {
  if (!sig) return "";
  const make = sig.make?.trim();
  const model = sig.model?.trim();
  const parts: string[] = [];
  if (make && model) parts.push(`${make} ${model}`);
  else if (make) parts.push(make);
  else if (model) parts.push(model);
  // Color is rendered as the dot, not the text label, so it is
  // intentionally not appended here.
  return parts.join(" ");
}

/**
 * vehicleDotColor maps a freeform colour string to a hex value. We
 * keep the palette short -- expanding it doesn't add information,
 * just rendering noise -- and fall back to neutral grey for unknown
 * or empty inputs.
 */
function vehicleDotColor(name: string | null): string {
  if (!name) return "var(--color-neutral-400)";
  const norm = name.trim().toLowerCase();
  switch (norm) {
    case "black":
      return "#1f2937";
    case "white":
      return "#f9fafb";
    case "silver":
    case "gray":
    case "grey":
      return "#9ca3af";
    case "red":
      return "#ef4444";
    case "blue":
      return "#3b82f6";
    case "green":
      return "#10b981";
    case "yellow":
      return "#eab308";
    case "orange":
      return "#f97316";
    case "brown":
      return "#92400e";
    case "purple":
    case "violet":
      return "#8b5cf6";
    default:
      return "var(--color-neutral-400)";
  }
}
