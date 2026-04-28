import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { AlprTuningCard } from "./AlprTuningCard";
import type {
  AlprTuningAuditEntry,
  AlprTuningWire,
} from "./tuning-types";

function makeTuning(
  overrides: Partial<AlprTuningWire> = {},
): AlprTuningWire {
  return {
    frame_rate: 2.0,
    confidence_min: 0.75,
    encounter_gap_seconds: 60,
    alpr_heuristic_turns_min: 2,
    alpr_heuristic_persistence_minutes_min: 8,
    alpr_heuristic_distinct_routes_min: 3,
    alpr_heuristic_distinct_areas_min: 2,
    alpr_heuristic_area_cell_km: 5,
    severity_bucket_sev2: 2,
    severity_bucket_sev3: 4,
    severity_bucket_sev4: 6,
    severity_bucket_sev5: 8,
    notify_min_severity: 4,
    defaults: {
      alpr_frames_per_second: 2.0,
      alpr_confidence_min: 0.75,
      alpr_encounter_gap_seconds: 60,
      alpr_heuristic_turns_min: 2,
      alpr_heuristic_persistence_minutes_min: 8,
      alpr_heuristic_distinct_routes_min: 3,
      alpr_heuristic_distinct_areas_min: 2,
      alpr_heuristic_area_cell_km: 5,
      alpr_heuristic_severity_bucket_sev2: 2,
      alpr_heuristic_severity_bucket_sev3: 4,
      alpr_heuristic_severity_bucket_sev4: 6,
      alpr_heuristic_severity_bucket_sev5: 8,
      alpr_notify_min_severity: 4,
    },
    ...overrides,
  };
}

function jsonOk(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  cleanup();
});

async function renderCard(initial: AlprTuningWire, audit: AlprTuningAuditEntry[] = []) {
  fetchMock.mockImplementation(async (url: string, init?: RequestInit) => {
    if (typeof url === "string" && url.endsWith("/v1/settings/alpr/tuning") && (!init || init.method === undefined || init.method === "GET")) {
      return jsonOk(initial);
    }
    if (typeof url === "string" && url.includes("/v1/settings/alpr/tuning/audit")) {
      return jsonOk(audit);
    }
    if (typeof url === "string" && url.endsWith("/v1/settings/alpr/tuning") && init?.method === "PUT") {
      return jsonOk(initial);
    }
    if (typeof url === "string" && url.endsWith("/v1/settings/alpr/tuning/reset")) {
      return jsonOk(initial);
    }
    return jsonOk({});
  });
  const utils = render(<AlprTuningCard />);
  await waitFor(() => {
    expect(screen.getByTestId("alpr-tuning-card")).toBeInTheDocument();
  });
  return utils;
}

describe("AlprTuningCard", () => {
  it("renders every knob with current value and default", async () => {
    await renderCard(makeTuning());
    // Distinct turns_min slider rendered with current value 2.
    const turns = screen.getByTestId("tuning-knob-alpr_heuristic_turns_min") as HTMLInputElement;
    expect(turns.value).toBe("2");
    // Severity bucket sev3 slider also rendered.
    expect(screen.getByTestId("tuning-knob-severity_bucket_sev3")).toBeInTheDocument();
  });

  it("save button is disabled when nothing changed", async () => {
    await renderCard(makeTuning());
    const save = screen.getByTestId("tuning-save") as HTMLButtonElement;
    expect(save.disabled).toBe(true);
  });

  it("flags monotonic violation inline without contacting the API", async () => {
    await renderCard(makeTuning());
    const sev3 = screen.getByTestId("tuning-knob-severity_bucket_sev3") as HTMLInputElement;
    // Drop sev3 below sev2 (default 2). Slider step is 0.1, range [0,100].
    fireEvent.change(sev3, { target: { value: "1" } });
    expect(
      screen.getByTestId("tuning-error-severity_bucket_sev3"),
    ).toBeInTheDocument();
    const save = screen.getByTestId("tuning-save") as HTMLButtonElement;
    expect(save.disabled).toBe(true);
  });

  it("opens the reset-all confirmation modal", async () => {
    await renderCard(makeTuning());
    fireEvent.click(screen.getByTestId("tuning-reset-all"));
    expect(screen.getByTestId("tuning-reset-all-modal")).toBeInTheDocument();
    expect(screen.getByTestId("tuning-reset-all-confirm")).toBeInTheDocument();
  });

  it("renders the re-evaluate panel with default 30-day window", async () => {
    await renderCard(makeTuning());
    const slider = screen.getByTestId("tuning-reevaluate-days") as HTMLInputElement;
    expect(slider.value).toBe("30");
    expect(screen.getByTestId("tuning-reevaluate-dry-run")).toBeInTheDocument();
  });

  it("populates the audit section when entries are returned", async () => {
    await renderCard(makeTuning(), [
      {
        id: 1,
        action: "tuning_change",
        actor: "user:1",
        payload: {
          changed_keys: { alpr_heuristic_turns_min: { from: 2, to: 3 } },
        },
        created_at: "2026-04-27T12:00:00Z",
      },
    ]);
    await waitFor(() => {
      expect(screen.getByText(/alpr_heuristic_turns_min/)).toBeInTheDocument();
    });
  });
});
