/**
 * Wire types and per-knob metadata for the ALPR tuning surface.
 *
 * The shapes mirror the backend's alprTuningResponse / alprTuningRequest
 * (internal/api/settings_alpr_tuning.go). Keeping them in their own
 * module keeps the heavy AlprTuningCard component lean and lets the
 * tests import the metadata catalogue without pulling in the React
 * tree.
 */

export type AlprTuningWire = {
  frame_rate: number;
  confidence_min: number;
  encounter_gap_seconds: number;
  alpr_heuristic_turns_min: number;
  alpr_heuristic_persistence_minutes_min: number;
  alpr_heuristic_distinct_routes_min: number;
  alpr_heuristic_distinct_areas_min: number;
  alpr_heuristic_area_cell_km: number;
  severity_bucket_sev2: number;
  severity_bucket_sev3: number;
  severity_bucket_sev4: number;
  severity_bucket_sev5: number;
  notify_min_severity: number;
  defaults: Record<string, number>;
};

export type AlprTuningRequest = Partial<Omit<AlprTuningWire, "defaults">>;

export type AlprTuningFieldErrors = Record<string, string>;

export interface AlprTuningValidationErrorBody {
  error: string;
  code: number;
  field_errors?: AlprTuningFieldErrors;
}

/**
 * AlprTuningKnob is the metadata for a single editable control.
 * `tooltip` is sourced from docs/ALPR.md so the in-product hint and
 * the long-form documentation stay aligned.
 */
export interface AlprTuningKnob {
  /** Wire JSON key. Persists in {field_errors}, the audit-log diff,
   *  and the request body. */
  key: keyof AlprTuningRequest;
  /** Settings-table key. Used for the Reset-to-default per-knob link
   *  to read the catalogue default off the GET response. */
  settingsKey: string;
  /** Human label rendered next to the slider. */
  label: string;
  /** Tooltip shown on hover/focus; one or two sentences sourced from
   *  docs/ALPR.md. */
  tooltip: string;
  /** Slider min / max bounds. The dashboard mirrors the backend
   *  validation so out-of-range values cannot be submitted in the
   *  first place; the backend still re-validates. */
  min: number;
  max: number;
  /** Step size for the slider. */
  step: number;
  /** Unit suffix appended to the displayed value. Empty for unitless
   *  controls (turns_min). */
  unit: string;
  /** Whether the knob is an integer-typed slider; true for counts
   *  (turns_min, distinct_routes_min, ...) so we coerce in submit. */
  isInt?: boolean;
}

