"use client";

import { useCallback, useMemo, useState } from "react";

// Segments are 1-minute chunks per the openpilot convention -- the device
// loggerd splits every recording into 60-second slices, so route-relative
// time = segmentNumber * 60 + segment-local time. This is hard-coded into
// the device side, not exposed via the API.
export const SEGMENT_DURATION_SEC = 60;

export interface RoutePlaybackState {
  /** Index of the segment currently mounted in the primary player. */
  currentSegment: number;
  /** Playhead position within the current segment, in seconds. */
  localTime: number;
  /** Route-relative playhead in seconds (currentSegment * 60 + localTime). */
  routeRelativeSec: number;
  /** Total route duration in seconds, derived from segment count. */
  totalDurationSec: number;
  /** True between user pressing play and the next pause/end-of-route. */
  isPlaying: boolean;
  /**
   * Segment-local seconds that the consumer should imperatively seek the
   * primary player to as soon as it reports ready. Set by seekRoute /
   * setSegment / handleEnded; cleared by `consumePendingSeek()`.
   */
  pendingSeekSec: number | null;
  /**
   * True when the consumer should call play() on the primary handle the next
   * time it becomes ready. Cleared by `consumePlayOnReady()`.
   */
  playOnReady: boolean;
}

export interface RoutePlaybackHandlers {
  /** Wire as VideoPlayer's onTimeUpdate -- updates localTime. */
  handleTimeUpdate: (segmentLocalSec: number) => void;
  /** Wire as VideoPlayer's onEnded -- advances to next segment if any. */
  handleEnded: () => void;
}

export interface RoutePlaybackController {
  /** Seek to a route-relative position; switches segments if needed. */
  seekRoute: (routeRelativeSec: number) => void;
  /** Force a specific segment, resetting local time to 0. */
  setSegment: (segmentNumber: number) => void;
  /** Mark playback as playing/paused; mirrors the user's intent. */
  setPlaying: (playing: boolean) => void;
  /** Called by consumer once it has applied the pending seek. */
  consumePendingSeek: () => void;
  /** Called by consumer once it has issued play() in response to playOnReady. */
  consumePlayOnReady: () => void;
}

interface UseRoutePlaybackOptions {
  /** Total segment count for the route. */
  segmentCount: number;
  /** Segments that actually have at least one playable camera. Used to skip
   *  empty/un-uploaded segments when auto-advancing. */
  playableSegmentNumbers: number[];
}

interface UseRoutePlaybackReturn {
  state: RoutePlaybackState;
  handlers: RoutePlaybackHandlers;
  controller: RoutePlaybackController;
}

/**
 * Owns playback state across a route's segments. The component layer attaches
 * handlers to the primary VideoPlayer and calls `controller.seekRoute()` when
 * external sources (SignalTimeline, segment list) want to jump.
 */
export function useRoutePlayback({
  segmentCount,
  playableSegmentNumbers,
}: UseRoutePlaybackOptions): UseRoutePlaybackReturn {
  const sortedPlayable = useMemo(
    () => [...playableSegmentNumbers].sort((a, b) => a - b),
    [playableSegmentNumbers],
  );

  const initialSegment = sortedPlayable[0] ?? 0;
  const [currentSegment, setCurrentSegment] = useState(initialSegment);
  const [localTime, setLocalTime] = useState(0);
  const [isPlaying, setIsPlaying] = useState(false);
  const [pendingSeekSec, setPendingSeekSec] = useState<number | null>(null);
  const [playOnReady, setPlayOnReady] = useState(false);

  const totalDurationSec = segmentCount * SEGMENT_DURATION_SEC;

  const findNextPlayable = useCallback(
    (after: number): number | null => {
      for (const n of sortedPlayable) {
        if (n > after) return n;
      }
      return null;
    },
    [sortedPlayable],
  );

  const findContaining = useCallback(
    (segmentNumber: number): number | null => {
      if (sortedPlayable.includes(segmentNumber)) return segmentNumber;
      for (const n of sortedPlayable) {
        if (n >= segmentNumber) return n;
      }
      return sortedPlayable[sortedPlayable.length - 1] ?? null;
    },
    [sortedPlayable],
  );

  const handleTimeUpdate = useCallback((segmentLocalSec: number) => {
    setLocalTime(segmentLocalSec);
  }, []);

  const handleEnded = useCallback(() => {
    const next = findNextPlayable(currentSegment);
    if (next === null) {
      setIsPlaying(false);
      return;
    }
    setPendingSeekSec(0);
    setPlayOnReady(true);
    setCurrentSegment(next);
    setLocalTime(0);
  }, [currentSegment, findNextPlayable]);

  const seekRoute = useCallback(
    (routeRelativeSec: number) => {
      const clamped = Math.max(
        0,
        Math.min(routeRelativeSec, totalDurationSec - 0.001),
      );
      const targetSegRaw = Math.floor(clamped / SEGMENT_DURATION_SEC);
      const targetSeg = findContaining(targetSegRaw) ?? targetSegRaw;
      const targetLocal = clamped - targetSeg * SEGMENT_DURATION_SEC;
      const localClamped = Math.max(
        0,
        Math.min(targetLocal, SEGMENT_DURATION_SEC - 0.001),
      );
      setPendingSeekSec(localClamped);
      if (isPlaying) setPlayOnReady(true);
      if (targetSeg !== currentSegment) {
        setCurrentSegment(targetSeg);
      }
      setLocalTime(localClamped);
    },
    [currentSegment, findContaining, isPlaying, totalDurationSec],
  );

  const setSegment = useCallback(
    (segmentNumber: number) => {
      const target = findContaining(segmentNumber) ?? segmentNumber;
      setPendingSeekSec(0);
      setCurrentSegment(target);
      setLocalTime(0);
    },
    [findContaining],
  );

  const setPlaying = useCallback((playing: boolean) => {
    setIsPlaying(playing);
  }, []);

  const consumePendingSeek = useCallback(() => {
    setPendingSeekSec(null);
  }, []);

  const consumePlayOnReady = useCallback(() => {
    setPlayOnReady(false);
  }, []);

  const state: RoutePlaybackState = {
    currentSegment,
    localTime,
    routeRelativeSec: currentSegment * SEGMENT_DURATION_SEC + localTime,
    totalDurationSec,
    isPlaying,
    pendingSeekSec,
    playOnReady,
  };

  const handlers: RoutePlaybackHandlers = {
    handleTimeUpdate,
    handleEnded,
  };

  const controller: RoutePlaybackController = {
    seekRoute,
    setSegment,
    setPlaying,
    consumePendingSeek,
    consumePlayOnReady,
  };

  return { state, handlers, controller };
}
