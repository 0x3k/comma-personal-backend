"use client";

import {
  forwardRef,
  useCallback,
  useEffect,
  useImperativeHandle,
  useRef,
  useState,
} from "react";
import Hls from "hls.js";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";

interface VideoPlayerProps {
  /** HLS manifest URL (.m3u8) */
  src: string;
  /** Additional CSS classes for the wrapper */
  className?: string;
  /**
   * Called as the video plays with the current playback position (in seconds,
   * relative to the stream start). Mirrors the native `timeupdate` event, so
   * it fires a few times per second while playing and once on seek.
   */
  onTimeUpdate?: (currentTime: number) => void;
}

/**
 * Imperative handle exposed to parent components. Consumers can hold a ref to
 * a VideoPlayer instance and call `seek(seconds)` to jump the underlying
 * media element to the given position -- used by the signal timeline to seek
 * when the user clicks on it.
 */
interface VideoPlayerHandle {
  seek: (seconds: number) => void;
}

/**
 * Single-stream HLS video player.
 * Uses hls.js when available, falls back to native HLS support (Safari).
 */
const VideoPlayer = forwardRef<VideoPlayerHandle, VideoPlayerProps>(
  function VideoPlayer({ src, className = "", onTimeUpdate }, ref) {
    const videoRef = useRef<HTMLVideoElement>(null);
    const hlsRef = useRef<Hls | null>(null);
    const mediaRecoveryAttempted = useRef(false);
    const safariCleanupRef = useRef<(() => void) | null>(null);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);

    useImperativeHandle(
      ref,
      () => ({
        seek: (seconds: number) => {
          const video = videoRef.current;
          if (!video) return;
          if (!Number.isFinite(seconds)) return;
          // Clamp into the valid range when we know the duration; otherwise
          // let the browser handle out-of-range values (it will clip).
          const duration = video.duration;
          const target =
            Number.isFinite(duration) && duration > 0
              ? Math.max(0, Math.min(seconds, duration))
              : Math.max(0, seconds);
          try {
            video.currentTime = target;
          } catch {
            // Ignore seek errors (e.g. readyState 0); next play will retry.
          }
        },
      }),
      [],
    );

    const destroyHls = useCallback(() => {
      if (hlsRef.current) {
        hlsRef.current.destroy();
        hlsRef.current = null;
      }
    }, []);

    const initPlayer = useCallback(() => {
      const video = videoRef.current;
      if (!video) return;

      setLoading(true);
      setError(null);
      mediaRecoveryAttempted.current = false;
      destroyHls();
      if (safariCleanupRef.current) {
        safariCleanupRef.current();
        safariCleanupRef.current = null;
      }

      if (Hls.isSupported()) {
        const hls = new Hls({
          enableWorker: true,
          lowLatencyMode: false,
        });
        hlsRef.current = hls;

        hls.on(Hls.Events.MANIFEST_PARSED, () => {
          setLoading(false);
        });

        hls.on(Hls.Events.ERROR, (_event, data) => {
          if (data.fatal) {
            switch (data.type) {
              case Hls.ErrorTypes.NETWORK_ERROR:
                setError("Network error: unable to load video stream");
                setLoading(false);
                break;
              case Hls.ErrorTypes.MEDIA_ERROR:
                if (!mediaRecoveryAttempted.current) {
                  mediaRecoveryAttempted.current = true;
                  hls.recoverMediaError();
                } else {
                  setError("Failed to recover video stream");
                  setLoading(false);
                  hls.destroy();
                }
                break;
              default:
                setError("Failed to load video stream");
                setLoading(false);
                hls.destroy();
                break;
            }
          }
        });

        hls.loadSource(src);
        hls.attachMedia(video);
      } else if (video.canPlayType("application/vnd.apple.mpegurl")) {
        // Native HLS support (Safari)
        video.src = src;

        const handleLoaded = () => setLoading(false);
        const handleError = () => {
          setError("Failed to load video stream");
          setLoading(false);
        };

        video.addEventListener("loadedmetadata", handleLoaded);
        video.addEventListener("error", handleError);

        safariCleanupRef.current = () => {
          video.removeEventListener("loadedmetadata", handleLoaded);
          video.removeEventListener("error", handleError);
        };
      } else {
        setError("HLS playback is not supported in this browser");
        setLoading(false);
      }
    }, [src, destroyHls]);

    useEffect(() => {
      initPlayer();
      return () => {
        destroyHls();
        if (safariCleanupRef.current) {
          safariCleanupRef.current();
          safariCleanupRef.current = null;
        }
      };
    }, [initPlayer, destroyHls]);

    // Wire the optional onTimeUpdate callback to the underlying media element.
    // Installed separately from initPlayer so that swapping callback identities
    // does not tear down the hls.js instance.
    useEffect(() => {
      const video = videoRef.current;
      if (!video || !onTimeUpdate) return;
      const handler = () => onTimeUpdate(video.currentTime);
      video.addEventListener("timeupdate", handler);
      video.addEventListener("seeked", handler);
      return () => {
        video.removeEventListener("timeupdate", handler);
        video.removeEventListener("seeked", handler);
      };
    }, [onTimeUpdate]);

    const handleRetry = useCallback(() => {
      initPlayer();
    }, [initPlayer]);

    return (
      <div
        className={[
          "relative w-full overflow-hidden rounded-lg bg-black",
          className,
        ]
          .filter(Boolean)
          .join(" ")}
      >
        {/* Video element -- always rendered so hls.js can attach */}
        <video
          ref={videoRef}
          className="aspect-video w-full"
          controls
          playsInline
          muted
        />

        {/* Loading overlay */}
        {loading && (
          <div className="absolute inset-0 flex items-center justify-center bg-black/60">
            <Spinner size="lg" label="Loading video" />
          </div>
        )}

        {/* Error overlay */}
        {error && (
          <div className="absolute inset-0 flex items-center justify-center bg-black/80 p-4">
            <ErrorMessage
              title="Video Error"
              message={error}
              retry={handleRetry}
            />
          </div>
        )}
      </div>
    );
  },
);

export { VideoPlayer, type VideoPlayerProps, type VideoPlayerHandle };
