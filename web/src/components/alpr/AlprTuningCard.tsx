"use client";

import { useCallback, useEffect, useId, useMemo, useState } from "react";
import { apiFetch } from "@/lib/api";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";
import { Button } from "@/components/ui/Button";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import {
  ALPR_TUNING_KNOBS,
  checkMonotonic,
  type AlprReevaluateResponse,
  type AlprTuningAuditEntry,
  type AlprTuningFieldErrors,
  type AlprTuningKnob,
  type AlprTuningRequest,
  type AlprTuningValidationErrorBody,
  type AlprTuningWire,
} from "./tuning-types";

// ---------------------------------------------------------------------------
// Wire payload shape (response of GET /v1/settings/alpr/tuning, etc.). The
// helpers translate to numeric form so the React state is plain `number`.
// ---------------------------------------------------------------------------

type EditableState = Omit<AlprTuningWire, "defaults">;

function pickEditable(w: AlprTuningWire): EditableState {
  const { defaults: _defaults, ...rest } = w;
  return rest;
}

function defaultsAsState(w: AlprTuningWire): EditableState {
  return {
    frame_rate: Number(w.defaults["alpr_frames_per_second"] ?? w.frame_rate),
    confidence_min: Number(
      w.defaults["alpr_confidence_min"] ?? w.confidence_min,
    ),
    encounter_gap_seconds: Number(
      w.defaults["alpr_encounter_gap_seconds"] ?? w.encounter_gap_seconds,
    ),
    alpr_heuristic_turns_min: Number(
      w.defaults["alpr_heuristic_turns_min"] ?? w.alpr_heuristic_turns_min,
    ),
    alpr_heuristic_persistence_minutes_min: Number(
      w.defaults["alpr_heuristic_persistence_minutes_min"] ??
        w.alpr_heuristic_persistence_minutes_min,
    ),
    alpr_heuristic_distinct_routes_min: Number(
      w.defaults["alpr_heuristic_distinct_routes_min"] ??
        w.alpr_heuristic_distinct_routes_min,
    ),
    alpr_heuristic_distinct_areas_min: Number(
      w.defaults["alpr_heuristic_distinct_areas_min"] ??
        w.alpr_heuristic_distinct_areas_min,
    ),
    alpr_heuristic_area_cell_km: Number(
      w.defaults["alpr_heuristic_area_cell_km"] ??
        w.alpr_heuristic_area_cell_km,
    ),
    severity_bucket_sev2: Number(
      w.defaults["alpr_heuristic_severity_bucket_sev2"] ??
        w.severity_bucket_sev2,
    ),
    severity_bucket_sev3: Number(
      w.defaults["alpr_heuristic_severity_bucket_sev3"] ??
        w.severity_bucket_sev3,
    ),
    severity_bucket_sev4: Number(
      w.defaults["alpr_heuristic_severity_bucket_sev4"] ??
        w.severity_bucket_sev4,
    ),
    severity_bucket_sev5: Number(
      w.defaults["alpr_heuristic_severity_bucket_sev5"] ??
        w.severity_bucket_sev5,
    ),
    notify_min_severity: Number(
      w.defaults["alpr_notify_min_severity"] ?? w.notify_min_severity,
    ),
  };
}

