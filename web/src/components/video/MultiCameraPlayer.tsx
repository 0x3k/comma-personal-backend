"use client";

import { forwardRef, useState } from "react";
import { VideoPlayer, type VideoPlayerHandle } from "./VideoPlayer";
import { Button } from "@/components/ui/Button";

/**
 * Camera streams a segment can expose. The HEVC trio (front / wide /
 * driver) is only present for routes the operator preserved or pulled
 * via athena -- by default openpilot/sunnypilot only ship `qcamera.ts`,
 * the low-res preview encoded as H.264 in MPEG-TS. The transcoder
 * worker repackages qcamera.ts into HLS so the player can fall back to
 * it when none of the HEVC streams are available.
 */
type CameraType = "fcamera" | "ecamera" | "dcamera" | "qcamera";

interface CameraOption {
  type: CameraType;
  label: string;
}

const CAMERA_OPTIONS: CameraOption[] = [
  { type: "fcamera", label: "Front" },
  { type: "ecamera", label: "Wide" },
  { type: "dcamera", label: "Driver" },
];

interface MultiCameraPlayerProps {
  /** Base URL for the segment, e.g. `{BASE_URL}/storage/{dongleId}/{route}/{segment}` */
  segmentBaseUrl: string;
  /** Which cameras have been uploaded for this segment */
  availableCameras: CameraType[];
  /** Additional CSS classes for the wrapper */
  className?: string;
  /**
   * Fires with the underlying video's playback position (in seconds) as it
   * plays. Forwarded to the inner VideoPlayer. Consumers use this to drive
   * overlays like the signal timeline.
   */
  onTimeUpdate?: (currentTime: number) => void;
}

/**
 * Multi-camera player with toggle buttons for front/wide/driver cameras.
 * Constructs the HLS manifest URL from the segment base URL and selected
 * camera. When none of the HEVC cameras are available but `qcamera` is,
 * the player falls back to the low-res qcamera preview and surfaces a
 * small badge so the user understands the quality drop is by design.
 */
const MultiCameraPlayer = forwardRef<VideoPlayerHandle, MultiCameraPlayerProps>(
  function MultiCameraPlayer(
    { segmentBaseUrl, availableCameras, className = "", onTimeUpdate },
    ref,
  ) {
    // Pick a sensible default: prefer fcamera, then any HEVC camera, then
    // fall back to qcamera if that is all we have.
    const [activeCamera, setActiveCamera] = useState<CameraType>(() => {
      if (availableCameras.includes("fcamera")) return "fcamera";
      const firstHevc = availableCameras.find(
        (c) => c === "ecamera" || c === "dcamera",
      );
      if (firstHevc) return firstHevc;
      if (availableCameras.includes("qcamera")) return "qcamera";
      return availableCameras[0] ?? "fcamera";
    });

    const hlsUrl = `${segmentBaseUrl}/${activeCamera}/index.m3u8`;
    const hasHevc = CAMERA_OPTIONS.some((c) =>
      availableCameras.includes(c.type),
    );
    const hasQcamera = availableCameras.includes("qcamera");
    // Fall back to qcamera only when there is nothing better. The HEVC
    // toggle buttons remain visible (and disabled) so the layout doesn't
    // shift between routes that do and don't have the HEVC trio.
    const usingQcameraFallback = activeCamera === "qcamera" && !hasHevc;

    return (
      <div
        className={["flex flex-col gap-3", className].filter(Boolean).join(" ")}
      >
        {/* Camera toggle buttons */}
        <div className="flex flex-wrap items-center gap-2">
          {CAMERA_OPTIONS.map((cam) => {
            const available = availableCameras.includes(cam.type);
            const isActive = cam.type === activeCamera;
            return (
              <Button
                key={cam.type}
                variant={isActive ? "primary" : "secondary"}
                size="sm"
                disabled={!available}
                onClick={() => setActiveCamera(cam.type)}
                aria-pressed={isActive}
                title={available ? cam.label : `${cam.label} (not available)`}
              >
                {cam.label}
              </Button>
            );
          })}
          {usingQcameraFallback && (
            <span
              className="ml-1 inline-flex items-center rounded bg-[var(--bg-tertiary)] px-2 py-0.5 text-xs font-medium text-[var(--text-secondary)]"
              title="Only the low-res qcamera preview was uploaded for this route. The full-resolution HEVC streams (front/wide/driver) are not available."
            >
              qcamera preview (low-res)
            </span>
          )}
          {!usingQcameraFallback && !hasHevc && hasQcamera && (
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setActiveCamera("qcamera")}
              title="Low-res preview"
            >
              qcamera
            </Button>
          )}
        </div>

        {/* Video player */}
        <VideoPlayer ref={ref} src={hlsUrl} onTimeUpdate={onTimeUpdate} />
      </div>
    );
  },
);

export {
  MultiCameraPlayer,
  type MultiCameraPlayerProps,
  type CameraType,
};