export const ALPR_TUNING_KNOBS: AlprTuningKnob[] = [
  {
    key: "frame_rate",
    settingsKey: "alpr_frames_per_second",
    label: "Frame rate",
    tooltip:
      "Frame-extractor sampling rate (frames per second of dashcam video sent to OCR). Higher values catch more plates but cost more engine time.",
    min: 0.5,
    max: 4,
    step: 0.1,
    unit: "fps",
  },
  {
    key: "confidence_min",
    settingsKey: "alpr_confidence_min",
    label: "Minimum OCR confidence",
    tooltip:
      "Lowest OCR confidence score the pipeline accepts. Lower values capture more plates at the cost of noisier reads. Going below 0.5 is not recommended.",
    min: 0.5,
    max: 0.95,
    step: 0.01,
    unit: "",
  },
  {
    key: "encounter_gap_seconds",
    settingsKey: "alpr_encounter_gap_seconds",
    label: "Encounter gap",
    tooltip:
      "Maximum gap between consecutive detections of the same plate within one encounter. A gap longer than this starts a new encounter.",
    min: 15,
    max: 300,
    step: 5,
    unit: "s",
  },
  {
    key: "alpr_heuristic_turns_min",
    settingsKey: "alpr_heuristic_turns_min",
    label: "Turns min",
    tooltip:
      "Per-encounter turn count above which within_route_turns starts to score. Higher values suppress alerts on chaotic urban routes.",
    min: 1,
    max: 8,
    step: 1,
    unit: "",
    isInt: true,
  },
  {
    key: "alpr_heuristic_persistence_minutes_min",
    settingsKey: "alpr_heuristic_persistence_minutes_min",
    label: "Persistence min",
    tooltip:
      "Minimum encounter duration (minutes) above which the persistence component awards points.",
    min: 3,
    max: 30,
    step: 1,
    unit: "min",
  },
  {
    key: "alpr_heuristic_distinct_routes_min",
    settingsKey: "alpr_heuristic_distinct_routes_min",
    label: "Distinct routes min",
    tooltip:
      "Minimum distinct routes a plate must appear on before cross_route_count fires. Higher values reduce false positives from common neighbors.",
    min: 2,
    max: 10,
    step: 1,
    unit: "",
    isInt: true,
  },
  {
    key: "alpr_heuristic_distinct_areas_min",
    settingsKey: "alpr_heuristic_distinct_areas_min",
    label: "Distinct areas min",
    tooltip:
      "Minimum distinct geo cells a plate must appear in for cross_route_geo_spread to fire. >= 2 cells means the plate appears in two parts of your life.",
    min: 1,
    max: 5,
    step: 1,
    unit: "",
    isInt: true,
  },
  {
    key: "alpr_heuristic_area_cell_km",
    settingsKey: "alpr_heuristic_area_cell_km",
    label: "Area cell size",
    tooltip:
      "Side length of the geo grid cell used for cross_route_geo_spread. ~5 km is roughly 'different neighborhood' without merging home and work.",
    min: 1,
    max: 20,
    step: 1,
    unit: "km",
  },
  {
    key: "severity_bucket_sev2",
    settingsKey: "alpr_heuristic_severity_bucket_sev2",
    label: "Severity 2 lower edge",
    tooltip:
      "Score at or above which an alert promotes to severity 2. Lower edges must be monotonically increasing across severities 2 -> 5.",
    min: 0,
    max: 100,
    step: 0.1,
    unit: "",
  },
  {
    key: "severity_bucket_sev3",
    settingsKey: "alpr_heuristic_severity_bucket_sev3",
    label: "Severity 3 lower edge",
    tooltip:
      "Score at or above which an alert promotes to severity 3. Must be >= severity 2.",
    min: 0,
    max: 100,
    step: 0.1,
    unit: "",
  },
  {
    key: "severity_bucket_sev4",
    settingsKey: "alpr_heuristic_severity_bucket_sev4",
    label: "Severity 4 lower edge",
    tooltip:
      "Score at or above which an alert promotes to severity 4. Must be >= severity 3.",
    min: 0,
    max: 100,
    step: 0.1,
    unit: "",
  },
  {
    key: "severity_bucket_sev5",
    settingsKey: "alpr_heuristic_severity_bucket_sev5",
    label: "Severity 5 lower edge",
    tooltip:
      "Score at or above which an alert promotes to severity 5. Must be >= severity 4.",
    min: 0,
    max: 100,
    step: 0.1,
    unit: "",
  },
  {
    key: "notify_min_severity",
    settingsKey: "alpr_notify_min_severity",
    label: "Notify min severity",
    tooltip:
      "Lowest severity that triggers an outbound notification (email or webhook). Severity 1 is reserved for manual notes.",
    min: 2,
    max: 5,
    step: 1,
    unit: "",
    isInt: true,
  },
];

/**
 * checkMonotonic returns the offending field key if the proposed
 * tuning's severity-bucket lower edges are not monotonic, or null
 * when the invariant holds. Mirrors the backend validation so the
 * dashboard can light up a warning before the operator clicks Save.
 */
export function checkMonotonic(
  values: Pick<
    AlprTuningWire,
    | "severity_bucket_sev2"
    | "severity_bucket_sev3"
    | "severity_bucket_sev4"
    | "severity_bucket_sev5"
  >,
): string | null {
  if (values.severity_bucket_sev2 > values.severity_bucket_sev3) {
    return "severity_bucket_sev3";
  }
  if (values.severity_bucket_sev3 > values.severity_bucket_sev4) {
    return "severity_bucket_sev4";
  }
  if (values.severity_bucket_sev4 > values.severity_bucket_sev5) {
    return "severity_bucket_sev5";
  }
  return null;
}

/**
 * AlprTuningAuditEntry is one row from
 * GET /v1/settings/alpr/tuning/audit.
 */
export interface AlprTuningAuditEntry {
  id: number;
  action: string;
  actor: string | null;
  payload: {
    changed_keys?: Record<string, { from: number; to: number }>;
    reset_all?: boolean;
  };
  created_at: string;
}

export type AlprReevaluateSummary = {
  by_severity_before: Record<string, number>;
  by_severity_after: Record<string, number>;
  whitelisted: number;
};

export interface AlprReevaluateResponse {
  accepted: boolean;
  eta_seconds: number;
  jobs_enqueued: number;
  dry_run: boolean;
  days_back: number;
  summary?: AlprReevaluateSummary;
}
