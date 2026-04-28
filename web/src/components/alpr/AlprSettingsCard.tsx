"use client";

import {
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { apiFetch } from "@/lib/api";
import {
  invalidateAlprSettings,
  type AlprSettings,
} from "@/lib/useAlprSettings";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";

/**
 * The disclaimer revision the UI presents and posts on ack. Mirror of the
 * backend's ALPRDisclaimerCurrentVersion in internal/api/settings_alpr.go;
 * the two MUST stay in sync. A bump on either side invalidates prior acks
 * and re-prompts the operator on the next enable.
 */
export const DISCLAIMER_VERSION = "2026-04-v1";

/**
 * Engine-reachability poll cadence while the card is in STATE 3
 * (enabled, engine offline). Matches the spec: probe every 10s, give up
 * after 30 attempts (~5 minutes) so we do not hammer the engine forever
 * if the operator has walked away from the page.
 */
const PROBE_INTERVAL_MS = 10_000;
const PROBE_MAX_ATTEMPTS = 30;

/**
 * Closed set of jurisdictions accepted by the disclaimer-ack endpoint.
 * Mirror of validJurisdictions in internal/api/settings_alpr.go.
 */
export type JurisdictionCode = "us" | "eu" | "uk" | "ca" | "au" | "other";

const JURISDICTION_OPTIONS: Array<{ code: JurisdictionCode; label: string }> = [
  { code: "us", label: "United States" },
  { code: "eu", label: "European Union" },
  { code: "uk", label: "United Kingdom" },
  { code: "ca", label: "Canada" },
  { code: "au", label: "Australia" },
  { code: "other", label: "Other / Not listed" },
];

/**
 * Per-jurisdiction disclaimer body shown in the modal. Each entry is a
 * tight UI-friendly summary of the longer treatment in docs/ALPR.md and
 * deliberately ends with a "this is not legal advice" line so the operator
 * is reminded to consult counsel. Editing these is a material change to
 * the disclaimer; bump DISCLAIMER_VERSION (and the backend constant) when
 * you do.
 */
export const DISCLAIMER_TEXT: Record<JurisdictionCode, ReactNode> = {
  us: (
    <>
      <p>
        Federal U.S. law does not directly regulate dashcam plate
        recognition for personal use, but several states do. Illinois has
        the strictest biometric privacy regime (BIPA) and related rulings
        have moved toward broad PII protection; California, Virginia,
        Colorado, and others have privacy statutes that may apply once
        plate text is paired with location and timestamp. Some
        municipalities also restrict storing or sharing plate data.
      </p>
      <p>
        You are responsible for confirming that recording, retaining, and
        analyzing license plate data from your own dashcam is permitted in
        your state and municipality, and for honoring any
        retention/disclosure limits that apply. Do not share recognized
        plate data outside your household without an independent legal
        review.
      </p>
      <p className="font-medium">
        This is not legal advice. Consult counsel licensed in your
        jurisdiction before enabling ALPR on data you intend to keep.
      </p>
    </>
  ),
  eu: (
    <>
      <p>
        Under GDPR, a license plate combined with a location and a
        timestamp is personal data. The household-use exemption is
        narrowly construed: as soon as you share the data outside your
        household, route it through a third party, or retain it beyond
        the time strictly necessary, you are almost certainly a
        controller subject to GDPR (lawful basis, minimization,
        retention limits, data-subject rights, and breach notification).
      </p>
      <p>
        Local supervisory authorities have published dashcam guidance
        that varies materially across member states. You are responsible
        for confirming a lawful basis, posting any required notices, and
        honoring access/erasure requests for plates you retain.
      </p>
      <p className="font-medium">
        This is not legal advice. Consult counsel licensed in your
        jurisdiction before enabling ALPR on data you intend to keep.
      </p>
    </>
  ),
  uk: (
    <>
      <p>
        Under the UK GDPR and Data Protection Act 2018, dashcam plate
        data combined with location is personal data and the
        household-use exemption is narrow. The ICO has published
        specific guidance for dashcam recordings in public places; if
        you retain or share the footage, you are likely a controller
        with the corresponding obligations (lawful basis, retention
        limits, subject access, and breach notification).
      </p>
      <p>
        You are responsible for confirming a lawful basis, applying a
        defensible retention period, and responding to subject-access or
        erasure requests for plates you keep.
      </p>
      <p className="font-medium">
        This is not legal advice. Consult counsel licensed in your
        jurisdiction before enabling ALPR on data you intend to keep.
      </p>
    </>
  ),
  ca: (
    <>
      <p>
        Under PIPEDA and Canadian provincial privacy laws (PIPA in BC,
        Alberta, and Quebec&rsquo;s Law 25), collecting plate data from public
        space for personal safety is generally permitted, but commercial
        use, sharing outside your household, or retention beyond a
        reasonable purpose triggers full PIPEDA obligations (consent,
        purpose limitation, safeguards, access requests, breach
        reporting).
      </p>
      <p>
        Provincial differences are material -- Quebec&rsquo;s Law 25 in
        particular imposes stricter notification and consent
        requirements than federal PIPEDA. You are responsible for
        confirming compliance with the regime that applies to you.
      </p>
      <p className="font-medium">
        This is not legal advice. Consult counsel licensed in your
        jurisdiction before enabling ALPR on data you intend to keep.
      </p>
    </>
  ),
  au: (
    <>
      <p>
        Under the Privacy Act 1988 and the Australian Privacy Principles,
        a license plate plus location is likely personal information once
        it is associated with an individual. The Act applies to most
        organizations and to natural persons in commercial contexts; the
        domestic-use exception is narrow and does not extend to data you
        share or publish.
      </p>
      <p>
        State-level surveillance and listening-device laws differ across
        jurisdictions and may impose additional limits on recording in
        public space. You are responsible for confirming what applies to
        you and honoring any retention or notification requirements.
      </p>
      <p className="font-medium">
        This is not legal advice. Consult counsel licensed in your
        jurisdiction before enabling ALPR on data you intend to keep.
      </p>
    </>
  ),
  other: (
    <>
      <p>
        Recording dashcam video in public is generally legal in most
        countries, but retaining recognized plate text combined with
        location and time crosses into regulated territory in many
        jurisdictions. National data-protection regimes,
        surveillance-device statutes, and municipal ordinances all
        apply. The personal/household-use exception is generally narrow
        and does not extend to data you share or publish.
      </p>
      <p>
        You are responsible for confirming that recording, retaining,
        and analyzing license plate data from your own dashcam is
        permitted where you live and operate the vehicle, and for
        honoring any retention or disclosure limits that apply.
      </p>
      <p className="font-medium">
        This is not legal advice. Consult counsel licensed in your
        jurisdiction before enabling ALPR on data you intend to keep.
      </p>
    </>
  ),
};

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

/**
 * AlprSettingsWire mirrors the JSON returned by GET /v1/settings/alpr. We
 * keep our own copy (rather than re-using the camelCase AlprSettings type)
 * so this card can hit the endpoint freshly on its own polling schedule
 * without depending on or invalidating the dashboard-wide cache.
 */
interface AlprSettingsWire {
  enabled: boolean;
  engine_url: string;
  region: string;
  frames_per_second: number;
  confidence_min: number;
  retention_days_unflagged: number;
  retention_days_flagged: number;
  notify_min_severity: number;
  encryption_key_configured: boolean;
  engine_reachable: boolean;
  disclaimer_required: boolean;
  disclaimer_version: string;
  disclaimer_acked_at: string | null;
  disclaimer_acked_jurisdiction: string | null;
}

interface AlertsSummaryWire {
  open_count: number;
  max_open_severity: number | null;
  last_alert_at: string | null;
}

function fromWire(w: AlprSettingsWire): AlprSettings {
  return {
    enabled: w.enabled,
    engineUrl: w.engine_url,
    region: w.region,
    framesPerSecond: w.frames_per_second,
    confidenceMin: w.confidence_min,
    retentionDaysUnflagged: w.retention_days_unflagged,
    retentionDaysFlagged: w.retention_days_flagged,
    notifyMinSeverity: w.notify_min_severity,
    encryptionKeyConfigured: w.encryption_key_configured,
    engineReachable: w.engine_reachable,
    disclaimerRequired: w.disclaimer_required,
    disclaimerVersion: w.disclaimer_version,
    disclaimerAckedAt: w.disclaimer_acked_at,
    disclaimerAckedJurisdiction: w.disclaimer_acked_jurisdiction,
  };
}

// ---------------------------------------------------------------------------
// State derivation
// ---------------------------------------------------------------------------

/**
 * Visual state of the ALPR card. The mapping from server state is:
 *   STATE_1_DISABLED          -> !enabled (regardless of key) -- disabled card
 *   STATE_3_ENGINE_OFFLINE    -> enabled && !engine_reachable
 *   STATE_4_ENGINE_ONLINE     -> enabled && engine_reachable
 * STATE_2 is purely a transient overlay shown when the user clicks Enable;
 * it is not reachable directly from the GET response.
 */
type CardState =
  | "STATE_1_DISABLED"
  | "STATE_3_ENGINE_OFFLINE"
  | "STATE_4_ENGINE_ONLINE";

function deriveState(s: AlprSettings): CardState {
  if (!s.enabled) return "STATE_1_DISABLED";
  if (!s.engineReachable) return "STATE_3_ENGINE_OFFLINE";
  return "STATE_4_ENGINE_ONLINE";
}

// ---------------------------------------------------------------------------
// Live stats fetcher
// ---------------------------------------------------------------------------

interface LiveStats {
  alerts24h: number | null;
  lastAlertAt: string | null;
}

// ---------------------------------------------------------------------------
// Disclaimer modal
// ---------------------------------------------------------------------------

interface DisclaimerModalProps {
  initialJurisdiction: JurisdictionCode;
  encryptionKeyConfigured: boolean;
  onClose: () => void;
  /**
   * Resolves once the ack + enable flow has completed successfully. The
   * caller refetches settings on resolve so the card transitions to
   * STATE 3 / STATE 4.
   */
  onConfirmed: () => void;
}

function DisclaimerModal({
  initialJurisdiction,
  encryptionKeyConfigured,
  onClose,
  onConfirmed,
}: DisclaimerModalProps) {
  const [jurisdiction, setJurisdiction] =
    useState<JurisdictionCode>(initialJurisdiction);
  const [consented, setConsented] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const headingId = useId();
  const consentId = useId();

  async function handleConfirm() {
    setSubmitting(true);
    setError(null);
    try {
      // Ack first; PUT enable second. Doing them in this order means a
      // failure between the two leaves the operator with a recorded ack
      // and a still-disabled flag (the safe direction).
      await apiFetch<unknown>("/v1/settings/alpr/disclaimer/ack", {
        method: "POST",
        body: { jurisdiction, version: DISCLAIMER_VERSION },
      });
      await apiFetch<unknown>("/v1/settings/alpr", {
        method: "PUT",
        body: { enabled: true },
      });
      // Drop the dashboard-wide cache so any other consumer of
      // useAlprSettings reflects the new state on its next mount.
      invalidateAlprSettings();
      onConfirmed();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to enable ALPR");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby={headingId}
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      onClick={(e) => {
        if (e.target === e.currentTarget && !submitting) onClose();
      }}
    >
      <div
        className="w-full max-w-2xl overflow-hidden rounded-lg bg-[var(--bg-surface)] shadow-2xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between gap-3 border-b border-[var(--border-primary)] px-6 py-4">
          <h2
            id={headingId}
            className="text-lg font-semibold text-[var(--text-primary)]"
          >
            Enable license plate recognition
          </h2>
          <Button
            variant="ghost"
            size="sm"
            onClick={onClose}
            disabled={submitting}
            aria-label="Close"
          >
            Close
          </Button>
        </div>

        <div className="max-h-[70vh] overflow-y-auto px-6 py-4">
          {!encryptionKeyConfigured ? (
            <div className="space-y-4 text-sm text-[var(--text-primary)]">
              <p>
                Set <code className="rounded bg-[var(--bg-tertiary)] px-1 py-0.5 font-mono text-xs">ALPR_ENCRYPTION_KEY</code>{" "}
                in your environment and restart the server -- this key
                encrypts plate data at rest.
              </p>
              <p>
                Generate one with:
              </p>
              <pre className="overflow-x-auto rounded border border-[var(--border-primary)] bg-[var(--bg-tertiary)] px-3 py-2 font-mono text-xs text-[var(--text-primary)]">
                go run ./cmd/alpr-keygen
              </pre>
              <p>
                Once the key is set and the server is back up, return here
                to enable ALPR.
              </p>
            </div>
          ) : (
            <div className="space-y-4">
              <div>
                <label
                  htmlFor={`${headingId}-jurisdiction`}
                  className="mb-1 block text-sm font-medium text-[var(--text-secondary)]"
                >
                  Your jurisdiction
                </label>
                <select
                  id={`${headingId}-jurisdiction`}
                  value={jurisdiction}
                  onChange={(e) =>
                    setJurisdiction(e.target.value as JurisdictionCode)
                  }
                  disabled={submitting}
                  className="w-full rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-3 py-1.5 text-sm text-[var(--text-primary)] outline-none focus:ring-2 focus:ring-[var(--ring-focus)]"
                >
                  {JURISDICTION_OPTIONS.map((opt) => (
                    <option key={opt.code} value={opt.code}>
                      {opt.label}
                    </option>
                  ))}
                </select>
                <p className="mt-1 text-xs text-[var(--text-tertiary)]">
                  Recorded in the audit log so you have a defensible trail
                  if the legal landscape shifts later.
                </p>
              </div>

              <div
                data-testid="disclaimer-text"
                className="space-y-3 rounded border border-[var(--border-primary)] bg-[var(--bg-tertiary)] px-4 py-3 text-sm text-[var(--text-primary)] [&>p]:leading-relaxed"
              >
                {DISCLAIMER_TEXT[jurisdiction]}
              </div>

              <label
                htmlFor={consentId}
                className="flex cursor-pointer items-start gap-3 text-sm text-[var(--text-primary)]"
              >
                <input
                  id={consentId}
                  type="checkbox"
                  checked={consented}
                  onChange={(e) => setConsented(e.target.checked)}
                  disabled={submitting}
                  className="mt-0.5 h-4 w-4"
                />
                <span>
                  I understand and consent to processing license plates
                  from my own dashcam footage for personal
                  stalking-detection purposes only.
                </span>
              </label>

              {error && (
                <p className="text-sm text-danger-500" role="alert">
                  {error}
                </p>
              )}
            </div>
          )}
        </div>

        <div className="flex items-center justify-end gap-2 border-t border-[var(--border-primary)] px-6 py-3">
          <Button
            variant="ghost"
            onClick={onClose}
            disabled={submitting}
          >
            {encryptionKeyConfigured ? "Cancel" : "Close"}
          </Button>
          {encryptionKeyConfigured && (
            <Button
              onClick={() => void handleConfirm()}
              disabled={!consented || submitting}
            >
              {submitting ? "Enabling..." : "Confirm and enable"}
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Disable confirm modal
// ---------------------------------------------------------------------------

interface DisableConfirmModalProps {
  onClose: () => void;
  onConfirmed: () => void;
}

function DisableConfirmModal({ onClose, onConfirmed }: DisableConfirmModalProps) {
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const headingId = useId();

  async function handleConfirm() {
    setSubmitting(true);
    setError(null);
    try {
      await apiFetch<unknown>("/v1/settings/alpr", {
        method: "PUT",
        body: { enabled: false },
      });
      invalidateAlprSettings();
      onConfirmed();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to disable ALPR");
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby={headingId}
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      onClick={(e) => {
        if (e.target === e.currentTarget && !submitting) onClose();
      }}
    >
      <div
        className="w-full max-w-md overflow-hidden rounded-lg bg-[var(--bg-surface)] shadow-2xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="border-b border-[var(--border-primary)] px-6 py-4">
          <h2
            id={headingId}
            className="text-lg font-semibold text-[var(--text-primary)]"
          >
            Disable ALPR?
          </h2>
        </div>
        <div className="px-6 py-4 text-sm text-[var(--text-primary)]">
          <p>
            Workers will stop processing new routes and the dashboard
            surfaces will hide. Existing detection data is retained until
            retention policy or manual deletion. Re-enable any time.
          </p>
          {error && (
            <p className="mt-3 text-sm text-danger-500" role="alert">
              {error}
            </p>
          )}
        </div>
        <div className="flex items-center justify-end gap-2 border-t border-[var(--border-primary)] px-6 py-3">
          <Button variant="ghost" onClick={onClose} disabled={submitting}>
            Cancel
          </Button>
          <Button
            variant="danger"
            onClick={() => void handleConfirm()}
            disabled={submitting}
          >
            {submitting ? "Disabling..." : "Disable ALPR"}
          </Button>
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function relativeTimeFrom(iso: string | null | undefined): string {
  if (!iso) return "never";
  const ts = Date.parse(iso);
  if (!Number.isFinite(ts)) return "unknown";
  const deltaMs = Date.now() - ts;
  if (deltaMs < 0) return "just now";
  const sec = Math.floor(deltaMs / 1000);
  if (sec < 60) return `${sec}s ago`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const day = Math.floor(hr / 24);
  return `${day}d ago`;
}

interface CopyableCommandProps {
  command: string;
  label: string;
}

function CopyableCommand({ command, label }: CopyableCommandProps) {
  const [copied, setCopied] = useState(false);

  async function handleCopy() {
    try {
      // navigator.clipboard is gated on a secure context; we fall back
      // to a temporary textarea + execCommand to keep the button useful
      // when the dashboard is served over plain HTTP on a LAN.
      if (typeof navigator !== "undefined" && navigator.clipboard) {
        await navigator.clipboard.writeText(command);
      } else if (typeof document !== "undefined") {
        const ta = document.createElement("textarea");
        ta.value = command;
        document.body.appendChild(ta);
        ta.select();
        document.execCommand("copy");
        document.body.removeChild(ta);
      }
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // Silent failure -- the command is still visible for manual copy.
    }
  }

  return (
    <div className="flex flex-col gap-1">
      <span className="text-xs font-medium text-[var(--text-secondary)]">
        {label}
      </span>
      <div className="flex items-stretch gap-2">
        <pre className="flex-1 overflow-x-auto rounded border border-[var(--border-primary)] bg-[var(--bg-tertiary)] px-3 py-2 font-mono text-xs text-[var(--text-primary)]">
          {command}
        </pre>
        <Button
          type="button"
          variant="secondary"
          size="sm"
          onClick={() => void handleCopy()}
          aria-label={`Copy: ${command}`}
        >
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// State-specific bodies
// ---------------------------------------------------------------------------

interface State1Props {
  encryptionKeyConfigured: boolean;
  onEnableClick: () => void;
}

function State1Disabled({ encryptionKeyConfigured, onEnableClick }: State1Props) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-[var(--text-primary)]">
        Detect license plates in your dashcam footage and warn when the
        same vehicle appears across multiple trips. All processing is
        local; plates are encrypted at rest. Off by default.
      </p>
      {!encryptionKeyConfigured && (
        <div
          role="alert"
          data-testid="alpr-encryption-key-warning"
          className="rounded border border-warning-500/30 bg-warning-500/10 px-3 py-2 text-sm text-warning-600"
        >
          <p className="font-medium">Encryption key not configured.</p>
          <p className="mt-1 text-xs">
            Set <code className="font-mono">ALPR_ENCRYPTION_KEY</code> in
            your server environment and restart before enabling ALPR.
            Generate one with{" "}
            <code className="font-mono">go run ./cmd/alpr-keygen</code>.
          </p>
        </div>
      )}
      <div>
        <Button
          onClick={onEnableClick}
          disabled={!encryptionKeyConfigured}
          aria-disabled={!encryptionKeyConfigured}
        >
          Enable...
        </Button>
      </div>
    </div>
  );
}

interface State3Props {
  attempts: number;
  probing: boolean;
  giveUp: boolean;
  onResume: () => void;
  onDisableClick: () => void;
}

function State3EngineOffline({
  attempts,
  probing,
  giveUp,
  onResume,
  onDisableClick,
}: State3Props) {
  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-3">
        <Badge variant="warning" data-testid="alpr-engine-status">
          Engine offline
        </Badge>
        <span className="text-xs text-[var(--text-tertiary)]">
          {giveUp
            ? "Stopped probing"
            : probing
              ? `Probing... (attempt ${attempts}/${PROBE_MAX_ATTEMPTS})`
              : `Next probe in ~${Math.round(PROBE_INTERVAL_MS / 1000)}s`}
        </span>
      </div>
      <p className="text-sm text-[var(--text-primary)]">
        ALPR is enabled but the detection engine is not responding. Start
        the sidecar container with one of the commands below; the card
        will notice automatically within a few seconds.
      </p>
      <div className="space-y-3">
        <CopyableCommand label="Preferred" command="make alpr-up" />
        <CopyableCommand
          label="Fallback (raw docker compose)"
          command="docker compose --profile alpr up -d alpr"
        />
      </div>
      {giveUp && (
        <div>
          <Button variant="secondary" onClick={onResume}>
            Stopped probing -- click to retry
          </Button>
        </div>
      )}
      <div className="flex justify-end border-t border-[var(--border-primary)] pt-3">
        <Button variant="ghost" onClick={onDisableClick}>
          Disable ALPR
        </Button>
      </div>
    </div>
  );
}

interface State4Props {
  settings: AlprSettings;
  stats: LiveStats | null;
  statsError: string | null;
  onDisableClick: () => void;
}

function State4EngineOnline({
  settings,
  stats,
  statsError,
  onDisableClick,
}: State4Props) {
  const [advancedOpen, setAdvancedOpen] = useState(false);

  const queueDepth = "unavailable";
  const lastAlertRel = stats ? relativeTimeFrom(stats.lastAlertAt) : "n/a";

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-3">
        <Badge variant="success" data-testid="alpr-engine-status">
          Engine online
        </Badge>
        <span className="text-xs text-[var(--text-tertiary)]">
          {settings.engineUrl || "engine"}
        </span>
      </div>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatTile label="Queue depth" value={queueDepth} hint="status endpoint not available yet" />
        <StatTile
          label="Last alert"
          value={lastAlertRel}
        />
        <StatTile
          label="Open alerts"
          value={stats ? String(stats.alerts24h ?? 0) : "-"}
        />
        <StatTile
          label="FPS / conf"
          value={`${settings.framesPerSecond.toFixed(1)} / ${settings.confidenceMin.toFixed(2)}`}
        />
      </div>

      {statsError && (
        <p className="text-xs text-[var(--text-tertiary)]" role="status">
          Could not load alert summary: {statsError}
        </p>
      )}

      <details
        open={advancedOpen}
        onToggle={(e) => setAdvancedOpen((e.target as HTMLDetailsElement).open)}
        className="rounded border border-[var(--border-primary)] bg-[var(--bg-tertiary)] px-3 py-2"
      >
        <summary className="cursor-pointer text-sm font-medium text-[var(--text-secondary)]">
          Advanced
        </summary>
        <div className="mt-3 space-y-2 text-sm text-[var(--text-primary)]">
          <dl className="grid grid-cols-2 gap-x-3 gap-y-1 text-xs">
            <dt className="text-[var(--text-tertiary)]">Region</dt>
            <dd className="font-mono">{settings.region}</dd>
            <dt className="text-[var(--text-tertiary)]">Frames per second</dt>
            <dd className="font-mono">{settings.framesPerSecond}</dd>
            <dt className="text-[var(--text-tertiary)]">Confidence min</dt>
            <dd className="font-mono">{settings.confidenceMin}</dd>
            <dt className="text-[var(--text-tertiary)]">Retention (unflagged)</dt>
            <dd className="font-mono">{settings.retentionDaysUnflagged}d</dd>
            <dt className="text-[var(--text-tertiary)]">Retention (flagged)</dt>
            <dd className="font-mono">{settings.retentionDaysFlagged}d</dd>
            <dt className="text-[var(--text-tertiary)]">Notify min severity</dt>
            <dd className="font-mono">{settings.notifyMinSeverity}</dd>
            <dt className="text-[var(--text-tertiary)]">Disclaimer acked</dt>
            <dd className="font-mono">
              {settings.disclaimerAckedAt
                ? `${settings.disclaimerAckedJurisdiction ?? "?"} @ ${settings.disclaimerAckedAt}`
                : "no"}
            </dd>
          </dl>
          <p className="text-xs text-[var(--text-tertiary)]">
            Editing these values inline is coming with the ALPR tuning
            UI. For now, adjust them via the API or environment variables.
          </p>
        </div>
      </details>

      <div className="flex justify-end border-t border-[var(--border-primary)] pt-3">
        <Button variant="ghost" onClick={onDisableClick}>
          Disable ALPR
        </Button>
      </div>
    </div>
  );
}

interface StatTileProps {
  label: string;
  value: string;
  hint?: string;
}

function StatTile({ label, value, hint }: StatTileProps) {
  return (
    <div className="rounded border border-[var(--border-primary)] bg-[var(--bg-tertiary)] px-3 py-2">
      <div className="text-xs font-medium uppercase tracking-wide text-[var(--text-tertiary)]">
        {label}
      </div>
      <div className="mt-1 truncate text-sm font-medium text-[var(--text-primary)]" title={value}>
        {value}
      </div>
      {hint && (
        <div className="mt-0.5 truncate text-[10px] text-[var(--text-tertiary)]">
          {hint}
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Main card
// ---------------------------------------------------------------------------

/**
 * AlprSettingsCard renders the Optional services -> ALPR onboarding flow.
 * It always issues a fresh GET /v1/settings/alpr (rather than going
 * through the cached useAlprSettings hook) so the card reflects the
 * current backend state even after a server restart, and so we can poll
 * engine reachability while STATE 3 is on screen without leaking stale
 * cache values to other dashboard widgets.
 */
export function AlprSettingsCard() {
  const [settings, setSettings] = useState<AlprSettings | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [showDisclaimer, setShowDisclaimer] = useState(false);
  const [showDisableConfirm, setShowDisableConfirm] = useState(false);
  const [stats, setStats] = useState<LiveStats | null>(null);
  const [statsError, setStatsError] = useState<string | null>(null);
  const [probeAttempts, setProbeAttempts] = useState(0);
  const [probing, setProbing] = useState(false);
  const [probeGiveUp, setProbeGiveUp] = useState(false);

  // Tracks whether the component is mounted; used to avoid setState on
  // unmounted instances when async fetches resolve late.
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const fetchSettings = useCallback(async (): Promise<AlprSettings | null> => {
    try {
      const wire = await apiFetch<AlprSettingsWire>("/v1/settings/alpr");
      const next = fromWire(wire);
      if (mountedRef.current) {
        setSettings(next);
        setError(null);
      }
      return next;
    } catch (e) {
      if (mountedRef.current) {
        setError(e instanceof Error ? e.message : "Failed to load ALPR settings");
      }
      return null;
    }
  }, []);

  const fetchStats = useCallback(async () => {
    try {
      const summary = await apiFetch<AlertsSummaryWire>(
        "/v1/alpr/alerts/summary",
      );
      if (mountedRef.current) {
        setStats({
          alerts24h: summary.open_count,
          lastAlertAt: summary.last_alert_at,
        });
        setStatsError(null);
      }
    } catch (e) {
      if (mountedRef.current) {
        setStatsError(e instanceof Error ? e.message : "stats unavailable");
      }
    }
  }, []);

  // Initial load.
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      setLoading(true);
      await fetchSettings();
      if (!cancelled && mountedRef.current) setLoading(false);
    })();
    return () => {
      cancelled = true;
    };
  }, [fetchSettings]);

  const cardState = settings ? deriveState(settings) : null;

  // STATE 3: probe engine reachability every PROBE_INTERVAL_MS, give up
  // after PROBE_MAX_ATTEMPTS so we do not hammer the engine forever.
  useEffect(() => {
    if (cardState !== "STATE_3_ENGINE_OFFLINE") {
      // Reset probe bookkeeping when leaving STATE 3 so a future return
      // to it starts a fresh attempt counter.
      if (probeAttempts !== 0 || probing || probeGiveUp) {
        setProbeAttempts(0);
        setProbing(false);
        setProbeGiveUp(false);
      }
      return undefined;
    }
    if (probeGiveUp) return undefined;

    let cancelled = false;
    const id = window.setInterval(() => {
      if (cancelled) return;
      void (async () => {
        setProbing(true);
        const next = await fetchSettings();
        setProbing(false);
        if (cancelled) return;
        setProbeAttempts((n) => {
          const incremented = n + 1;
          if (incremented >= PROBE_MAX_ATTEMPTS) setProbeGiveUp(true);
          return incremented;
        });
        if (next && next.enabled && next.engineReachable) {
          // The next render switches to STATE 4; the cleanup below
          // tears the interval down before another tick.
          setProbeAttempts(0);
          setProbeGiveUp(false);
        }
      })();
    }, PROBE_INTERVAL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(id);
    };
  }, [cardState, probeGiveUp, fetchSettings, probeAttempts, probing]);

  // STATE 4: load alerts summary once on entry. We do not poll because
  // the dashboard badge already polls this endpoint; the settings card
  // is informational.
  useEffect(() => {
    if (cardState !== "STATE_4_ENGINE_ONLINE") return;
    void fetchStats();
  }, [cardState, fetchStats]);

  function handleEnableClick() {
    if (!settings) return;
    if (!settings.encryptionKeyConfigured) {
      // Spec: do not let the user click into the modal at all when the
      // key is unconfigured -- the inline warning above the button is
      // the dead-end signal. This branch is defensive (the button is
      // disabled) but keeps the contract clear.
      return;
    }
    setShowDisclaimer(true);
  }

  async function handleConfirmed() {
    setShowDisclaimer(false);
    setShowDisableConfirm(false);
    setProbeAttempts(0);
    setProbeGiveUp(false);
    await fetchSettings();
  }

  return (
    <Card className="mt-4" data-testid="alpr-settings-card">
      <CardHeader>
        <div className="flex items-center justify-between gap-3">
          <div>
            <h2 className="text-subheading">Optional services</h2>
            <p className="text-caption">
              Off-by-default integrations that augment the dashboard.
            </p>
          </div>
        </div>
      </CardHeader>
      <CardBody className="space-y-4">
        <div
          className="rounded border border-[var(--border-primary)] bg-[var(--bg-surface)] p-4"
          data-testid="alpr-card"
        >
          <div className="mb-3 flex items-center justify-between gap-3">
            <h3 className="text-base font-semibold text-[var(--text-primary)]">
              License plate recognition (ALPR)
            </h3>
            {settings && (
              <Badge
                variant={
                  cardState === "STATE_4_ENGINE_ONLINE"
                    ? "success"
                    : cardState === "STATE_3_ENGINE_OFFLINE"
                      ? "warning"
                      : "neutral"
                }
                data-testid="alpr-card-state-pill"
              >
                {cardState === "STATE_1_DISABLED" && "Disabled"}
                {cardState === "STATE_3_ENGINE_OFFLINE" && "Enabled - engine offline"}
                {cardState === "STATE_4_ENGINE_ONLINE" && "Enabled - engine online"}
              </Badge>
            )}
          </div>

          {loading && (
            <div className="flex items-center justify-center py-6">
              <Spinner size="md" label="Loading ALPR status" />
            </div>
          )}

          {error && !loading && (
            <ErrorMessage
              title="Failed to load ALPR settings"
              message={error}
              retry={() => {
                setLoading(true);
                void fetchSettings().finally(() => {
                  if (mountedRef.current) setLoading(false);
                });
              }}
            />
          )}

          {settings && !loading && !error && cardState === "STATE_1_DISABLED" && (
            <State1Disabled
              encryptionKeyConfigured={settings.encryptionKeyConfigured}
              onEnableClick={handleEnableClick}
            />
          )}

          {settings &&
            !loading &&
            !error &&
            cardState === "STATE_3_ENGINE_OFFLINE" && (
              <State3EngineOffline
                attempts={probeAttempts}
                probing={probing}
                giveUp={probeGiveUp}
                onResume={() => {
                  setProbeAttempts(0);
                  setProbeGiveUp(false);
                }}
                onDisableClick={() => setShowDisableConfirm(true)}
              />
            )}

          {settings &&
            !loading &&
            !error &&
            cardState === "STATE_4_ENGINE_ONLINE" && (
              <State4EngineOnline
                settings={settings}
                stats={stats}
                statsError={statsError}
                onDisableClick={() => setShowDisableConfirm(true)}
              />
            )}
        </div>
      </CardBody>

      {showDisclaimer && settings && (
        <DisclaimerModal
          initialJurisdiction={
            (settings.disclaimerAckedJurisdiction as JurisdictionCode | null) ??
            "us"
          }
          encryptionKeyConfigured={settings.encryptionKeyConfigured}
          onClose={() => setShowDisclaimer(false)}
          onConfirmed={() => void handleConfirmed()}
        />
      )}

      {showDisableConfirm && (
        <DisableConfirmModal
          onClose={() => setShowDisableConfirm(false)}
          onConfirmed={() => void handleConfirmed()}
        />
      )}
    </Card>
  );
}
