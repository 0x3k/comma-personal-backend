import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { EditPlateModal } from "./EditPlateModal";

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

function jsonError(status: number, message: string): Response {
  return new Response(JSON.stringify({ error: message, code: status }), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("EditPlateModal", () => {
  it("renders nothing when closed", () => {
    const { container } = render(
      <EditPlateModal
        detectionId={42}
        currentPlate="ABC123"
        open={false}
        onClose={() => {}}
        onSaved={() => {}}
      />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("seeds the input from currentPlate when opened", () => {
    render(
      <EditPlateModal
        detectionId={42}
        currentPlate="ABC123"
        open
        onClose={() => {}}
        onSaved={() => {}}
      />,
    );
    expect(
      (screen.getByTestId("edit-plate-input") as HTMLInputElement).value,
    ).toBe("ABC123");
  });

  it("blocks submission when input is empty", async () => {
    render(
      <EditPlateModal
        detectionId={42}
        currentPlate=""
        open
        onClose={() => {}}
        onSaved={() => {}}
      />,
    );
    fireEvent.click(screen.getByTestId("edit-plate-submit"));
    await waitFor(() => {
      expect(screen.getByTestId("edit-plate-error")).toBeInTheDocument();
    });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("happy path: PATCH /v1/alpr/detections/:id and call onSaved", async () => {
    fetchMock.mockResolvedValueOnce(
      jsonOk({ accepted: true, affected_routes: 2 }),
    );
    const onSaved = vi.fn();
    const onClose = vi.fn();

    render(
      <EditPlateModal
        detectionId={42}
        currentPlate="ABC123"
        open
        onClose={onClose}
        onSaved={onSaved}
      />,
    );

    fireEvent.change(screen.getByTestId("edit-plate-input"), {
      target: { value: "abc124" },
    });
    fireEvent.click(screen.getByTestId("edit-plate-submit"));

    await waitFor(() => {
      expect(onSaved).toHaveBeenCalledTimes(1);
    });
    expect(onClose).toHaveBeenCalledTimes(1);

    // Hit the right endpoint, with the uppercased value.
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toContain("/v1/alpr/detections/42");
    expect(init.method).toBe("PATCH");
    expect(JSON.parse(init.body as string)).toEqual({ plate: "ABC124" });
    expect(onSaved.mock.calls[0][0]).toMatchObject({
      accepted: true,
      affected_routes: 2,
    });
  });

  it("hint path: surfaces match_hash_b64 to onSaved", async () => {
    fetchMock.mockResolvedValueOnce(
      jsonOk({
        accepted: true,
        affected_routes: 1,
        hint: "A different plate already has hash X -- consider merging.",
        match_hash_b64: "OTHERHASHb64",
      }),
    );
    const onSaved = vi.fn();

    render(
      <EditPlateModal
        detectionId={7}
        currentPlate="OLD"
        open
        onClose={() => {}}
        onSaved={onSaved}
      />,
    );

    fireEvent.change(screen.getByTestId("edit-plate-input"), {
      target: { value: "NEW" },
    });
    fireEvent.click(screen.getByTestId("edit-plate-submit"));

    await waitFor(() => {
      expect(onSaved).toHaveBeenCalledTimes(1);
    });
    expect(onSaved.mock.calls[0][0]).toMatchObject({
      hint: expect.stringContaining("merging"),
      match_hash_b64: "OTHERHASHb64",
    });
  });

  it("surfaces server errors inline without closing", async () => {
    fetchMock.mockResolvedValueOnce(jsonError(400, "plate is empty"));
    const onClose = vi.fn();

    render(
      <EditPlateModal
        detectionId={1}
        currentPlate="ABC"
        open
        onClose={onClose}
        onSaved={() => {}}
      />,
    );

    fireEvent.change(screen.getByTestId("edit-plate-input"), {
      target: { value: "Z" },
    });
    fireEvent.click(screen.getByTestId("edit-plate-submit"));

    await waitFor(() => {
      expect(screen.getByTestId("edit-plate-error").textContent).toContain(
        "plate is empty",
      );
    });
    expect(onClose).not.toHaveBeenCalled();
  });

  it("disables submit when detectionId is null", () => {
    render(
      <EditPlateModal
        detectionId={null}
        currentPlate="ABC"
        open
        onClose={() => {}}
        onSaved={() => {}}
      />,
    );
    expect(
      screen.getByTestId("edit-plate-submit").hasAttribute("disabled"),
    ).toBe(true);
    expect(screen.getByTestId("edit-plate-no-detection")).toBeInTheDocument();
  });
});
