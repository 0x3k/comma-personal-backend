import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";
import {
  __setAlertSummaryPollMsForTests,
  classifyAlertSeverity,
  useAlertSummary,
} from "./useAlertSummary";

let fetchMock: ReturnType<typeof vi.fn>;

/**
 * jsdom does not let us reassign document.visibilityState directly --
 * the property is a getter on Document.prototype. We install a writable
 * getter for the duration of the test so toggling visibility is a one-
 * line operation, then restore the original at teardown.
 */
let originalVisibilityDescriptor: PropertyDescriptor | undefined;
let visibilityState: DocumentVisibilityState = "visible";

function installVisibilityShim() {
  originalVisibilityDescriptor = Object.getOwnPropertyDescriptor(
    Document.prototype,
    "visibilityState",
  );
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    get: () => visibilityState,
  });
}

function restoreVisibilityShim() {
  // The descriptor we installed lives on the document instance (not
  // its prototype). Remove it via Reflect so TypeScript's read-only
  // typing for built-in DOM properties does not trip the strict
  // `delete` rule. The prototype getter is intact regardless.
  Reflect.deleteProperty(document, "visibilityState");
  if (originalVisibilityDescriptor) {
    Object.defineProperty(
      Document.prototype,
      "visibilityState",
      originalVisibilityDescriptor,
    );
  }
}

function setVisibility(state: DocumentVisibilityState) {
  visibilityState = state;
  document.dispatchEvent(new Event("visibilitychange"));
}

/**
 * Flush queued microtasks under act(). Used to let a fetch's
 * `.then(...)` chain run before reading state. Several flushes are
 * cheap and cover the longest chain we have (fetch -> setSummary ->
 * scheduleNext, which is two awaits deep).
 */
async function flushMicrotasks() {
  await act(async () => {
    for (let i = 0; i < 5; i++) {
      await Promise.resolve();
    }
  });
}

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  installVisibilityShim();
  visibilityState = "visible";
});

afterEach(() => {
  // Unmount any hooks rendered with renderHook so their effect
  // cleanups (visibilitychange listeners, pending timers) run before
  // the next test mounts and shares the same jsdom document. Without
  // this the listener count grows test over test and fake-timer
  // assertions become flaky from cross-test event handlers firing.
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  __setAlertSummaryPollMsForTests(null);
  restoreVisibilityShim();
});

function jsonOk(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

describe("useAlertSummary", () => {
  it("does not fetch when disabled", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: false });
    renderHook(() => useAlertSummary(false));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000);
    });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("fetches on mount when enabled and exposes the resolved summary", async () => {
    fetchMock.mockResolvedValue(
      jsonOk({ open_count: 3, max_open_severity: 4, last_alert_at: null }),
    );

    const { result } = renderHook(() => useAlertSummary(true));
    await flushMicrotasks();

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(result.current.summary?.open_count).toBe(3);
    expect(result.current.summary?.max_open_severity).toBe(4);
  });

  it("polls on the configured interval while the tab is visible", async () => {
    __setAlertSummaryPollMsForTests(1_000);
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        jsonOk({ open_count: 1, max_open_severity: 2, last_alert_at: null }),
      ),
    );

    // Fake timers from the very start so all setTimeout calls land
    // in the same fake queue.
    vi.useFakeTimers({ shouldAdvanceTime: false });
    renderHook(() => useAlertSummary(true));

    // First fetch fires immediately on mount; let its microtask
    // chain settle so scheduleNext queues the next tick.
    await flushMicrotasks();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Tick #2.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    await flushMicrotasks();
    expect(fetchMock).toHaveBeenCalledTimes(2);

    // Tick #3 -- exercises the recursive chain: scheduleNext was
    // queued after the second resolve, advancing 1s fires it.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    await flushMicrotasks();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("pauses polling when the tab becomes hidden and resumes on return", async () => {
    __setAlertSummaryPollMsForTests(1_000);
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        jsonOk({ open_count: 1, max_open_severity: 2, last_alert_at: null }),
      ),
    );

    vi.useFakeTimers({ shouldAdvanceTime: false });
    renderHook(() => useAlertSummary(true));

    await flushMicrotasks();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Hide the tab BEFORE the next tick fires. The visibility
    // handler clears the pending timeout; advancing 5s should not
    // produce another fetch.
    setVisibility("hidden");

    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000);
    });
    await flushMicrotasks();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Becoming visible again triggers an immediate catch-up fetch.
    setVisibility("visible");
    await flushMicrotasks();
    expect(fetchMock).toHaveBeenCalledTimes(2);

    // Cadence resumes from there.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    await flushMicrotasks();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });
});

describe("classifyAlertSeverity", () => {
  it("maps 1 to amber (the under-2 fallback)", () => {
    expect(classifyAlertSeverity(1)).toBe("amber");
  });

  it("maps 2 and 3 to amber", () => {
    expect(classifyAlertSeverity(2)).toBe("amber");
    expect(classifyAlertSeverity(3)).toBe("amber");
  });

  it("maps 4 and 5 to red", () => {
    expect(classifyAlertSeverity(4)).toBe("red");
    expect(classifyAlertSeverity(5)).toBe("red");
  });

  it("treats null/undefined as amber (the safe default)", () => {
    expect(classifyAlertSeverity(null)).toBe("amber");
    expect(classifyAlertSeverity(undefined)).toBe("amber");
  });
});