function diffAgainst(
  current: EditableState,
  base: EditableState,
): AlprTuningRequest {
  const out: AlprTuningRequest = {};
  for (const k of Object.keys(current) as (keyof EditableState)[]) {
    if (current[k] !== base[k]) {
      // The wire types include numeric values for every key; we cast
      // through unknown to keep TS happy with the dynamic indexing.
      (out as Record<string, number>)[k] = current[k];
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// Small subcomponents.
// ---------------------------------------------------------------------------

interface KnobRowProps {
  knob: AlprTuningKnob;
  value: number;
  defaultValue: number;
  fieldError?: string;
  onChange: (next: number) => void;
}

function KnobRow({
  knob,
  value,
  defaultValue,
  fieldError,
  onChange,
}: KnobRowProps) {
  const id = useId();
  const dirty = value !== defaultValue;
  return (
    <div className="grid grid-cols-1 gap-1 border-b border-[var(--border-primary)] py-3 last:border-b-0 sm:grid-cols-[180px_1fr_auto] sm:items-center sm:gap-3">
      <div>
        <label
          htmlFor={id}
          className="block text-sm font-medium text-[var(--text-primary)]"
          title={knob.tooltip}
        >
          {knob.label}
        </label>
        <p
          className="mt-0.5 hidden text-xs text-[var(--text-tertiary)] sm:block"
          title={knob.tooltip}
        >
          {knob.tooltip}
        </p>
      </div>
      <div className="flex items-center gap-3">
        <input
          id={id}
          type="range"
          min={knob.min}
          max={knob.max}
          step={knob.step}
          value={value}
          onChange={(e) => {
            const n = Number(e.target.value);
            onChange(knob.isInt ? Math.round(n) : n);
          }}
          className="w-full"
          aria-invalid={Boolean(fieldError)}
          data-testid={`tuning-knob-${knob.key}`}
        />
        <span className="w-20 shrink-0 text-right text-sm font-mono text-[var(--text-primary)]">
          {knob.isInt ? value : value.toFixed(2)}
          {knob.unit ? ` ${knob.unit}` : ""}
        </span>
      </div>
      <div className="flex items-center gap-2 text-xs text-[var(--text-tertiary)]">
        <span>default {knob.isInt ? defaultValue : defaultValue.toFixed(2)}</span>
        <button
          type="button"
          onClick={() => onChange(defaultValue)}
          disabled={!dirty}
          className="text-[var(--accent)] hover:underline disabled:opacity-40"
          data-testid={`tuning-reset-${knob.key}`}
        >
          Reset
        </button>
      </div>
      {fieldError && (
        <p
          className="col-span-full text-xs text-danger-600"
          role="alert"
          data-testid={`tuning-error-${knob.key}`}
        >
          {fieldError}
        </p>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Audit-log section.
// ---------------------------------------------------------------------------

interface AuditSectionProps {
  entries: AlprTuningAuditEntry[];
}

function AuditSection({ entries }: AuditSectionProps) {
  if (entries.length === 0) {
    return (
      <p className="text-xs text-[var(--text-tertiary)]">
        No tuning changes recorded yet.
      </p>
    );
  }
  return (
    <ul className="divide-y divide-[var(--border-primary)] text-xs">
      {entries.map((e) => {
        const changed = e.payload.changed_keys ?? {};
        const keys = Object.keys(changed);
        const summary = e.payload.reset_all
          ? `Reset all (${keys.length} keys)`
          : keys.length === 0
            ? "(no diff)"
            : keys
                .map(
                  (k) =>
                    `${k}: ${changed[k].from} -> ${changed[k].to}`,
                )
                .join(", ");
        return (
          <li
            key={e.id}
            className="grid grid-cols-1 gap-0.5 py-2 sm:grid-cols-[160px_140px_1fr]"
          >
            <span className="font-mono text-[var(--text-tertiary)]">
              {e.created_at}
            </span>
            <span className="text-[var(--text-secondary)]">
              {e.actor ?? "system"}
            </span>
            <span className="text-[var(--text-primary)]">{summary}</span>
          </li>
        );
      })}
    </ul>
  );
}

// ---------------------------------------------------------------------------
// Reset-all confirmation modal.
// ---------------------------------------------------------------------------

interface ResetAllModalProps {
  onCancel: () => void;
  onConfirm: () => void;
  submitting: boolean;
  error: string | null;
}

function ResetAllModal({
  onCancel,
  onConfirm,
  submitting,
  error,
}: ResetAllModalProps) {
  const headingId = useId();
  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby={headingId}
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      onClick={(e) => {
        if (e.target === e.currentTarget && !submitting) onCancel();
      }}
      data-testid="tuning-reset-all-modal"
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
            Reset every knob to default?
          </h2>
        </div>
        <div className="px-6 py-4 text-sm text-[var(--text-primary)]">
          <p>
            This restores every tuning value to its package default and
            writes one tuning_change audit row. Any calibration you have
            applied will be lost.
          </p>
          {error && (
            <p
              className="mt-3 text-sm text-danger-500"
              role="alert"
            >
              {error}
            </p>
          )}
        </div>
        <div className="flex items-center justify-end gap-2 border-t border-[var(--border-primary)] px-6 py-3">
          <Button variant="ghost" onClick={onCancel} disabled={submitting}>
            Cancel
          </Button>
          <Button
            variant="danger"
            onClick={onConfirm}
            disabled={submitting}
            data-testid="tuning-reset-all-confirm"
          >
            {submitting ? "Resetting..." : "Reset all"}
          </Button>
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Re-evaluate panel.
// ---------------------------------------------------------------------------

function severityCount(map: Record<string, number>, sev: number): number {
  return Number(map[String(sev)] ?? 0);
}

interface ReevaluatePanelProps {
  /** Optional callback so the parent can refresh dependent widgets
   *  after a live re-evaluation completes (e.g. the audit log). */
  onLiveCompleted?: () => void;
}

function ReevaluatePanel({ onLiveCompleted }: ReevaluatePanelProps) {
  const [daysBack, setDaysBack] = useState(30);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [dryRunResp, setDryRunResp] = useState<AlprReevaluateResponse | null>(
    null,
  );
  const [liveDoneAt, setLiveDoneAt] = useState<string | null>(null);

  const runDryRun = useCallback(async () => {
    setSubmitting(true);
    setError(null);
    try {
      const resp = await apiFetch<AlprReevaluateResponse>(
        "/v1/alpr/heuristic/reevaluate",
        { method: "POST", body: { days_back: daysBack, dry_run: true } },
      );
      setDryRunResp(resp);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Re-evaluation failed");
    } finally {
      setSubmitting(false);
    }
  }, [daysBack]);

  const applyLive = useCallback(async () => {
    setSubmitting(true);
    setError(null);
    try {
      await apiFetch<AlprReevaluateResponse>(
        "/v1/alpr/heuristic/reevaluate",
        { method: "POST", body: { days_back: daysBack, dry_run: false } },
      );
      setLiveDoneAt(new Date().toISOString());
      onLiveCompleted?.();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Re-evaluation failed");
    } finally {
      setSubmitting(false);
    }
  }, [daysBack, onLiveCompleted]);

  return (
    <div
      className="space-y-3 rounded border border-[var(--border-primary)] bg-[var(--bg-tertiary)] p-3"
      data-testid="tuning-reevaluate-panel"
    >
      <div className="flex items-center justify-between gap-3">
        <h3 className="text-sm font-semibold text-[var(--text-primary)]">
          Re-evaluate alerts against current tuning
        </h3>
      </div>
      <div className="flex items-center gap-3">
        <label className="text-xs text-[var(--text-secondary)]">
          Look back (days)
        </label>
        <input
          type="range"
          min={1}
          max={365}
          step={1}
          value={daysBack}
          onChange={(e) => setDaysBack(Number(e.target.value))}
          className="flex-1"
          data-testid="tuning-reevaluate-days"
        />
        <span className="w-12 text-right text-xs font-mono text-[var(--text-primary)]">
          {daysBack}d
        </span>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant="secondary"
          onClick={() => void runDryRun()}
          disabled={submitting}
          data-testid="tuning-reevaluate-dry-run"
        >
          {submitting ? "Running..." : "Dry-run"}
        </Button>
        <Button
          variant="primary"
          onClick={() => void applyLive()}
          disabled={submitting || !dryRunResp}
          data-testid="tuning-reevaluate-apply"
        >
          Apply
        </Button>
        {liveDoneAt && (
          <span className="text-xs text-[var(--text-tertiary)]">
            Last live re-run at {liveDoneAt}
          </span>
        )}
      </div>
      {error && (
        <p className="text-xs text-danger-600" role="alert">
          {error}
        </p>
      )}
      {dryRunResp?.summary && (
        <div
          className="rounded border border-[var(--border-primary)] bg-[var(--bg-surface)] p-3 text-sm"
          data-testid="tuning-reevaluate-summary"
        >
          <div className="mb-2 text-xs text-[var(--text-tertiary)]">
            {dryRunResp.jobs_enqueued} plate(s) scored over the last{" "}
            {dryRunResp.days_back} days
          </div>
          <table className="w-full text-xs">
            <thead>
              <tr className="text-left text-[var(--text-tertiary)]">
                <th className="pr-2">Severity</th>
                <th className="pr-2">Current</th>
                <th>After</th>
              </tr>
            </thead>
            <tbody className="font-mono">
              {[2, 3, 4, 5].map((sev) => (
                <tr key={sev}>
                  <td className="pr-2 text-[var(--text-secondary)]">
                    sev {sev}
                  </td>
                  <td className="pr-2">
                    {severityCount(dryRunResp.summary!.by_severity_before, sev)}
                  </td>
                  <td>
                    {severityCount(dryRunResp.summary!.by_severity_after, sev)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Main card.
// ---------------------------------------------------------------------------

/**
 * AlprTuningCard renders the tuning surface: every knob with a slider,
 * default-value chip, "Reset to default" link, and tooltip; a Re-
 * evaluate panel that previews the impact of the current tuning on
 * existing alerts; a recent-changes audit log section; and a
 * Reset-all destructive button at the bottom of the card.
 */
export function AlprTuningCard() {
  const [base, setBase] = useState<EditableState | null>(null);
  const [values, setValues] = useState<EditableState | null>(null);
  const [defaults, setDefaults] = useState<EditableState | null>(null);
  const [audit, setAudit] = useState<AlprTuningAuditEntry[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<AlprTuningFieldErrors>({});
  const [showResetAllModal, setShowResetAllModal] = useState(false);
  const [resetAllError, setResetAllError] = useState<string | null>(null);

  const fetchTuning = useCallback(async () => {
    const wire = await apiFetch<AlprTuningWire>("/v1/settings/alpr/tuning");
    setBase(pickEditable(wire));
    setValues(pickEditable(wire));
    setDefaults(defaultsAsState(wire));
  }, []);

  const fetchAudit = useCallback(async () => {
    try {
      const rows = await apiFetch<AlprTuningAuditEntry[]>(
        "/v1/settings/alpr/tuning/audit?limit=10",
      );
      setAudit(rows);
    } catch {
      // Audit fetch failures are non-fatal -- the rest of the card
      // remains usable.
      setAudit([]);
    }
  }, []);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      setLoading(true);
      setError(null);
      try {
        await fetchTuning();
        await fetchAudit();
      } catch (e) {
        if (!cancelled) {
          setError(e instanceof Error ? e.message : "Failed to load tuning");
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [fetchTuning, fetchAudit]);

  const dirty = useMemo(() => {
    if (!values || !base) return false;
    return Object.keys(diffAgainst(values, base)).length > 0;
  }, [values, base]);

  const monotonicError = useMemo(() => {
    if (!values) return null;
    return checkMonotonic(values);
  }, [values]);

  // Save flow.
  const onSave = useCallback(async () => {
    if (!values || !base) return;
    setSubmitting(true);
    setError(null);
    setFieldErrors({});
    try {
      const body = diffAgainst(values, base);
      if (Object.keys(body).length === 0) {
        setSubmitting(false);
        return;
      }
      await apiFetch<AlprTuningWire>("/v1/settings/alpr/tuning", {
        method: "PUT",
        body,
      });
      await fetchTuning();
      await fetchAudit();
    } catch (e) {
      // 422 from the backend carries field_errors; surface them per-
      // knob so the offending controls are flagged inline.
      if (e instanceof Error) {
        try {
          // The apiFetch wrapper throws an Error with the server
          // message, but loses the body. We refetch the body via a
          // direct fetch to capture field_errors when status is 422.
          // For simplicity here, we fall back to the message string.
          const parsed = JSON.parse(
            e.message,
          ) as AlprTuningValidationErrorBody;
          if (parsed.field_errors) {
            setFieldErrors(parsed.field_errors);
          } else {
            setError(e.message);
          }
        } catch {
          setError(e.message);
        }
      } else {
        setError("Save failed");
      }
    } finally {
      setSubmitting(false);
    }
  }, [values, base, fetchTuning, fetchAudit]);

  const onResetAll = useCallback(async () => {
    setSubmitting(true);
    setResetAllError(null);
    try {
      const resp = await apiFetch<AlprTuningWire>(
        "/v1/settings/alpr/tuning/reset",
        { method: "POST" },
      );
      const next = pickEditable(resp);
      setBase(next);
      setValues(next);
      setDefaults(defaultsAsState(resp));
      await fetchAudit();
      setShowResetAllModal(false);
    } catch (e) {
      setResetAllError(e instanceof Error ? e.message : "Reset failed");
    } finally {
      setSubmitting(false);
    }
  }, [fetchAudit]);

  if (loading) {
    return (
      <Card>
        <CardBody>
          <div className="flex items-center justify-center py-6">
            <Spinner label="Loading tuning" />
          </div>
        </CardBody>
      </Card>
    );
  }
  if (error || !values || !defaults) {
    return (
      <Card>
        <CardBody>
          <ErrorMessage
            title="Failed to load ALPR tuning"
            message={error ?? "unknown error"}
            retry={() => {
              setLoading(true);
              setError(null);
              void fetchTuning().finally(() => setLoading(false));
            }}
          />
        </CardBody>
      </Card>
    );
  }

  return (
    <Card data-testid="alpr-tuning-card">
      <CardHeader>
        <h2 className="text-subheading">Tuning</h2>
        <p className="text-caption">
          Adjust thresholds at runtime. Changes take effect immediately for
          new evaluations; use Re-evaluate below to see the impact on
          existing alerts.
        </p>
      </CardHeader>
      <CardBody className="space-y-5">
        <div>
          {ALPR_TUNING_KNOBS.map((knob) => (
            <KnobRow
              key={knob.key}
              knob={knob}
              value={values[knob.key]}
              defaultValue={defaults[knob.key]}
              fieldError={
                fieldErrors[knob.key] ??
                (monotonicError === knob.key
                  ? "must be greater than or equal to the prior severity bucket"
                  : undefined)
              }
              onChange={(next) =>
                setValues((prev) =>
                  prev ? { ...prev, [knob.key]: next } : prev,
                )
              }
            />
          ))}
        </div>

        <div className="flex flex-wrap items-center gap-2 border-t border-[var(--border-primary)] pt-3">
          <Button
            variant="primary"
            onClick={() => void onSave()}
            disabled={
              submitting || !dirty || Boolean(monotonicError)
            }
            data-testid="tuning-save"
          >
            {submitting ? "Saving..." : "Save changes"}
          </Button>
          <Button
            variant="ghost"
            onClick={() => {
              if (base) setValues(base);
              setFieldErrors({});
            }}
            disabled={submitting || !dirty}
          >
            Discard
          </Button>
          <div className="flex-1" />
          <Button
            variant="danger"
            onClick={() => {
              setResetAllError(null);
              setShowResetAllModal(true);
            }}
            data-testid="tuning-reset-all"
          >
            Reset all to defaults
          </Button>
        </div>

        <div>
          <h3 className="mb-2 text-sm font-semibold text-[var(--text-primary)]">
            Re-evaluate
          </h3>
          <ReevaluatePanel onLiveCompleted={() => void fetchAudit()} />
        </div>

        <div>
          <h3 className="mb-2 text-sm font-semibold text-[var(--text-primary)]">
            Recent tuning changes
          </h3>
          <AuditSection entries={audit ?? []} />
        </div>
      </CardBody>

      {showResetAllModal && (
        <ResetAllModal
          onCancel={() => setShowResetAllModal(false)}
          onConfirm={() => void onResetAll()}
          submitting={submitting}
          error={resetAllError}
        />
      )}
    </Card>
  );
}
