import { describe, expect, it } from "vitest";
import {
  ALPR_TUNING_KNOBS,
  checkMonotonic,
  type AlprTuningWire,
} from "./tuning-types";

describe("checkMonotonic", () => {
  it("returns null when buckets ascend", () => {
    const v: Pick<
      AlprTuningWire,
      | "severity_bucket_sev2"
      | "severity_bucket_sev3"
      | "severity_bucket_sev4"
      | "severity_bucket_sev5"
    > = {
      severity_bucket_sev2: 2,
      severity_bucket_sev3: 4,
      severity_bucket_sev4: 6,
      severity_bucket_sev5: 8,
    };
    expect(checkMonotonic(v)).toBeNull();
  });

  it("flags sev3 when sev3 < sev2", () => {
    expect(
      checkMonotonic({
        severity_bucket_sev2: 5,
        severity_bucket_sev3: 4,
        severity_bucket_sev4: 6,
        severity_bucket_sev5: 8,
      }),
    ).toBe("severity_bucket_sev3");
  });

  it("flags the first violator only", () => {
    expect(
      checkMonotonic({
        severity_bucket_sev2: 2,
        severity_bucket_sev3: 4,
        severity_bucket_sev4: 3, // breaks sev4 >= sev3
        severity_bucket_sev5: 1, // also broken, but sev4 is reported first
      }),
    ).toBe("severity_bucket_sev4");
  });

  it("allows equal values (monotonically non-decreasing)", () => {
    expect(
      checkMonotonic({
        severity_bucket_sev2: 2,
        severity_bucket_sev3: 2,
        severity_bucket_sev4: 2,
        severity_bucket_sev5: 2,
      }),
    ).toBeNull();
  });
});

describe("ALPR_TUNING_KNOBS", () => {
  it("includes the spec'd knobs", () => {
    const keys = ALPR_TUNING_KNOBS.map((k) => k.key);
    expect(keys).toContain("frame_rate");
    expect(keys).toContain("confidence_min");
    expect(keys).toContain("encounter_gap_seconds");
    expect(keys).toContain("alpr_heuristic_turns_min");
    expect(keys).toContain("alpr_heuristic_persistence_minutes_min");
    expect(keys).toContain("alpr_heuristic_distinct_routes_min");
    expect(keys).toContain("alpr_heuristic_distinct_areas_min");
    expect(keys).toContain("alpr_heuristic_area_cell_km");
    expect(keys).toContain("severity_bucket_sev2");
    expect(keys).toContain("severity_bucket_sev3");
    expect(keys).toContain("severity_bucket_sev4");
    expect(keys).toContain("severity_bucket_sev5");
    expect(keys).toContain("notify_min_severity");
  });

  it("has consistent min/max ranges", () => {
    for (const k of ALPR_TUNING_KNOBS) {
      expect(k.min).toBeLessThan(k.max);
      expect(k.tooltip.length).toBeGreaterThan(10);
    }
  });
});
