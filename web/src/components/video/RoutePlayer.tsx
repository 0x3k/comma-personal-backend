"use client";

import {
  forwardRef,
  useCallback,
  useEffect,
  useImperativeHandle,
  useMemo,
  useRef,
  useState,
} from "react";
import { VideoPlayer, type VideoPlayerHandle } from "./VideoPlayer";
import { useRoutePlayback } from "./useRoutePlayback";
import type { CameraType } from "./MultiCameraPlayer";
import type { Segment } from "@/lib/types";
import { Button } from "@/components/ui/Button";
import {
  loadPlayerPrefs,
  savePlayerPrefs,
  slotCount,
  padSources,
  type PlayerLayout,
  type PlayerPrefs,
} from "@/lib/playerPrefs";

const CAMERA_LABEL: Record<CameraType, string> = {
  fcamera: "Front",
  ecamera: "Wide",
  dcamera: "Driver",
  qcamera: "qcamera (low-res)",
};

const LAYOUT_OPTIONS: { value: PlayerLayout; label: string }[] = [
  { value: "single", label: "Single" },
  { value: "pip", label: "PIP" },
  { value: "split", label: "Split" },
  { value: "grid3", label: "Grid 3-up" },
  { value: "main2pip", label: "Main + 2 PIP" },
];

const SLOT_LABELS: Record<PlayerLayout, string[]> = {
  single: ["Camera"],
  pip: ["Main", "Inset"],
  split: ["Left", "Right"],
  grid3: ["Left", "Center", "Right"],
  main2pip: ["Main", "Inset top", "Inset bottom"],
};

const ALL_CAMERAS: CameraType[] = ["fcamera", "ecamera", "dcamera", "qcamera"];

export interface RoutePlayerHandle {
  /** Seek to a route-relative time (segment * 60 + local). */
  seekRoute: (routeRelativeSec: number) => void;
}

interface RoutePlayerProps {
  segments: Segment[];
  /** Builds the per-segment base URL (without the camera suffix). */
  buildSegmentBaseUrl: (segmentNumber: number) => string;
  /** Reports route-relative playback time for downstream consumers. */
  onTimeUpdate?: (routeRelativeSec: number) => void;
  /** Called whenever the currently-playing segment changes. */
  onCurrentSegmentChange?: (segmentNumber: number) => void;
  className?: string;
}

function getAvailableCameras(segment: Segment): CameraType[] {
  const cameras: CameraType[] = [];
  if (segment.fcameraUploaded) cameras.push("fcamera");
  if (segment.ecameraUploaded) cameras.push("ecamera");
  if (segment.dcameraUploaded) cameras.push("dcamera");
  if (segment.qcameraUploaded) cameras.push("qcamera");
  return cameras;
}

/** Picks a usable camera for a segment if the chosen one is missing. Mirrors
 *  the fallback rules in MultiCameraPlayer.tsx so behavior is consistent. */
function resolveCamera(
  desired: CameraType,
  available: CameraType[],
): CameraType | null {
  if (available.length === 0) return null;
  if (available.includes(desired)) return desired;
  if (available.includes("fcamera")) return "fcamera";
  const hevc = available.find((c) => c === "ecamera" || c === "dcamera");
  if (hevc) return hevc;
  if (available.includes("qcamera")) return "qcamera";
  return available[0];
}

/**
 * Continuous, multi-source player for a route. Holds per-route playback state
 * and renders 1-3 VideoPlayer instances based on the selected layout. The
 * primary slot drives auto-advance and play/pause; follower slots mirror the
 * primary's segment + currentTime, muted and chromeless.
 */
