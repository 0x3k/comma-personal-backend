import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { MergePlateModal, normalizeHashInput } from "./MergePlateModal";

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

describe("normalizeHashInput", () => {
  it("strips a /plates/<hash> prefix", () => {
    expect(normalizeHashInput("  /plates/AAAbbb  ")).toBe("AAAbbb");
  });

  it("extracts the hash from an absolute URL", () => {
    expect(
      normalizeHashInput("https://dash.example.com/plates/HHHaaa?x=1"),
    ).toBe("HHHaaa");
  });

  it("returns the trimmed input when no /plates/ prefix matches", () => {
    expect(normalizeHashInput(" rawHashHere ")).toBe("rawHashHere");
  });
});

describe("MergePlateModal", () => {
  it("renders nothing when closed", () => {
    const { container } = render(
      <MergePlateModal
        fromHashB64="from123"
        open={false}
        onClose={() => {}}
        onMerged={() => {}}
      />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("prefills destination when prefillToHashB64 is set", () => {
    render(
      <MergePlateModal
        fromHashB64="from123"
        prefillToHashB64="dest456"
        open
        onClose={() => {}}
        onMerged={() => {}}
      />,
    );
    expect(
      (screen.getByTestId("merge-plate-input") as HTMLInputElement).value,
    ).toBe("dest456");
  });

  it("happy path: POST /v1/alpr/plates/merge then call onMerged", async () => {
    fetchMock.mockResolvedValueOnce(
      jsonOk({ accepted: true, affected_routes: 3 }),
    );
    const onMerged = vi.fn();
    const onClose = vi.fn();

    render(
      <MergePlateModal
        fromHashB64="src"
        open
        onClose={onClose}
        onMerged={onMerged}
      />,
    );

    fireEvent.change(screen.getByTestId("merge-plate-input"), {
      target: { value: "dst" },
    });
    fireEvent.click(screen.getByTestId("merge-plate-submit"));

    await waitFor(() => {
      expect(onMerged).toHaveBeenCalledTimes(1);
    });
    expect(onMerged.mock.calls[0][0]).toBe("dst");
    expect(onClose).toHaveBeenCalledTimes(1);

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toContain("/v1/alpr/plates/merge");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({
      from_hash_b64: "src",
      to_hash_b64: "dst",
    });
  });

  it("rejects merge into the same hash inline", async () => {
    render(
      <MergePlateModal
        fromHashB64="abc"
        open
        onClose={() => {}}
        onMerged={() => {}}
      />,
    );
    fireEvent.change(screen.getByTestId("merge-plate-input"), {
      target: { value: "abc" },
    });
    fireEvent.click(screen.getByTestId("merge-plate-submit"));
    await waitFor(() => {
      expect(screen.getByTestId("merge-plate-error").textContent).toContain(
        "differ",
      );
    });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("accepts a /plates/<hash> URL paste in the input", async () => {
    fetchMock.mockResolvedValueOnce(
      jsonOk({ accepted: true, affected_routes: 1 }),
    );
    const onMerged = vi.fn();

    render(
      <MergePlateModal
        fromHashB64="src"
        open
        onClose={() => {}}
        onMerged={onMerged}
      />,
    );

    fireEvent.change(screen.getByTestId("merge-plate-input"), {
      target: { value: "https://dash.example.com/plates/destFromUrl" },
    });
    fireEvent.click(screen.getByTestId("merge-plate-submit"));

    await waitFor(() => {
      expect(onMerged).toHaveBeenCalledWith("destFromUrl");
    });
    const [, init] = fetchMock.mock.calls[0];
    expect(JSON.parse(init.body as string)).toEqual({
      from_hash_b64: "src",
      to_hash_b64: "destFromUrl",
    });
  });
});
