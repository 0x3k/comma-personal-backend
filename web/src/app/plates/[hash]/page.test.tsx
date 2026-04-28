import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import PlateDetailPage from "./page";
import {
  __setAlprSettingsCacheForTests,
  type AlprSettings,
} from "@/lib/useAlprSettings";

/**
 * The Leaflet inner map component reaches into `window` at import
 * time -- the page wraps it via next/dynamic with ssr: false, but
 * jsdom still tries to module-resolve it when the page renders. We
 * replace it with a stub so tests focus on the page-level behaviour
 * without standing up a real map.
 */
vi.mock("@/components/plates/PlateSightingsMap", () => ({
  PlateSightingsMap: ({
    points,
  }: {
    points: { key: string; lat: number; lng: number }[];
  }) => (
    <div data-testid="plate-map-stub" data-count={String(points.length)} />
  ),
}));

const { routerState } = vi.hoisted(() => {
  return { routerState: { pushed: [] as string[] } };
});

vi.mock("next/navigation", () => {
  return {
    useRouter: () => ({
      push: (href: string) => {
        routerState.pushed.push(href);
      },
      replace: (href: string) => {
        routerState.pushed.push(href);
      },
    }),
    useSearchParams: () => new URLSearchParams(""),
    usePathname: () => "/plates/test",
  };
});

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  routerState.pushed.length = 0;
  __setAlprSettingsCacheForTests(makeAlprSettings(true));
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  __setAlprSettingsCacheForTests(null);
  cleanup();
});

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