const RoutePlayer = forwardRef<RoutePlayerHandle, RoutePlayerProps>(
  function RoutePlayer(
    {
      segments,
      buildSegmentBaseUrl,
      onTimeUpdate,
      onCurrentSegmentChange,
      className = "",
    },
    ref,
  ) {
    const playableSegmentNumbers = useMemo(
      () =>
        segments
          .filter((s) => getAvailableCameras(s).length > 0)
          .map((s) => s.number),
      [segments],
    );

    const segmentByNumber = useMemo(() => {
      const map = new Map<number, Segment>();
      for (const s of segments) map.set(s.number, s);
      return map;
    }, [segments]);

    const segmentCount = segments.length;

    const { state, handlers, controller } = useRoutePlayback({
      segmentCount,
      playableSegmentNumbers,
    });

    // -- Layout + source preferences (persisted) --------------------------
    const [prefs, setPrefs] = useState<PlayerPrefs>(() => loadPlayerPrefs());

    useEffect(() => {
      savePlayerPrefs(prefs);
    }, [prefs]);

    const setLayout = useCallback((layout: PlayerLayout) => {
      setPrefs((prev) => {
        const target = slotCount(layout);
        return { layout, sources: padSources(prev.sources, target) };
      });
    }, []);

    const setSourceAt = useCallback(
      (index: number, cam: CameraType) => {
        setPrefs((prev) => {
          const next = [...prev.sources];
          next[index] = cam;
          return { ...prev, sources: next };
        });
        // Changing the primary camera remounts its VideoPlayer (key changes
        // with the URL), which would otherwise restart at t=0 of the new
        // camera. Re-seeking via the controller stashes the current position
        // and resumes play once the new manifest is ready. Followers sync to
        // the primary in their own onReady so they don't need this.
        if (index === 0) {
          controller.seekRoute(state.routeRelativeSec);
        }
      },
      [controller, state.routeRelativeSec],
    );

    // The current segment may not have every camera; resolve each slot to a
    // playable URL using the same fallback rules as MultiCameraPlayer.
    const currentSegment = segmentByNumber.get(state.currentSegment);
    const availableForCurrent = useMemo(
      () => (currentSegment ? getAvailableCameras(currentSegment) : []),
      [currentSegment],
    );

    // If multi-source layout is requested but the segment only has qcamera
    // (i.e. no second distinct camera), collapse to single mode for this
    // segment to avoid showing the same low-res feed in every slot.
    const effectiveSlotCount = useMemo(() => {
      const requested = slotCount(prefs.layout);
      const distinctAvailable = availableForCurrent.length;
      if (distinctAvailable === 0) return 0;
      return Math.min(requested, distinctAvailable);
    }, [prefs.layout, availableForCurrent.length]);

    const effectiveLayout: PlayerLayout = useMemo(() => {
      if (effectiveSlotCount <= 1) return "single";
      if (effectiveSlotCount === 2) {
        return prefs.layout === "pip" || prefs.layout === "split"
          ? prefs.layout
          : "split";
      }
      return prefs.layout === "main2pip" ? "main2pip" : "grid3";
    }, [effectiveSlotCount, prefs.layout]);

    // Resolve each requested source to a playable URL for the current segment.
    const slotCameras: (CameraType | null)[] = useMemo(() => {
      const out: (CameraType | null)[] = [];
      for (let i = 0; i < effectiveSlotCount; i++) {
        const desired = prefs.sources[i] ?? "fcamera";
        out.push(resolveCamera(desired, availableForCurrent));
      }
      return out;
    }, [effectiveSlotCount, prefs.sources, availableForCurrent]);

    // -- Player refs (one per slot) ---------------------------------------
    const slotRefs = useRef<(VideoPlayerHandle | null)[]>([]);
    slotRefs.current.length = effectiveSlotCount;
    const setSlotRef = useCallback(
      (index: number) => (h: VideoPlayerHandle | null) => {
        slotRefs.current[index] = h;
      },
      [],
    );

    // Notify parent when the current segment changes.
    useEffect(() => {
      onCurrentSegmentChange?.(state.currentSegment);
    }, [state.currentSegment, onCurrentSegmentChange]);

    // Mirror route-relative time to parent.
    useEffect(() => {
      onTimeUpdate?.(state.routeRelativeSec);
    }, [state.routeRelativeSec, onTimeUpdate]);

    // -- Pending seek + play-on-ready drain --------------------------------
    // When the primary becomes ready (manifest parsed) we apply any pending
    // segment-local seek and resume play if the user was playing across the
    // segment boundary.
    const handlePrimaryReady = useCallback(() => {
      const primary = slotRefs.current[0];
      if (!primary) return;
      if (state.pendingSeekSec !== null) {
        primary.seek(state.pendingSeekSec);
        // Sync followers immediately too.
        for (let i = 1; i < slotRefs.current.length; i++) {
          slotRefs.current[i]?.seek(state.pendingSeekSec);
        }
        controller.consumePendingSeek();
      }
      if (state.playOnReady) {
        primary.play();
        for (let i = 1; i < slotRefs.current.length; i++) {
          slotRefs.current[i]?.play();
        }
        controller.consumePlayOnReady();
      }
    }, [state.pendingSeekSec, state.playOnReady, controller]);

    // Same-segment seeks don't trigger handleReady (the players stay mounted),
    // so we drain the pending seek here as long as the segment hasn't changed.
    // Cross-segment seeks are intentionally left for handlePrimaryReady to
    // drain once the new manifest has parsed.
    const prevSegmentRef = useRef(state.currentSegment);
    useEffect(() => {
      const segmentChanged = prevSegmentRef.current !== state.currentSegment;
      prevSegmentRef.current = state.currentSegment;
      if (segmentChanged) return;
      if (state.pendingSeekSec === null) return;
      const primary = slotRefs.current[0];
      if (!primary) return;
      primary.seek(state.pendingSeekSec);
      for (let i = 1; i < slotRefs.current.length; i++) {
        slotRefs.current[i]?.seek(state.pendingSeekSec);
      }
      controller.consumePendingSeek();
    }, [state.pendingSeekSec, state.currentSegment, controller]);

    // -- Follower drift correction ----------------------------------------
    // On every primary timeupdate, check followers stay within 250ms. If they
    // drift, seek them. This handles the small lag introduced by the second
    // hls.js instance buffering at its own pace.
    useEffect(() => {
      if (effectiveSlotCount <= 1) return;
      const primary = slotRefs.current[0];
      if (!primary) return;
      const t = primary.getCurrentTime();
      for (let i = 1; i < slotRefs.current.length; i++) {
        const follower = slotRefs.current[i];
        if (!follower) continue;
        const ft = follower.getCurrentTime();
        if (Math.abs(ft - t) > 0.25) {
          follower.seek(t);
        }
      }
    }, [state.localTime, effectiveSlotCount]);

    // Mirror play/pause to followers.
    useEffect(() => {
      if (effectiveSlotCount <= 1) return;
      for (let i = 1; i < slotRefs.current.length; i++) {
        const follower = slotRefs.current[i];
        if (!follower) continue;
        if (state.isPlaying) follower.play();
        else follower.pause();
      }
    }, [state.isPlaying, effectiveSlotCount]);

    // -- Imperative handle for parent (SignalTimeline + segment list) ------
    useImperativeHandle(
      ref,
      () => ({
        seekRoute: (routeRelativeSec: number) => {
          controller.seekRoute(routeRelativeSec);
        },
      }),
      [controller],
    );

    // -- Render -----------------------------------------------------------
    const showFallbackBadge =
      slotCameras[0] === "qcamera" &&
      !availableForCurrent.some(
        (c) => c === "fcamera" || c === "ecamera" || c === "dcamera",
      );

    if (effectiveSlotCount === 0 || !currentSegment) {
      return (
        <div
          className={["flex flex-col gap-3", className]
            .filter(Boolean)
            .join(" ")}
        >
          <p className="text-sm text-[var(--text-secondary)]">
            No playable video available for this route.
          </p>
        </div>
      );
    }

    return (
      <div
        className={["flex flex-col gap-3", className].filter(Boolean).join(" ")}
      >
        {/* Layout + camera toolbar */}
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <div className="flex items-center gap-1.5">
            <span className="text-xs uppercase tracking-wide text-[var(--text-secondary)]">
              Layout
            </span>
            {LAYOUT_OPTIONS.map((opt) => (
              <Button
                key={opt.value}
                variant={prefs.layout === opt.value ? "primary" : "secondary"}
                size="sm"
                onClick={() => setLayout(opt.value)}
                aria-pressed={prefs.layout === opt.value}
              >
                {opt.label}
              </Button>
            ))}
          </div>

          <div className="flex flex-wrap items-center gap-3">
            {Array.from({ length: slotCount(prefs.layout) }).map((_, i) => {
              const labels = SLOT_LABELS[prefs.layout];
              const value = prefs.sources[i] ?? "fcamera";
              return (
                <label
                  key={i}
                  className="flex items-center gap-1.5 text-xs text-[var(--text-secondary)]"
                >
                  <span>{labels[i] ?? `Source ${i + 1}`}</span>
                  <select
                    value={value}
                    onChange={(e) =>
                      setSourceAt(i, e.target.value as CameraType)
                    }
                    className="rounded border border-[var(--border-primary)] bg-[var(--bg-secondary)] px-2 py-1 text-xs text-[var(--text-primary)]"
                  >
                    {ALL_CAMERAS.map((cam) => (
                      <option key={cam} value={cam}>
                        {CAMERA_LABEL[cam]}
                      </option>
                    ))}
                  </select>
                </label>
              );
            })}
          </div>

          <div className="ml-auto flex items-center gap-2 text-xs text-[var(--text-secondary)]">
            <span>
              Segment {state.currentSegment} / {Math.max(0, segmentCount - 1)}
            </span>
            {showFallbackBadge && (
              <span
                className="inline-flex items-center rounded bg-[var(--bg-tertiary)] px-2 py-0.5 text-xs font-medium text-[var(--text-secondary)]"
                title="Only the low-res qcamera preview is available for this segment."
              >
                qcamera fallback
              </span>
            )}
            {effectiveLayout !== prefs.layout && (
              <span
                className="inline-flex items-center rounded bg-[var(--bg-tertiary)] px-2 py-0.5 text-xs font-medium text-[var(--text-secondary)]"
                title={`Layout downgraded to ${effectiveLayout} for this segment because not enough distinct cameras are available.`}
              >
                {effectiveLayout} (this segment)
              </span>
            )}
          </div>
        </div>

        {/* Player surface */}
        <PlayerSurface
          layout={effectiveLayout}
          slotCameras={slotCameras}
          buildSegmentBaseUrl={buildSegmentBaseUrl}
          currentSegment={state.currentSegment}
          setSlotRef={setSlotRef}
          onPrimaryTimeUpdate={handlers.handleTimeUpdate}
          onPrimaryEnded={handlers.handleEnded}
          onPrimaryReady={handlePrimaryReady}
          onPrimaryPlay={() => controller.setPlaying(true)}
          onPrimaryPause={() => controller.setPlaying(false)}
          onFollowerReady={(index) => {
            const primary = slotRefs.current[0];
            const follower = slotRefs.current[index];
            if (!primary || !follower) return;
            follower.seek(primary.getCurrentTime());
            if (state.isPlaying) follower.play();
          }}
        />
      </div>
    );
  },
);

