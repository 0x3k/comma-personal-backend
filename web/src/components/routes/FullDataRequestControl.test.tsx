import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { FullDataRequestControl } from "./FullDataRequestControl";
import type {
  RouteDataRequestPostResponse,
  RouteDataRequestStatusResponse,
} from "@/lib/types";

/**
 * fetchMock is a tiny one-off stub that returns whatever the test
 * queues up next. We do not use msw because the production code
 * always goes through the apiFetch wrapper, which itself just calls
 * the global fetch -- a plain vi.fn() is enough.
 */
type ResponseSpec =
  | { status: number; body: unknown }
  | { status: number; bodyText: string };

function jsonResponse(spec: ResponseSpec): Response {
  if ("body" in spec) {
    return new Response(JSON.stringify(spec.body), {
      status: spec.status,
      headers: { "Content-Type": "application/json" },
    });
  }
  return new Response(spec.bodyText, {
    status: spec.status,
    headers: { "Content-Type": "application/json" },
  });
}

function makePostResponse(
  overrides: Partial<RouteDataRequestPostResponse["request"]> = {},
): RouteDataRequestPostResponse {
  return {
    request: {
      id: 42,
      routeId: 7,
      requestedBy: null,
      requestedAt: "2026-04-25T12:00:00Z",
      kind: "all",
      status: "dispatched",
      dispatchedAt: "2026-04-25T12:00:01Z",
      completedAt: null,
      error: null,
      filesRequested: 36,
      ...overrides,
    },
  };
}

function makeStatusResponse(
  overrides: {
    status?: RouteDataRequestPostResponse["request"]["status"];
    filesUploaded?: number;
    filesRequested?: number;
    error?: string | null;
  } = {},
): RouteDataRequestStatusResponse {
  const filesRequested = overrides.filesRequested ?? 36;
  const filesUploaded = overrides.filesUploaded ?? 14;
  return {
    request: {
      id: 42,
      routeId: 7,
      requestedBy: null,
      requestedAt: "2026-04-25T12:00:00Z",
      kind: "all",
      status: overrides.status ?? "dispatched",
      dispatchedAt: "2026-04-25T12:00:01Z",
      completedAt: overrides.status === "complete"
        ? "2026-04-25T12:05:00Z"
        : null,
      error: overrides.error ?? null,
      filesRequested,
    },
    progress: {
      filesRequested,
      filesUploaded,
      percent:
        filesRequested === 0
          ? 100
          : Math.floor((filesUploaded * 100) / filesRequested),
    },
    segments: [],
  };
}

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  // Use fake timers but advance with shouldAdvanceTime so waitFor's
  // internal polling still progresses without us having to manually
  // tick after every microtask. This is the recommended pattern when
  // mixing RTL's findBy/waitFor helpers with vi.useFakeTimers().
  vi.useFakeTimers({ shouldAdvanceTime: true });
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  cleanup();
});

/** Open the kind menu and click the option matching the given label. */
function openMenuAndClick(label: string) {
  fireEvent.click(
    screen.getByRole("button", {
      name: /Request full-resolution data for this drive/i,
    }),
  );
  const menu = screen.getByRole("menu");
  fireEvent.click(within(menu).getByRole("menuitem", { name: new RegExp(label, "i") }));
}

describe("FullDataRequestControl", () => {
  it("disables a kind while a request for that kind is in flight", async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ status: 201, body: makePostResponse({ status: "dispatched" }) }),
    );
    // Subsequent polls return in-flight progress.
    fetchMock.mockResolvedValue(
      jsonResponse({
        status: 200,
        body: makeStatusResponse({ status: "dispatched", filesUploaded: 5 }),
      }),
    );

    render(
      <FullDataRequestControl dongleId="dongle-1" routeName="route-1" />,
    );

    openMenuAndClick("Everything");

    // Wait until the progress text shows up. Vitest fake timers don't
    // advance microtasks, so we await pending promises explicitly.
    await waitFor(() => {
      expect(
        screen.getAllByText(/Everything: uploading 5 \/ 36 files/).length,
      ).toBeGreaterThan(0);
    });

    // Re-open the menu: the "Everything" option should now be disabled
    // because the in-flight row tracks the same kind.
    fireEvent.click(
      screen.getByRole("button", {
        name: /Request full-resolution data for this drive/i,
      }),
    );
    const menu = screen.getByRole("menu");
    const everythingItem = within(menu).getByRole("menuitem", {
      name: /Everything/i,
    });
    expect(everythingItem).toBeDisabled();
  });

  it("invokes onComplete and stops showing progress once status=complete", async () => {
    const onComplete = vi.fn();

    fetchMock.mockResolvedValueOnce(
      jsonResponse({ status: 201, body: makePostResponse({ status: "dispatched" }) }),
    );
    // First poll: still uploading.
    fetchMock.mockResolvedValueOnce(
      jsonResponse({
        status: 200,
        body: makeStatusResponse({ status: "dispatched", filesUploaded: 18 }),
      }),
    );
    // Second poll (after timer tick): complete.
    fetchMock.mockResolvedValueOnce(
      jsonResponse({
        status: 200,
        body: makeStatusResponse({
          status: "complete",
          filesUploaded: 36,
          filesRequested: 36,
        }),
      }),
    );

    render(
      <FullDataRequestControl
        dongleId="dongle-1"
        routeName="route-1"
        onComplete={onComplete}
      />,
    );

    openMenuAndClick("Everything");

    await waitFor(() => {
      expect(
        screen.getAllByText(/Everything: uploading 18 \/ 36 files/).length,
      ).toBeGreaterThan(0);
    });

    // Advance the polling interval. The next poll resolves to status=complete.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000);
    });

    await waitFor(() => {
      expect(
        screen.getAllByText(/Everything: complete/).length,
      ).toBeGreaterThan(0);
    });
    expect(onComplete).toHaveBeenCalledTimes(1);
  });

  it("shows an inline error and posts a fresh request on retry", async () => {
    // First POST -> 500. We must shape the response so apiFetch's
    // error path picks up the message field.
    fetchMock.mockResolvedValueOnce(
      jsonResponse({
        status: 500,
        body: { error: "device returned 503", code: 500 },
      }),
    );

    render(
      <FullDataRequestControl dongleId="dongle-1" routeName="route-1" />,
    );

    openMenuAndClick("Full video");

    await waitFor(() => {
      expect(
        screen.getAllByText(/Full video request failed: device returned 503/)
          .length,
      ).toBeGreaterThan(0);
    });

    // Retry path: queue up a successful POST followed by a poll that
    // shows progress so we can assert the fresh request actually went
    // out.
    fetchMock.mockResolvedValueOnce(
      jsonResponse({
        status: 201,
        body: makePostResponse({ id: 99, kind: "full_video", status: "dispatched", filesRequested: 9 }),
      }),
    );
    fetchMock.mockResolvedValue(
      jsonResponse({
        status: 200,
        body: {
          request: {
            id: 99,
            routeId: 7,
            requestedBy: null,
            requestedAt: "2026-04-25T12:00:00Z",
            kind: "full_video",
            status: "dispatched",
            dispatchedAt: "2026-04-25T12:00:01Z",
            completedAt: null,
            error: null,
            filesRequested: 9,
          },
          progress: { filesRequested: 9, filesUploaded: 2, percent: 22 },
          segments: [],
        },
      }),
    );

    fireEvent.click(
      screen.getByRole("button", { name: /Try full video again/i }),
    );

    await waitFor(() => {
      expect(
        screen.getAllByText(/Full video: uploading 2 \/ 9 files/).length,
      ).toBeGreaterThan(0);
    });
  });
});
