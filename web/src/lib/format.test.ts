import { describe, expect, it } from "vitest";
import { formatDuration, formatDurationBetween } from "./format";

describe("formatDuration", () => {
  it("renders sub-minute values in seconds", () => {
    expect(formatDuration(0)).toBe("0s");
    expect(formatDuration(1)).toBe("1s");
    expect(formatDuration(59)).toBe("59s");
  });

  it("renders sub-hour values as minutes-and-seconds", () => {
    expect(formatDuration(60)).toBe("1m");
    expect(formatDuration(90)).toBe("1m 30s");
    expect(formatDuration(684)).toBe("11m 24s");
    expect(formatDuration(3599)).toBe("59m 59s");
  });

  it("drops the seconds field for hour-scale durations", () => {
    expect(formatDuration(3600)).toBe("1h");
    expect(formatDuration(3660)).toBe("1h 1m");
    expect(formatDuration(5400)).toBe("1h 30m");
    expect(formatDuration(7325)).toBe("2h 2m");
  });

  it("returns -- for missing or non-finite input", () => {
    expect(formatDuration(null)).toBe("--");
    expect(formatDuration(undefined)).toBe("--");
    expect(formatDuration(NaN)).toBe("--");
    expect(formatDuration(Infinity)).toBe("--");
  });

  it("clamps negatives to zero rather than rendering '-Xs'", () => {
    expect(formatDuration(-5)).toBe("0s");
  });

  it("floors fractional seconds", () => {
    expect(formatDuration(59.9)).toBe("59s");
    expect(formatDuration(60.5)).toBe("1m");
  });
});

describe("formatDurationBetween", () => {
  it("computes the span between two ISO strings", () => {
    expect(
      formatDurationBetween(
        "2026-04-23T22:06:03Z",
        "2026-04-23T22:17:27Z",
      ),
    ).toBe("11m 24s");
  });

  it("returns -- when either endpoint is missing", () => {
    expect(formatDurationBetween(null, "2026-04-23T22:17:27Z")).toBe("--");
    expect(formatDurationBetween("2026-04-23T22:06:03Z", null)).toBe("--");
    expect(formatDurationBetween(null, null)).toBe("--");
    expect(formatDurationBetween(undefined, undefined)).toBe("--");
  });

  it("returns -- for non-positive ranges", () => {
    expect(
      formatDurationBetween(
        "2026-04-23T22:17:27Z",
        "2026-04-23T22:06:03Z",
      ),
    ).toBe("--");
    expect(
      formatDurationBetween(
        "2026-04-23T22:06:03Z",
        "2026-04-23T22:06:03Z",
      ),
    ).toBe("--");
  });

  it("returns -- for unparseable input", () => {
    expect(formatDurationBetween("not-a-date", "2026-04-23T22:17:27Z")).toBe(
      "--",
    );
  });
});