interface PlayerSurfaceProps {
  layout: PlayerLayout;
  slotCameras: (CameraType | null)[];
  buildSegmentBaseUrl: (segmentNumber: number) => string;
  currentSegment: number;
  setSlotRef: (index: number) => (h: VideoPlayerHandle | null) => void;
  onPrimaryTimeUpdate: (sec: number) => void;
  onPrimaryEnded: () => void;
  onPrimaryReady: () => void;
  onPrimaryPlay: () => void;
  onPrimaryPause: () => void;
  onFollowerReady: (index: number) => void;
}

function PlayerSurface({
  layout,
  slotCameras,
  buildSegmentBaseUrl,
  currentSegment,
  setSlotRef,
  onPrimaryTimeUpdate,
  onPrimaryEnded,
  onPrimaryReady,
  onPrimaryPlay,
  onPrimaryPause,
  onFollowerReady,
}: PlayerSurfaceProps) {
  const base = buildSegmentBaseUrl(currentSegment);
  const urls = slotCameras.map((cam) =>
    cam ? `${base}/${cam}/index.m3u8` : null,
  );

  function renderSlot(index: number, extraClass = "") {
    const url = urls[index];
    if (!url) return null;
    const isPrimary = index === 0;
    return (
      <VideoPlayer
        // Remount when segment or camera URL changes so hls.js cleanly
        // reattaches with the new manifest.
        key={`slot-${index}-${url}`}
        ref={setSlotRef(index)}
        src={url}
        controls={isPrimary}
        muted
        className={extraClass}
        onTimeUpdate={isPrimary ? onPrimaryTimeUpdate : undefined}
        onEnded={isPrimary ? onPrimaryEnded : undefined}
        onReady={
          isPrimary ? onPrimaryReady : () => onFollowerReady(index)
        }
        onPlay={isPrimary ? onPrimaryPlay : undefined}
        onPause={isPrimary ? onPrimaryPause : undefined}
      />
    );
  }

  if (layout === "single") {
    return <div>{renderSlot(0)}</div>;
  }

  if (layout === "split") {
    return (
      <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
        {renderSlot(0)}
        {renderSlot(1)}
      </div>
    );
  }

  if (layout === "grid3") {
    return (
      <div className="grid grid-cols-1 gap-2 md:grid-cols-3">
        {renderSlot(0)}
        {renderSlot(1)}
        {renderSlot(2)}
      </div>
    );
  }

  if (layout === "pip") {
    return (
      <div className="relative">
        {renderSlot(0)}
        <div className="pointer-events-none absolute bottom-3 right-3 w-1/4 overflow-hidden rounded-md shadow-lg ring-1 ring-white/30">
          {renderSlot(1)}
        </div>
      </div>
    );
  }

  // main2pip
  return (
    <div className="relative">
      {renderSlot(0)}
      <div className="pointer-events-none absolute bottom-3 right-3 flex w-1/4 flex-col gap-2">
        <div className="overflow-hidden rounded-md shadow-lg ring-1 ring-white/30">
          {renderSlot(1)}
        </div>
        <div className="overflow-hidden rounded-md shadow-lg ring-1 ring-white/30">
          {renderSlot(2)}
        </div>
      </div>
    </div>
  );
}

export { RoutePlayer };
