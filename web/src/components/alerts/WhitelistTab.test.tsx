import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { WhitelistTab } from "./WhitelistTab";
import type { WhitelistItem } from "./api";

function jsonOk(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

function whitelistResponse(items: WhitelistItem[]): Response {
  return jsonOk({ whitelist: items });
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

describe("WhitelistTab: empty state", () => {
  it("renders the spec's empty title when no rows", async () => {
    fetchMock.mockResolvedValueOnce(whitelistResponse([]));
    render(<WhitelistTab />);
    await waitFor(() => {
      expect(screen.getByText("No whitelisted plates")).toBeInTheDocument();
    });
  });
});

describe("WhitelistTab: add form validation", () => {
  it("rejects an all-whitespace plate without round-tripping", async () => {
    fetchMock.mockResolvedValueOnce(whitelistResponse([]));
    render(<WhitelistTab />);

    await waitFor(() => {
      // Wait until the initial load completes and the form is visible.
      expect(screen.getByTestId("whitelist-add-form")).toBeInTheDocument();
    });

    const input = screen.getByLabelText(/^plate$/i) as HTMLInputElement;
    fireEvent.change(input, { target: { value: " - . " } });
    // Required-attr fires only when value is "" -- a whitespace string
    // passes the HTML required check, so our normalize() check is the
    // load-bearing validator. That's exactly what we want to test.
    fireEvent.submit(screen.getByTestId("whitelist-add-form"));

    expect(
      await screen.findByText(/empty after normalization/i),
    ).toBeInTheDocument();
    // Only the initial GET happened; no POST went out for the rejected
    // submission.
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("posts to /v1/alpr/whitelist and refreshes on success", async () => {
    // Initial GET: empty.
    fetchMock.mockResolvedValueOnce(whitelistResponse([]));
    // POST: success.
    fetchMock.mockResolvedValueOnce(
      jsonOk({ plate_hash_b64: "h-new", kind: "whitelist" }),
    );
    // Refresh GET: the new row.
    fetchMock.mockResolvedValueOnce(
      whitelistResponse([
        {
          plate_hash_b64: "h-new",
          plate: "ABC123",
          label: "Mom's car",
          created_at: "2026-04-25T13:00:00Z",
        },
      ]),
    );

    render(<WhitelistTab />);

    await waitFor(() => {
      expect(screen.getByTestId("whitelist-add-form")).toBeInTheDocument();
    });

    fireEvent.change(screen.getByLabelText(/^plate$/i), {
      target: { value: "abc-123" },
    });
    fireEvent.change(screen.getByLabelText(/label/i), {
      target: { value: "Mom's car" },
    });
    fireEvent.click(screen.getByTestId("whitelist-add-submit"));

    // After the round-trip + refetch, the new row appears.
    await waitFor(() => {
      expect(screen.getByTestId("whitelist-row-h-new")).toBeInTheDocument();
    });

    const calls = fetchMock.mock.calls;
    expect(calls.length).toBe(3);

    // The POST went to /v1/alpr/whitelist with the unnormalized plate
    // text -- the backend normalizes server-side; we just pass through.
    const postCall = calls[1];
    expect(postCall[0]).toContain("/v1/alpr/whitelist");
    expect(postCall[1].method).toBe("POST");
    const postBody = JSON.parse(postCall[1].body as string);
    expect(postBody.plate).toBe("abc-123");
    expect(postBody.label).toBe("Mom's car");

    // The form should be cleared on success.
    expect(
      (screen.getByLabelText(/^plate$/i) as HTMLInputElement).value,
    ).toBe("");
  });
});

describe("WhitelistTab: remove", () => {
  it("optimistically removes a row on click", async () => {
    fetchMock.mockResolvedValueOnce(
      whitelistResponse([
        {
          plate_hash_b64: "h-1",
          plate: "ABC123",
          created_at: "2026-04-25T13:00:00Z",
        },
        {
          plate_hash_b64: "h-2",
          plate: "DEF456",
          created_at: "2026-04-25T13:00:00Z",
        },
      ]),
    );
    fetchMock.mockResolvedValueOnce(jsonOk({ removed: true }));

    render(<WhitelistTab />);

    await waitFor(() => {
      expect(screen.getByTestId("whitelist-row-h-1")).toBeInTheDocument();
    });

    fireEvent.click(screen.getByTestId("whitelist-remove-h-1"));

    // The row should drop out of the list immediately (optimistic).
    await waitFor(() => {
      expect(screen.queryByTestId("whitelist-row-h-1")).toBeNull();
    });
    expect(screen.getByTestId("whitelist-row-h-2")).toBeInTheDocument();

    const calls = fetchMock.mock.calls;
    const deleteCall = calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === "DELETE",
    );
    expect(deleteCall).toBeDefined();
    expect(deleteCall![0]).toContain("/v1/alpr/whitelist/h-1");
  });
});
