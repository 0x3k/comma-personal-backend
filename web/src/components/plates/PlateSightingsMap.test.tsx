import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { PlateSightingsMap } from "./PlateSightingsMap";

/**
 * The default export of PlateSightingsMapInner is loaded via
 * next/dynamic with ssr:false. In a jsdom test that import path
 * still resolves Leaflet, which touches `window` at module load and
 * is fine here, but we don't want to actually render the map -- the
 * point of these tests is the wrapper's branch logic. We stub the
 * inner component out entirely.
 */
vi.mock("./PlateSightingsMapInner", () => ({
  default: ({ points }: { points: { key: string }[] }) => (
    <div data-testid="map-inner-stub" data-count={String(points.length)} />
  ),
}));

afterEach(() => {
  cleanup();
});

describe("PlateSightingsMap", () => {
  it("renders the empty placeholder when zero points", () => {
    render(<PlateSightingsMap points={[]} />);
    expect(screen.getByTestId("plate-map-empty")).toBeInTheDocument();
    expect(screen.queryByTestId("plate-map")).toBeNull();
  });

  it("renders the inner map when at least one point exists", () => {
    render(
      <PlateSightingsMap
        points={[
          {
            key: "d|r",
            lat: 40,
            lng: -73,
            severity: "amber",
            label: "test",
          },
        ]}
      />,
    );
    expect(screen.getByTestId("plate-map")).toBeInTheDocument();
    expect(screen.queryByTestId("plate-map-empty")).toBeNull();
  });
});
