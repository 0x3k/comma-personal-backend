import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import {
  buildEvidenceBullets,
  PlateEvidenceAccordion,
} from "./PlateEvidenceAccordion";
import type { PlateDetailEncounter } from "@/lib/plateDetail";

afterEach(() => {
  cleanup();
});

const ENCOUNTER: PlateDetailEncounter = {
  dongle_id: "d1",
  route: "2026-04-25--12-00-00",
  first_seen_ts: "2026-04-25T12:00:00Z",
  last_seen_ts: "2026-04-25T12:05:00Z",
  detection_count: 10,
  turn_count: 4,
  signature: null,
  area_cluster_label: "Brooklyn",
};

describe("buildEvidenceBullets", () => {
  it("emits no bullets for empty stats and no encounters", () => {
    expect(
      buildEvidenceBullets(
        {
          distinct_routes_30d: 0,
          distinct_areas_30d: 0,
          total_detections: 0,
        },
        [],
      ),
    ).toEqual([]);
  });

  it("emits the routes/30d bullet when nonzero", () => {
    const bullets = buildEvidenceBullets(
      {
        distinct_routes_30d: 5,
        distinct_areas_30d: 5,
        total_detections: 50,
      },
      [],
    );
    expect(bullets[0]).toContain("5 distinct routes");
    expect(bullets[0]).toContain("30 days");
  });

  it("does not duplicate the routes bullet as an areas bullet when equal", () => {
    const bullets = buildEvidenceBullets(
      {
        distinct_routes_30d: 5,
        distinct_areas_30d: 5,
        total_detections: 50,
      },
      [],
    );
    expect(bullets.filter((b) => b.includes("distinct"))).toHaveLength(1);
  });

  it("emits the turn-count bullet when an encounter has multiple turns", () => {
    const bullets = buildEvidenceBullets(
      {
        distinct_routes_30d: 1,
        distinct_areas_30d: 1,
        total_detections: 10,
      },
      [ENCOUNTER],
    );
    expect(bullets.some((b) => b.includes("4 turns"))).toBe(true);
  });

  it("skips the turn-count bullet for sub-2-turn encounters", () => {
    const enc = { ...ENCOUNTER, turn_count: 1 };
    const bullets = buildEvidenceBullets(
      {
        distinct_routes_30d: 0,
        distinct_areas_30d: 0,
        total_detections: 5,
      },
      [enc],
    );
    expect(bullets.some((b) => b.includes("turn"))).toBe(false);
  });
});

describe("PlateEvidenceAccordion", () => {
  it("starts collapsed; clicking the toggle reveals the body", () => {
    render(
      <PlateEvidenceAccordion
        stats={{
          distinct_routes_30d: 3,
          distinct_areas_30d: 2,
          total_detections: 30,
        }}
        encounters={[ENCOUNTER]}
      />,
    );
    expect(screen.queryByTestId("plate-evidence-body")).toBeNull();

    fireEvent.click(screen.getByTestId("plate-evidence-toggle"));

    const body = screen.getByTestId("plate-evidence-body");
    expect(body).toBeInTheDocument();
    expect(body.textContent).toContain("3 distinct routes");
  });
});