function jsonOk(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

function jsonError(status: number, message: string): Response {
  return new Response(JSON.stringify({ error: message, code: status }), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function makeDetail(overrides: Partial<PlateDetailWire> = {}): PlateDetailWire {
  return {
    plate: "ABC123",
    plate_hash_b64: "TESTHASH",
    watchlist_status: null,
    signature: null,
    encounters: [
      {
        dongle_id: "d1",
        route: "2026-04-25--12-00-00",
        first_seen_ts: "2026-04-25T12:00:00Z",
        last_seen_ts: "2026-04-25T12:05:00Z",
        detection_count: 10,
        turn_count: 3,
        signature: null,
        area_cluster_label: "Brooklyn",
      },
    ],
    stats: {
      distinct_routes_30d: 5,
      distinct_areas_30d: 3,
      total_detections: 100,
      first_ever_seen: "2026-04-01T00:00:00Z",
      last_ever_seen: "2026-04-25T12:05:00Z",
    },
    ...overrides,
  };
}

interface PlateDetailWire {
  plate: string;
  plate_hash_b64: string;
  watchlist_status: {
    kind: string;
    severity: number | null;
    label?: string;
    acked_at: string | null;
  } | null;
  signature: {
    make?: string;
    model?: string;
    color?: string;
    body_type?: string;
    confidence?: number;
  } | null;
  encounters: {
    dongle_id: string;
    route: string;
    first_seen_ts: string;
    last_seen_ts: string;
    detection_count: number;
    turn_count: number;
    signature: PlateDetailWire["signature"];
    area_cluster_label: string;
  }[];
  stats: {
    distinct_routes_30d: number;
    distinct_areas_30d: number;
    total_detections: number;
    first_ever_seen?: string;
    last_ever_seen?: string;
  };
}

/**
 * Set up the typical "plate page load" mock chain. The trip endpoint
 * is fired per encounter; we accept any /v1/routes/.../trip URL with
 * an empty trip so the map renders with zero plottable points but
 * the rest of the page paints normally.
 */
function mockPlateLoad(detail: PlateDetailWire) {
  fetchMock.mockImplementation(async (url: string) => {
    if (url.includes("/v1/plates/")) return jsonOk(detail);
    if (url.includes("/trip")) {
      return jsonOk({
        start_lat: null,
        start_lng: null,
        start_address: null,
        start_time: null,
        duration_seconds: null,
      });
    }
    return jsonOk({});
  });
}

/**
 * Cache the params promise so re-renders see the same object. React's
 * `use(promise)` short-circuits when it has already resolved a given
 * promise; creating a fresh Promise per render would wedge the
 * Suspense boundary forever in jsdom because each render queues a
 * new pending value.
 */
const RESOLVED_PARAMS = Promise.resolve({ hash: "TESTHASH" });
function pageProps() {
  return { params: RESOLVED_PARAMS };
}

describe("PlateDetailPage: feature flag", () => {
  it("renders the 'feature disabled' shell when ALPR is off", async () => {
    __setAlprSettingsCacheForTests(makeAlprSettings(false));
    render(<PlateDetailPage {...pageProps()} />);

    // The shell may take a microtask to mount because the page
    // suspends on `use(params)` before the disabled-flag branch
    // runs. waitFor polls until the inner content appears.
    await waitFor(
      () => {
        expect(screen.getByTestId("plate-feature-disabled")).toBeInTheDocument();
      },
      { timeout: 4000 },
    );
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe("PlateDetailPage: 404", () => {
  it("renders 'plate not found' on a 404 from the API", async () => {
    fetchMock.mockResolvedValueOnce(jsonError(404, "no encounters"));

    render(<PlateDetailPage {...pageProps()} />);

    expect(
      await screen.findByTestId("plate-not-found"),
    ).toBeInTheDocument();
    expect(screen.getByText(/back to alerts/i)).toBeInTheDocument();
  });
});

describe("PlateDetailPage: happy path", () => {
  it("renders header, stats, encounter list, and evidence", async () => {
    mockPlateLoad(
      makeDetail({
        watchlist_status: {
          kind: "alerted",
          severity: 4,
          acked_at: null,
        },
      }),
    );

    render(<PlateDetailPage {...pageProps()} />);

    // Header
    expect(
      await screen.findByTestId("plate-header-text"),
    ).toHaveTextContent("ABC123");
    expect(screen.getByTestId("plate-header-status").textContent).toContain(
      "Open alert sev 4",
    );

    // Stats
    expect(screen.getByTestId("plate-stats-routes-30d").textContent).toBe("5");
    expect(screen.getByTestId("plate-stats-total-detections").textContent).toBe(
      "100",
    );

    // Encounters
    expect(
      screen.getByTestId("plate-encounter-row-d1-2026-04-25--12-00-00"),
    ).toBeInTheDocument();

    // Map stub received the (zero-point) data
    expect(screen.getByTestId("plate-map-stub")).toBeInTheDocument();

    // Evidence accordion present
    expect(screen.getByTestId("plate-evidence-accordion")).toBeInTheDocument();
  });
});

describe("PlateDetailPage: cluster mode (map)", () => {
  it("supplies plottable points to the map when trips have GPS", async () => {
    fetchMock.mockImplementation(async (url: string) => {
      if (url.includes("/v1/plates/")) {
        return jsonOk(
          makeDetail({
            encounters: [
              ...makeDetail().encounters,
              {
                dongle_id: "d1",
                route: "2026-04-26--13-00-00",
                first_seen_ts: "2026-04-26T13:00:00Z",
                last_seen_ts: "2026-04-26T13:05:00Z",
                detection_count: 5,
                turn_count: 2,
                signature: null,
                area_cluster_label: "Queens",
              },
            ],
          }),
        );
      }
      if (url.includes("/trip")) {
        // Two distinct GPS points so the map count is 2.
        if (url.includes("2026-04-25--12-00-00")) {
          return jsonOk({
            start_lat: 40.6782,
            start_lng: -73.9442,
            start_address: "Brooklyn",
            start_time: null,
            duration_seconds: null,
          });
        }
        return jsonOk({
          start_lat: 40.7282,
          start_lng: -73.7949,
          start_address: "Queens",
          start_time: null,
          duration_seconds: null,
        });
      }
      return jsonOk({});
    });

    render(<PlateDetailPage {...pageProps()} />);

    await waitFor(() => {
      const stub = screen.getByTestId("plate-map-stub");
      expect(stub.getAttribute("data-count")).toBe("2");
    });
  });
});

describe("PlateDetailPage: edit -> hint -> merge flow", () => {
  it("exposes a merge action when the edit response carries match_hash_b64", async () => {
    fetchMock.mockImplementation(async (url: string, init?: RequestInit) => {
      // Initial detail load
      if (
        url.endsWith("/v1/plates/TESTHASH") ||
        url.includes("/v1/plates/TESTHASH?")
      ) {
        return jsonOk(makeDetail());
      }
      // Trip lookups: empty
      if (url.includes("/trip")) {
        return jsonOk({
          start_lat: null,
          start_lng: null,
          start_address: null,
          start_time: null,
          duration_seconds: null,
        });
      }
      // Per-route encounter lookup (for findRepresentativeDetectionId)
      if (url.includes("/plates") && url.includes("/v1/routes/")) {
        return jsonOk({
          encounters: [
            {
              plate_hash_b64: "TESTHASH",
              sample_thumb_url: "/v1/alpr/detections/777/thumbnail",
            },
          ],
        });
      }
      // Edit detection PATCH
      if (
        url.includes("/v1/alpr/detections/") &&
        init?.method === "PATCH"
      ) {
        return jsonOk({
          accepted: true,
          affected_routes: 1,
          hint: "This plate now matches another in your history -- merge?",
          match_hash_b64: "MERGEDESTHASH",
        });
      }
      return jsonOk({});
    });

    render(<PlateDetailPage {...pageProps()} />);

    // Wait for header to render so the edit button is interactive.
    await screen.findByTestId("plate-action-edit");

    fireEvent.click(screen.getByTestId("plate-action-edit"));

    // The modal opens once the detection id is resolved.
    await waitFor(() => {
      expect(screen.getByTestId("edit-plate-modal")).toBeInTheDocument();
    });

    fireEvent.change(screen.getByTestId("edit-plate-input"), {
      target: { value: "NEW999" },
    });
    fireEvent.click(screen.getByTestId("edit-plate-submit"));

    // The page surfaces a toast with a merge action.
    await waitFor(() => {
      expect(screen.getByTestId("plate-toast-action")).toBeInTheDocument();
    });

    fireEvent.click(screen.getByTestId("plate-toast-action"));

    // The merge modal is now open and prefilled with MERGEDESTHASH.
    await waitFor(() => {
      const input = screen.getByTestId(
        "merge-plate-input",
      ) as HTMLInputElement;
      expect(input.value).toBe("MERGEDESTHASH");
    });
  });
});

describe("PlateDetailPage: merge happy path", () => {
  it("redirects to the destination plate after a successful merge", async () => {
    fetchMock.mockImplementation(async (url: string, init?: RequestInit) => {
      if (
        url.endsWith("/v1/plates/TESTHASH") ||
        url.includes("/v1/plates/TESTHASH?")
      ) {
        return jsonOk(makeDetail());
      }
      if (url.includes("/trip")) {
        return jsonOk({
          start_lat: null,
          start_lng: null,
          start_address: null,
          start_time: null,
          duration_seconds: null,
        });
      }
      if (url.includes("/v1/alpr/plates/merge") && init?.method === "POST") {
        return jsonOk({ accepted: true, affected_routes: 4 });
      }
      return jsonOk({});
    });

    render(<PlateDetailPage {...pageProps()} />);

    await screen.findByTestId("plate-action-merge");
    fireEvent.click(screen.getByTestId("plate-action-merge"));

    await waitFor(() => {
      expect(screen.getByTestId("merge-plate-modal")).toBeInTheDocument();
    });

    fireEvent.change(screen.getByTestId("merge-plate-input"), {
      target: { value: "OTHERHASH" },
    });
    fireEvent.click(screen.getByTestId("merge-plate-submit"));

    await waitFor(() => {
      expect(routerState.pushed).toContain("/plates/OTHERHASH");
    });
  });
});
