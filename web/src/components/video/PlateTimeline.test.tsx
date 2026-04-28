import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { PlateTimeline } from "./PlateTimeline";
import type { PlateEncounter } from "./PlateTimeline";
import {
  __setAlprSettingsCacheForTests,
  type AlprSettings,
} from "@/lib/useAlprSettings";

/**
 * Build a fully-populated AlprSettings shape with the given enabled
 * value. Other fields are filler so the hook's consumers see a
 * realistic object without each test having to spell every key.
 */
function makeAlprSettings(enabled: boolean): AlprSettings {
  return {
    enabled,
    engineUrl: "http://localhost:8000",
    region: "us",
    framesPerSecond: 1.0,
    confidenceMin: 0.7,
    retentionDaysUnflagged: 30,
    retentionDaysFlagged: 365,
    notifyMinSeverity: 3,
    encryptionKeyConfigured: true,
    engineReachable: true,
    disclaimerRequired: false,
    disclaimerVersion: "2026-04-v1",
    disclaimerAckedAt: "2026-04-01T00:00:00Z",
    disclaimerAckedJurisdiction: "us",
  };
}

/**
 * Build a single encounter with reasonable defaults. The route in
 * the tests starts at 2026-04-25T12:00:00Z and runs for 600 seconds,
 * so the encounter at firstSecOffset/lastSecOffset will land on the
 * shared time axis.
 */
function makeEncounter(
  overrides: Partial<PlateEncounter> & { firstSecOffset?: number; lastSecOffset?: number } = {},
): PlateEncounter {
  const t0 = Date.parse("2026-04-25T12:00:00Z");
  const firstOffset = overrides.firstSecOffset ?? 60;
  const lastOffset = overrides.lastSecOffset ?? 75;
  return {
    plate: overrides.plate ?? "ABC123",
    plate_hash_b64: overrides.plate_hash_b64 ?? "aaa",
    first_seen_ts: new Date(t0 + firstOffset * 1000).toISOString(),
    last_seen_ts: new Date(t0 + lastOffset * 1000).toISOString(),
    detection_count: overrides.detection_count ?? 5,
    turn_count: overrides.turn_count ?? 0,
    signature: overrides.signature ?? null,
    severity_if_alerted: overrides.severity_if_alerted ?? null,
    ack_status: overrides.ack_status ?? null,
    bbox_first: overrides.bbox_first ?? null,
    sample_thumb_url: overrides.sample_thumb_url ?? null,
  };
}

const ROUTE_START = "2026-04-25T12:00:00Z";
const ROUTE_DURATION_SEC = 600;

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  __setAlprSettingsCacheForTests(null);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  __setAlprSettingsCacheForTests(null);
  cleanup();
});

function jsonOk(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

describe("PlateTimeline", () => {
  it("renders nothing when alpr is disabled (feature-flag-off)", async () => {
    __setAlprSettingsCacheForTests(makeAlprSettings(false));

    const { container } = render(
      <PlateTimeline
        dongleId="dongle-1"
        routeName="2026-04-25--12-00-00"
        routeStartTs={ROUTE_START}
        routeDurationSec={ROUTE_DURATION_SEC}
        onSeek={vi.fn()}
      />,
    );

    // The flag is cached as `false`, so the component returns null
    // synchronously on the first render. There is no rail in the DOM,
    // and crucially we never hit the /plates endpoint.
    expect(container.firstChild).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("renders nothing for the empty state (zero encounters)", async () => {
    __setAlprSettingsCacheForTests(makeAlprSettings(true));
    fetchMock.mockResolvedValueOnce(jsonOk({ encounters: [] }));

    const { container } = render(
      <PlateTimeline
        dongleId="dongle-1"
        routeName="2026-04-25--12-00-00"
        routeStartTs={ROUTE_START}
        routeDurationSec={ROUTE_DURATION_SEC}
        onSeek={vi.fn()}
      />,
    );

    // Wait for the fetch to resolve. Until it does, the component
    // returns null because `encounters` is still null. After the
    // resolve, the empty-state branch keeps it null. Either way, no
    // children should ever appear.
    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });
    expect(container.firstChild).toBeNull();
    expect(screen.queryByTestId("plate-timeline")).toBeNull();
  });

  it("seeks to first_seen_ts when an encounter segment is clicked", async () => {
    __setAlprSettingsCacheForTests(makeAlprSettings(true));
    const onSeek = vi.fn();

    // One alerted (red) encounter at 60-75s, one unalerted at 200-210s.
    // The first encounter starts at offset 60 from route start so a
    // click should yield onSeek(60).
    fetchMock.mockResolvedValueOnce(
      jsonOk({
        encounters: [
          makeEncounter({
            plate: "RED-1",
            plate_hash_b64: "hash-red",
            firstSecOffset: 60,
            lastSecOffset: 75,
            severity_if_alerted: 4,
            ack_status: "open",
            signature: { make: "Toyota", model: "Camry", color: "white" },
          }),
          makeEncounter({
            plate: "GRY-1",
            plate_hash_b64: "hash-gray",
            firstSecOffset: 200,
            lastSecOffset: 210,
          }),
        ],
      }),
    );

    render(
      <PlateTimeline
        dongleId="dongle-1"
        routeName="2026-04-25--12-00-00"
        routeStartTs={ROUTE_START}
        routeDurationSec={ROUTE_DURATION_SEC}
        onSeek={onSeek}
      />,
    );

    // Wait until the rail appears. testid is stable across the two
    // encounters so we can target the first one specifically.
    const firstSegment = await screen.findByTestId("plate-segment-0");
    expect(firstSegment.getAttribute("data-severity")).toBe("red");

    fireEvent.click(firstSegment);
    expect(onSeek).toHaveBeenCalledTimes(1);
    // The first encounter's first_seen_ts is exactly 60s after the
    // route start, so onSeek should fire with 60. Allowing a small
    // tolerance keeps the test resilient to floating-point drift.
    const seekArg = onSeek.mock.calls[0][0];
    expect(seekArg).toBeCloseTo(60, 6);
  });
});
