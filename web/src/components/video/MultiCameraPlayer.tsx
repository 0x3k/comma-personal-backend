"use client";

import { forwardRef, useState } from "react";
import { VideoPlayer, type VideoPlayerHandle } from "./VideoPlayer";
import { Button } from "@/components/ui/Button";

/** Camera types available on comma devices. */
type CameraType = "fcamera" | "ecamera" | "dcamera";

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
 * Constructs the HLS manifest URL from the segment base URL and selected camera.
 */
const MultiCameraPlayer = forwardRef<VideoPlayerHandle, MultiCameraPlayerProps>(
  function MultiCameraPlayer(
    { segmentBaseUrl, availableCameras, className = "", onTimeUpdate },
    ref,
  ) {
    // Default to the first available camera
    const [activeCamera, setActiveCamera] = useState<CameraType>(() => {
      if (availableCameras.includes("fcamera")) return "fcamera";
      return availableCameras[0] ?? "fcamera";
    });

    const hlsUrl = `${segmentBaseUrl}/${activeCamera}/index.m3u8`;

    return (
      <div
        className={["flex flex-col gap-3", className].filter(Boolean).join(" ")}
      >
        {/* Camera toggle buttons */}
        <div className="flex gap-2">
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
