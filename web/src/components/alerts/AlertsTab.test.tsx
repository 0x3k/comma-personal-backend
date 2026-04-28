import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { AlertsTab } from "./AlertsTab";
import { EMPTY_ALERTS_FILTERS } from "./filters";
import type { AlertItem } from "./api";

/**
 * Convenience response builder. Centralised because every test feeds
 * the listAlerts wrapper a {alerts: [...]} envelope, and forgetting
 * the wrapping makes the tests fail with confusing messages.
 */
function alertsResponse(items: AlertItem[]): Response {
  return new Response(JSON.stringify({ alerts: items }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
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

function makeAlert(overrides: Partial<AlertItem> = {}): AlertItem {
  // Use the spread merge pattern instead of `?? default` so an
  // explicitly-passed null/0/empty-string overrides the default.
  // Specifically, severity=null means "no severity bucket" and we
  // want that to make it through to the component.
  const base: AlertItem = {
    plate_hash_b64: "h1",
    plate: "ABC123",
    signature: null,
    severity: 4,
    kind: "alerted",
    first_alert_at: "2026-04-25T12:00:00Z",
    last_alert_at: "2026-04-25T12:00:00Z",
    acked_at: null,
    encounter_count: 3,
    latest_route: null,
    evidence_summary: "Seen on 5 trips in 2 areas over 9 days.",
  };
  return { ...base, ...overrides };
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

describe("AlertsTab: empty state", () => {
  it("renders the empty title + description when zero rows", async () => {
    fetchMock.mockResolvedValueOnce(alertsResponse([]));

    render(
      <AlertsTab
        mode="open"
        filters={EMPTY_ALERTS_FILTERS}
        emptyTitle="No open alerts"
        emptyDescription="Why this might be empty."
      />,
    );

    await waitFor(() => {
      expect(screen.getByTestId("alerts-empty")).toBeInTheDocument();
    });
    expect(screen.getByText("No open alerts")).toBeInTheDocument();
    expect(screen.getByText(/why this might be empty/i)).toBeInTheDocument();
  });
});

describe("AlertsTab: bulk-ack happy path", () => {
  it("acks every selected row in parallel and refetches", async () => {
    // Initial load: two alerts.
    fetchMock.mockResolvedValueOnce(
      alertsResponse([
        makeAlert({ plate_hash_b64: "h1", plate: "AAA111" }),
        makeAlert({ plate_hash_b64: "h2", plate: "BBB222" }),
      ]),
    );
    // Two ack calls: both succeed.
    fetchMock.mockResolvedValueOnce(jsonOk({ acked_at: "2026-04-25T13:00:00Z" }));
    fetchMock.mockResolvedValueOnce(jsonOk({ acked_at: "2026-04-25T13:00:00Z" }));
    // Refetch after success: empty list (rows dropped out of "open").
    fetchMock.mockResolvedValueOnce(alertsResponse([]));

    render(
      <AlertsTab
        mode="open"
        filters={EMPTY_ALERTS_FILTERS}
        emptyTitle="No open alerts"
      />,
    );

    // Wait for initial fetch + render.
    await waitFor(() => {
      expect(screen.getByTestId("alerts-row-h1")).toBeInTheDocument();
    });
    expect(screen.getByTestId("alerts-row-h2")).toBeInTheDocument();

    // Select both rows by ticking each row checkbox.
    fireEvent.click(screen.getByTestId("alerts-row-select-h1"));
    fireEvent.click(screen.getByTestId("alerts-row-select-h2"));

    // The bulk bar should now show "2 selected" and the CTA enabled.
    expect(screen.getByTestId("alerts-bulk-bar")).toHaveTextContent("2 selected");
    const bulkBtn = screen.getByTestId("alerts-bulk-ack");
    expect(bulkBtn).not.toBeDisabled();

    fireEvent.click(bulkBtn);

    // Wait for the refetch to clear the list down to the empty state.
    await waitFor(() => {
      expect(screen.getByTestId("alerts-empty")).toBeInTheDocument();
    });

    // Two POSTs to /v1/alpr/alerts/<hash>/ack went out, plus the
    // initial GET and the refetch GET = 4 total.
    expect(fetchMock).toHaveBeenCalledTimes(4);

    // Inspect the calls -- POSTs use the bare hash without
    // URL-encoding (base64-url chars are URL-safe).
    const calls = fetchMock.mock.calls.map((c) => c[0] as string);
    expect(calls.some((u) => u.includes("/v1/alpr/alerts/h1/ack"))).toBe(true);
    expect(calls.some((u) => u.includes("/v1/alpr/alerts/h2/ack"))).toBe(true);
  });
});

describe("AlertsTab: bulk-ack partial failure", () => {
  it("marks the failed row red and keeps the successful one acked", async () => {
    fetchMock.mockResolvedValueOnce(
      alertsResponse([
        makeAlert({ plate_hash_b64: "h1", plate: "AAA111" }),
        makeAlert({ plate_hash_b64: "h2", plate: "BBB222" }),
      ]),
    );
    // h1 succeeds, h2 fails.
    fetchMock.mockResolvedValueOnce(jsonOk({ acked_at: "t" }));
    fetchMock.mockResolvedValueOnce(jsonError(500, "boom"));
    // After partial: refetch. The successful row drops out; the
    // failed row remains (still in open).
    fetchMock.mockResolvedValueOnce(
      alertsResponse([makeAlert({ plate_hash_b64: "h2", plate: "BBB222" })]),
    );

    render(
      <AlertsTab
        mode="open"
        filters={EMPTY_ALERTS_FILTERS}
        emptyTitle="No open alerts"
      />,
    );

    await waitFor(() => {
      expect(screen.getByTestId("alerts-row-h1")).toBeInTheDocument();
    });

    fireEvent.click(screen.getByTestId("alerts-row-select-h1"));
    fireEvent.click(screen.getByTestId("alerts-row-select-h2"));
    fireEvent.click(screen.getByTestId("alerts-bulk-ack"));

    // After the refetch, h1 should be gone (acked, dropped from open),
    // and h2 should still be in the list.
    await waitFor(() => {
      expect(screen.queryByTestId("alerts-row-h1")).toBeNull();
    });
    expect(screen.getByTestId("alerts-row-h2")).toBeInTheDocument();

    // Toast surfaces the partial result.
    expect(
      screen.getByText(/Acknowledged 1, 1 failed/i),
    ).toBeInTheDocument();
  });
});

describe("AlertsTab: severity badge color buckets", () => {
  it("classifies severity 4 as red and severity 2 as amber", async () => {
    fetchMock.mockResolvedValueOnce(
      alertsResponse([
        makeAlert({ plate_hash_b64: "h-red", severity: 4 }),
        makeAlert({ plate_hash_b64: "h-amber", severity: 2 }),
        makeAlert({ plate_hash_b64: "h-none", severity: null }),
      ]),
    );

    render(
      <AlertsTab
        mode="open"
        filters={EMPTY_ALERTS_FILTERS}
        emptyTitle="No open alerts"
      />,
    );

    await waitFor(() => {
      expect(screen.getByTestId("alerts-row-h-red")).toBeInTheDocument();
    });

    const redRow = screen.getByTestId("alerts-row-h-red");
    const amberRow = screen.getByTestId("alerts-row-h-amber");
    const noneRow = screen.getByTestId("alerts-row-h-none");

    expect(
      within(redRow).getByTestId("alerts-row-severity").getAttribute(
        "data-severity-bucket",
      ),
    ).toBe("red");
    expect(
      within(amberRow).getByTestId("alerts-row-severity").getAttribute(
        "data-severity-bucket",
      ),
    ).toBe("amber");
    expect(
      within(noneRow).getByTestId("alerts-row-severity").getAttribute(
        "data-severity-bucket",
      ),
    ).toBe("none");
  });
});
