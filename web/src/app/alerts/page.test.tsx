import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import AlertsPage from "./page";
import {
  __setAlprSettingsCacheForTests,
  type AlprSettings,
} from "@/lib/useAlprSettings";

/**
 * Mock the next/navigation router + searchParams. vi.hoisted is the
 * canonical way to share mutable state between the hoisted vi.mock
 * factory and the surrounding test body -- a plain `let` would be
 * undefined at the time the factory runs.
 *
 * The mock reads from urlState.value, which the tests mutate to
 * simulate URL changes (deep-link or back-button), and writes to it
 * via the replaceMock so the page's router.replace() calls round-trip
 * through the URL state.
 */
/**
 * Module-shared state between the hoisted next/navigation mock and
 * the test bodies. vi.hoisted gives the mock factory access to this
 * record before the test file's own statements run; without it, a
 * plain `let` would still be `undefined` when the factory executes.
 *
 * urlState.value carries the simulated URL; calls record records the
 * (href) arguments the page passes to router.replace so tests can
 * assert URL writes happened.
 */
const { urlState, calls } = vi.hoisted(() => {
  return { urlState: { value: "" }, calls: [] as string[] };
});

vi.mock("next/navigation", () => {
  const replace = (href: string) => {
    calls.push(href);
    const idx = href.indexOf("?");
    urlState.value = idx >= 0 ? href.slice(idx + 1) : "";
  };
  return {
    useRouter: () => ({ replace, push: replace }),
    useSearchParams: () => new URLSearchParams(urlState.value),
    usePathname: () => "/alerts",
  };
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

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  urlState.value = "";
  calls.length = 0;
  __setAlprSettingsCacheForTests(makeAlprSettings(true));
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

/**
 * Set up the typical "page load" mock chain: devices list (empty so
 * the dongle selector hides) + alerts list. Helper extracted so each
 * test reads as the scenario, not the boilerplate.
 */
function mockBasicLoad() {
  fetchMock.mockImplementation(async (url: string) => {
    if (url.endsWith("/v1/devices")) return jsonOk([]);
    if (url.includes("/v1/alpr/alerts")) return jsonOk({ alerts: [] });
    if (url.includes("/v1/alpr/whitelist")) return jsonOk({ whitelist: [] });
    return jsonOk({});
  });
}

describe("AlertsPage: feature flag", () => {
  it("renders a disabled-state card when alpr is off", async () => {
    __setAlprSettingsCacheForTests(makeAlprSettings(false));
    mockBasicLoad();

    render(<AlertsPage />);

    expect(await screen.findByText(/ALPR is disabled/i)).toBeInTheDocument();
    // No tabs were rendered.
    expect(screen.queryByTestId("alerts-tab-open")).toBeNull();
  });
});

describe("AlertsPage: tabs + URL sync", () => {
  it("defaults to the Open tab and writes tab=acked when switched", async () => {
    mockBasicLoad();

    render(<AlertsPage />);

    // Wait for the tab bar to appear.
    await waitFor(() => {
      expect(screen.getByTestId("alerts-tab-open")).toBeInTheDocument();
    });

    // Open is the active tab on first render.
    expect(
      screen.getByTestId("alerts-tab-open").getAttribute("aria-selected"),
    ).toBe("true");

    // Switch to Acknowledged.
    fireEvent.click(screen.getByTestId("alerts-tab-acked"));

    await waitFor(() => {
      expect(
        screen.getByTestId("alerts-tab-acked").getAttribute("aria-selected"),
      ).toBe("true");
    });

    // The router was asked to replace with ?tab=acked. We don't assert
    // the exact call count because the page also runs a one-shot
    // initial-sync replace; we just look for at least one call carrying
    // tab=acked.
    expect(calls.some((u) => u.includes("tab=acked"))).toBe(true);
  });

  it("hydrates filters + tab from the URL on mount (deep-link)", async () => {
    urlState.value ="tab=acked&severity=4,5";
    fetchMock.mockImplementation(async (url: string) => {
      if (url.endsWith("/v1/devices")) return jsonOk([]);
      if (url.includes("/v1/alpr/alerts")) {
        // Acked tab + severity filter survives. The mock just returns
        // empty, but we record the URL for assertion below.
        return jsonOk({ alerts: [] });
      }
      return jsonOk({});
    });

    render(<AlertsPage />);

    await waitFor(() => {
      expect(
        screen.getByTestId("alerts-tab-acked").getAttribute("aria-selected"),
      ).toBe("true");
    });

    // The list fetch should have used status=acked and severity=4,5.
    const alertCalls = fetchMock.mock.calls
      .map((c) => c[0] as string)
      .filter((u) => u.includes("/v1/alpr/alerts"));
    expect(alertCalls.length).toBeGreaterThan(0);
    const last = alertCalls[alertCalls.length - 1];
    expect(last).toContain("status=acked");
    expect(last).toContain("severity=4%2C5");

    // Severity chips reflect the URL state -- chip 4 + chip 5 are
    // active, others are not.
    expect(
      screen
        .getByTestId("alerts-filter-severity-4")
        .getAttribute("data-active"),
    ).toBe("true");
    expect(
      screen
        .getByTestId("alerts-filter-severity-2")
        .getAttribute("data-active"),
    ).toBe("false");
  });

  it("re-hydrates state when the URL changes externally (back-button)", async () => {
    mockBasicLoad();

    const { rerender } = render(<AlertsPage />);

    await waitFor(() => {
      expect(
        screen.getByTestId("alerts-tab-open").getAttribute("aria-selected"),
      ).toBe("true");
    });

    // Simulate a browser back-button by changing the URL backing the
    // mocked useSearchParams and re-rendering. Next.js triggers a
    // re-render when the URL changes; the test stands in for that.
    urlState.value ="tab=acked";
    rerender(<AlertsPage />);

    await waitFor(() => {
      expect(
        screen.getByTestId("alerts-tab-acked").getAttribute("aria-selected"),
      ).toBe("true");
    });
  });

  it("renders the whitelist tab without firing /v1/alpr/alerts", async () => {
    urlState.value ="tab=whitelist";
    mockBasicLoad();

    render(<AlertsPage />);

    await waitFor(() => {
      expect(screen.getByTestId("whitelist-add-form")).toBeInTheDocument();
    });

    // No alert listing call -- the whitelist tab does not need it.
    const alertCalls = fetchMock.mock.calls
      .map((c) => c[0] as string)
      .filter((u) => u.includes("/v1/alpr/alerts"));
    expect(alertCalls.length).toBe(0);
  });
});
