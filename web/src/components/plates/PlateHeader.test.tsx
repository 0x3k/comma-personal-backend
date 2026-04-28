import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { PlateHeader, derivePillState } from "./PlateHeader";
import type { PlateWatchlistStatus } from "@/lib/plateDetail";

afterEach(() => {
  cleanup();
});

const NOOP = () => {};

const ACTIONS = {
  onAck: NOOP,
  onUnack: NOOP,
  onAddWhitelist: NOOP,
  onRemoveWhitelist: NOOP,
  onEdit: NOOP,
  onMerge: NOOP,
};

function renderHeader(watchlistStatus: PlateWatchlistStatus | null) {
  return render(
    <PlateHeader
      plate="ABC123"
      plateHashB64="abcdefABCDEF"
      signature={null}
      watchlistStatus={watchlistStatus}
      actions={ACTIONS}
    />,
  );
}

describe("derivePillState", () => {
  it("collapses null to none", () => {
    expect(derivePillState(null)).toEqual({ kind: "none" });
  });

  it("returns whitelist for kind=whitelist", () => {
    expect(
      derivePillState({ kind: "whitelist", severity: null, acked_at: null }),
    ).toEqual({ kind: "whitelist" });
  });

  it("returns acked when acked_at is set", () => {
    expect(
      derivePillState({
        kind: "alerted",
        severity: 4,
        acked_at: "2026-04-25T12:00:00Z",
      }),
    ).toEqual({ kind: "acked" });
  });

  it("returns open with severity for unacked alerted rows", () => {
    expect(
      derivePillState({ kind: "alerted", severity: 5, acked_at: null }),
    ).toEqual({ kind: "open", severity: 5 });
  });
});

describe("PlateHeader: status pill", () => {
  it("renders 'No alerts' when watchlist is null", () => {
    renderHeader(null);
    const pill = screen.getByTestId("plate-header-status");
    expect(pill.getAttribute("data-status")).toBe("none");
    expect(pill.textContent).toContain("No alerts");
  });

  it("renders 'Open alert sev N' for an open alerted row", () => {
    renderHeader({ kind: "alerted", severity: 4, acked_at: null });
    const pill = screen.getByTestId("plate-header-status");
    expect(pill.getAttribute("data-status")).toBe("open");
    expect(pill.getAttribute("data-severity-bucket")).toBe("red");
    expect(pill.textContent).toContain("Open alert sev 4");
  });

  it("uses amber bucket for severity 2-3", () => {
    renderHeader({ kind: "alerted", severity: 2, acked_at: null });
    const pill = screen.getByTestId("plate-header-status");
    expect(pill.getAttribute("data-severity-bucket")).toBe("amber");
    expect(pill.textContent).toContain("Open alert sev 2");
  });

  it("renders 'Acknowledged' for acked alerted rows", () => {
    renderHeader({
      kind: "alerted",
      severity: 4,
      acked_at: "2026-04-26T00:00:00Z",
    });
    const pill = screen.getByTestId("plate-header-status");
    expect(pill.getAttribute("data-status")).toBe("acked");
    expect(pill.textContent).toContain("Acknowledged");
  });

  it("renders 'Whitelisted' for whitelist rows", () => {
    renderHeader({ kind: "whitelist", severity: null, acked_at: null });
    const pill = screen.getByTestId("plate-header-status");
    expect(pill.getAttribute("data-status")).toBe("whitelist");
    expect(pill.textContent).toContain("Whitelisted");
  });
});

describe("PlateHeader: action buttons", () => {
  it("shows Ack button only when alert is open", () => {
    renderHeader({ kind: "alerted", severity: 3, acked_at: null });
    expect(screen.getByTestId("plate-action-ack")).toBeInTheDocument();
    expect(screen.queryByTestId("plate-action-unack")).toBeNull();
  });

  it("shows Unack button only when alert is acked", () => {
    renderHeader({
      kind: "alerted",
      severity: 3,
      acked_at: "2026-04-26T00:00:00Z",
    });
    expect(screen.getByTestId("plate-action-unack")).toBeInTheDocument();
    expect(screen.queryByTestId("plate-action-ack")).toBeNull();
  });

  it("shows Remove-whitelist button on whitelist rows", () => {
    renderHeader({ kind: "whitelist", severity: null, acked_at: null });
    expect(
      screen.getByTestId("plate-action-remove-whitelist"),
    ).toBeInTheDocument();
    expect(screen.queryByTestId("plate-action-add-whitelist")).toBeNull();
  });

  it("shows Add-whitelist on non-whitelist rows", () => {
    renderHeader(null);
    expect(screen.getByTestId("plate-action-add-whitelist")).toBeInTheDocument();
    expect(screen.queryByTestId("plate-action-remove-whitelist")).toBeNull();
  });

  it("disables every action while busy", () => {
    render(
      <PlateHeader
        plate="ABC"
        plateHashB64="h"
        signature={null}
        watchlistStatus={{
          kind: "alerted",
          severity: 4,
          acked_at: null,
        }}
        actions={ACTIONS}
        busy
      />,
    );
    expect(
      screen.getByTestId("plate-action-ack").hasAttribute("disabled"),
    ).toBe(true);
    expect(
      screen.getByTestId("plate-action-edit").hasAttribute("disabled"),
    ).toBe(true);
    expect(
      screen.getByTestId("plate-action-merge").hasAttribute("disabled"),
    ).toBe(true);
  });

  it("invokes onAck when Acknowledge is clicked", () => {
    const onAck = vi.fn();
    render(
      <PlateHeader
        plate="X"
        plateHashB64="h"
        signature={null}
        watchlistStatus={{ kind: "alerted", severity: 3, acked_at: null }}
        actions={{ ...ACTIONS, onAck }}
      />,
    );
    screen.getByTestId("plate-action-ack").click();
    expect(onAck).toHaveBeenCalledTimes(1);
  });
});

describe("PlateHeader: vehicle badge", () => {
  it("renders the badge when signature carries make/model", () => {
    render(
      <PlateHeader
        plate="ABC"
        plateHashB64="h"
        signature={{ make: "Honda", model: "Civic", color: "blue" }}
        watchlistStatus={null}
        actions={ACTIONS}
      />,
    );
    const badge = screen.getByTestId("plate-header-vehicle");
    expect(badge.textContent).toContain("Honda Civic");
  });

  it("hides the badge when signature is null", () => {
    renderHeader(null);
    expect(screen.queryByTestId("plate-header-vehicle")).toBeNull();
  });
});
