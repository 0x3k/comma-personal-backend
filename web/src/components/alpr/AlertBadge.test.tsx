import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { AlertBadge } from "./AlertBadge";
import type { AlertSummary } from "@/lib/useAlertSummary";

afterEach(() => {
  cleanup();
});

function summary(overrides: Partial<AlertSummary> = {}): AlertSummary {
  return {
    open_count: overrides.open_count ?? 0,
    max_open_severity: overrides.max_open_severity ?? null,
    last_alert_at: overrides.last_alert_at ?? null,
  };
}

describe("AlertBadge", () => {
  it("renders nothing when summary is null", () => {
    const { container } = render(<AlertBadge summary={null} />);
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing when open_count is 0", () => {
    const { container } = render(
      <AlertBadge
        summary={summary({ open_count: 0, max_open_severity: null })}
      />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("renders an amber badge for severity 2-3", () => {
    render(
      <AlertBadge
        summary={summary({ open_count: 2, max_open_severity: 3 })}
      />,
    );
    const badge = screen.getByTestId("alpr-alert-badge");
    expect(badge.getAttribute("data-severity-bucket")).toBe("amber");
    expect(badge.textContent).toContain("2 open alerts");
  });

  it("renders a red badge for severity 4-5", () => {
    render(
      <AlertBadge
        summary={summary({ open_count: 7, max_open_severity: 5 })}
      />,
    );
    const badge = screen.getByTestId("alpr-alert-badge");
    expect(badge.getAttribute("data-severity-bucket")).toBe("red");
    expect(badge.textContent).toContain("7 open alerts");
  });

  it("uses singular language for a count of 1", () => {
    render(
      <AlertBadge
        summary={summary({ open_count: 1, max_open_severity: 2 })}
      />,
    );
    const badge = screen.getByTestId("alpr-alert-badge");
    expect(badge.textContent).toContain("1 open alert");
    expect(badge.textContent).not.toContain("alerts");
  });

  it("links to the /alerts page", () => {
    render(
      <AlertBadge
        summary={summary({ open_count: 1, max_open_severity: 4 })}
      />,
    );
    const badge = screen.getByTestId("alpr-alert-badge");
    expect(badge.getAttribute("href")).toBe("/alerts");
  });
});
