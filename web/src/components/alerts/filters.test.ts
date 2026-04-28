import { describe, expect, it } from "vitest";
import {
  alertsQueryString,
  alertWithinDateRange,
  EMPTY_ALERTS_FILTERS,
  filtersFromSearchParams,
  filtersToSearchParams,
  isDefaultAlertsFilters,
  parseTab,
} from "./filters";

describe("filters: round-trip", () => {
  it("decodes an empty querystring to the default filters", () => {
    const f = filtersFromSearchParams(new URLSearchParams(""));
    expect(f).toEqual(EMPTY_ALERTS_FILTERS);
    expect(isDefaultAlertsFilters(f)).toBe(true);
  });

  it("encodes severity as a comma-separated list", () => {
    const sp = filtersToSearchParams({
      ...EMPTY_ALERTS_FILTERS,
      severity: [3, 5],
    });
    // Sorted ascending; the helper does not enforce order on the
    // input, but the encoded form is whatever the caller passed in
    // (sorted-on-add by the chip toggle).
    expect(sp.get("severity")).toBe("3,5");
  });

  it("round-trips severity + dongle + dates without loss", () => {
    const original = {
      severity: [2, 4] as (0 | 1 | 2 | 3 | 4 | 5)[],
      dongle_id: "dongle-1",
      from: "2026-04-01",
      to: "2026-04-25",
    };
    const sp = filtersToSearchParams({ ...EMPTY_ALERTS_FILTERS, ...original });
    const decoded = filtersFromSearchParams(new URLSearchParams(sp.toString()));
    expect(decoded).toEqual({ ...EMPTY_ALERTS_FILTERS, ...original });
  });

  it("drops malformed severity tokens", () => {
    const f = filtersFromSearchParams(
      new URLSearchParams("severity=2,foo,9,3"),
    );
    // 9 is out of range, "foo" parses as NaN, both dropped.
    expect(f.severity).toEqual([2, 3]);
  });

  it("drops bare YYYY-MM-DD dates that don't match the strict regex", () => {
    const f = filtersFromSearchParams(
      new URLSearchParams("from=not-a-date&to=2026-13-99"),
    );
    expect(f.from).toBe("");
    // 2026-13-99 matches the regex \d{4}-\d{2}-\d{2} so it's kept as-is;
    // alertWithinDateRange parses it as Invalid Date and gives back false
    // for any sensible alert. That is fine: the UI shows the user's typo
    // back at them; the date filter just yields zero rows.
    expect(f.to).toBe("2026-13-99");
  });
});

describe("filters: alertsQueryString", () => {
  it("omits tab=open (the default) but keeps tab=acked", () => {
    const open = alertsQueryString("open", EMPTY_ALERTS_FILTERS);
    expect(open).toBe("");
    const acked = alertsQueryString("acked", EMPTY_ALERTS_FILTERS);
    expect(acked).toBe("tab=acked");
  });

  it("composes filters + tab", () => {
    const qs = alertsQueryString("acked", {
      ...EMPTY_ALERTS_FILTERS,
      severity: [4],
      dongle_id: "dongle-1",
    });
    // The exact key order depends on URLSearchParams insertion order,
    // which is filters first (set in filtersToSearchParams), then tab.
    const sp = new URLSearchParams(qs);
    expect(sp.get("severity")).toBe("4");
    expect(sp.get("dongle_id")).toBe("dongle-1");
    expect(sp.get("tab")).toBe("acked");
  });
});

describe("filters: parseTab", () => {
  it("falls back to open for unknown values", () => {
    expect(parseTab(null)).toBe("open");
    expect(parseTab("")).toBe("open");
    expect(parseTab("foo")).toBe("open");
  });
  it("preserves known tab values", () => {
    expect(parseTab("acked")).toBe("acked");
    expect(parseTab("whitelist")).toBe("whitelist");
  });
});

describe("filters: alertWithinDateRange", () => {
  it("returns true when no bounds are set, regardless of timestamp", () => {
    expect(alertWithinDateRange("2026-04-25T00:00:00Z", "", "")).toBe(true);
    expect(alertWithinDateRange(null, "", "")).toBe(true);
  });
  it("drops alerts with no timestamp when any bound is set", () => {
    expect(alertWithinDateRange(null, "2026-04-01", "")).toBe(false);
    expect(alertWithinDateRange(null, "", "2026-04-01")).toBe(false);
  });
  it("treats `to` as inclusive of the end day", () => {
    // 2026-04-25T23:59:00Z is inside "to=2026-04-25".
    expect(
      alertWithinDateRange("2026-04-25T23:59:00Z", "", "2026-04-25"),
    ).toBe(true);
    // 2026-04-26T00:00:00Z is past the inclusive day; dropped.
    expect(
      alertWithinDateRange("2026-04-26T00:00:00Z", "", "2026-04-25"),
    ).toBe(false);
  });
  it("respects the `from` lower bound (inclusive)", () => {
    expect(
      alertWithinDateRange("2026-04-01T00:00:00Z", "2026-04-01", ""),
    ).toBe(true);
    expect(
      alertWithinDateRange("2026-03-31T23:59:59Z", "2026-04-01", ""),
    ).toBe(false);
  });
});
