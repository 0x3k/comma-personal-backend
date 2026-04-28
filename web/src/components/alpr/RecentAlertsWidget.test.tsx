import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { RecentAlertsWidget } from "./RecentAlertsWidget";
import type { AlertItem } from "./RecentAlertsWidget";

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

function jsonOk(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

function makeAlert(overrides: Partial<AlertItem> = {}): AlertItem {
  return {
    plate_hash_b64: overrides.plate_hash_b64 ?? "hash-default",
    plate: overrides.plate ?? "ABC123",
    signature: overrides.signature ?? null,
    severity: overrides.severity ?? null,
    kind: overrides.kind ?? "alerted",
    first_alert_at: overrides.first_alert_at ?? null,
    last_alert_at: overrides.last_alert_at ?? null,
    acked_at: overrides.acked_at ?? null,
    encounter_count: overrides.encounter_count ?? 1,
    latest_route: overrides.latest_route ?? null,
    evidence_summary: overrides.evidence_summary ?? "",
    notes: overrides.notes,
  };
}

describe("RecentAlertsWidget", () => {
  it("hides entirely when the alerts list is empty", async () => {
    fetchMock.mockResolvedValueOnce(jsonOk({ alerts: [] }));

    const { container } = render(<RecentAlertsWidget />);

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });
    // After resolution: container should be empty (the widget returns null).
    await waitFor(() => {
      expect(container.firstChild).toBeNull();
    });
  });

  it("renders one row per alert with severity, plate, and evidence", async () => {
    fetchMock.mockResolvedValueOnce(
      jsonOk({
        alerts: [
          makeAlert({
            plate_hash_b64: "hash-red",
            plate: "RED-1",
            severity: 5,
            evidence_summary: "Followed through 3 turns",
            signature: { make: "Toyota", model: "Camry", color: "white" },
          }),
          makeAlert({
            plate_hash_b64: "hash-amber",
            plate: "AMB-1",
            severity: 3,
            evidence_summary: "First alert",
          }),
        ],
      }),
    );

    render(<RecentAlertsWidget />);

    await waitFor(() => {
      expect(screen.getByTestId("alpr-recent-alerts-widget")).toBeTruthy();
    });

    const rows = screen.getAllByTestId("alpr-recent-alert-row");
    expect(rows).toHaveLength(2);

    const chips = screen.getAllByTestId("alpr-severity-chip");
    expect(chips[0].getAttribute("data-severity-bucket")).toBe("red");
    expect(chips[0].textContent).toContain("Sev 5");
    expect(chips[1].getAttribute("data-severity-bucket")).toBe("amber");
    expect(chips[1].textContent).toContain("Sev 3");

    expect(screen.getByText("RED-1")).toBeTruthy();
    expect(screen.getByText("AMB-1")).toBeTruthy();
    expect(screen.getByText("Toyota Camry")).toBeTruthy();
    expect(screen.getByText("Followed through 3 turns")).toBeTruthy();
  });

  it("links each row to the plate-detail route by hash", async () => {
    fetchMock.mockResolvedValueOnce(
      jsonOk({
        alerts: [
          makeAlert({
            plate_hash_b64: "hash-with/slash+plus",
            plate: "ENC",
            severity: 4,
          }),
        ],
      }),
    );

    render(<RecentAlertsWidget />);
    const row = await screen.findByTestId("alpr-recent-alert-row");
    // The hash gets URL-encoded so reserved characters round-trip safely.
    expect(row.getAttribute("href")).toBe(
      `/plates/${encodeURIComponent("hash-with/slash+plus")}`,
    );
  });
});
