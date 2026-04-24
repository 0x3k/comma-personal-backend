"use client";

import { useState } from "react";
import { BASE_URL } from "@/lib/api";

/**
 * Visual variant for the thumbnail. The list cards use the compact
 * `list` variant that sits flush with the card's top edge and fills
 * the card width. The detail hero uses a larger, centered card.
 *
 * Both variants preserve a 16:9 aspect ratio via the `aspect-video`
 * utility so the placeholder occupies exactly the same box as the
 * loaded image -- no layout shift on load, no jump when a broken
 * image is replaced by the skeleton.
 */
type Variant = "list" | "hero";

interface RouteThumbnailProps {
  dongleId: string;
  routeName: string;
  variant?: Variant;
  /** Extra classes appended to the outer container. */
  className?: string;
  /**
   * alt text for the image. Defaults to "" so the thumbnail is treated
   * as decorative -- the adjacent route metadata already names the route.
   */
  alt?: string;
}

/**
 * RouteThumbnail renders the JPEG preview produced by the thumbnail
 * worker at /v1/routes/:dongle_id/:route_name/thumbnail.
 *
 * State model:
 *   - `"loading"`: skeleton shown while the <img> is fetching.
 *   - `"loaded"`:  image fades in on top of the skeleton slot.
 *   - `"error"`:   404 / network failure -- the skeleton stays visible
 *                  and the <img> is removed so broken-image glyphs
 *                  never appear.
 *
 * Auth: the backend endpoint is gated by SessionOrJWT. Browsers attach
 * the session cookie to same-origin <img> requests automatically, and
 * cross-origin requests go through the same pattern the video player
 * already uses for /storage/. No explicit credentials handling is
 * required here.
 *
 * Detail hero policy: the hero image STAYS visible above the video
 * player even when a segment is selected. It is a cheap, always-loaded
 * anchor that orients the user while they scan between segments; the
 * video player below is the interactive surface. Keeping both visible
 * avoids a jarring re-layout when the user opens or closes a segment.
 */
export function RouteThumbnail({
  dongleId,
  routeName,
  variant = "list",
  className = "",
  alt = "",
}: RouteThumbnailProps) {
  const [state, setState] = useState<"loading" | "loaded" | "error">(
    "loading",
  );

  const src = `${BASE_URL}/v1/routes/${encodeURIComponent(dongleId)}/${encodeURIComponent(routeName)}/thumbnail`;

  // Variant styles. We set explicit aspect-video on the outer box so
  // the skeleton reserves the same space the loaded image will occupy.
  const variantClass =
    variant === "hero"
      ? "w-full max-w-[640px] aspect-video"
      : "w-full aspect-video";

  const containerClass = [
    "relative overflow-hidden rounded-md",
    "bg-[var(--bg-tertiary)]",
    variantClass,
    className,
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <div className={containerClass} aria-hidden={alt === "" ? true : undefined}>
      {/* Skeleton / placeholder slot. Rendered in every state except
          "loaded" so a missing or in-flight thumbnail shows a neutral
          block, not a broken-image icon. */}
      {state !== "loaded" && (
        <div
          className="absolute inset-0 bg-[var(--bg-tertiary)]"
          role={state === "loading" ? "status" : undefined}
          aria-label={state === "loading" ? "Loading thumbnail" : undefined}
        />
      )}
      {state !== "error" && (
        // Using a plain <img> rather than next/image: thumbnails live
        // on the Go backend (cross-origin in dev) and we don't have a
        // remotePatterns entry. The native `loading="lazy"` attribute
        // covers the offscreen-fetching requirement; browsers attach
        // session cookies to the request automatically.
        // eslint-disable-next-line @next/next/no-img-element
        <img
          src={src}
          alt={alt}
          loading="lazy"
          decoding="async"
          className={[
            "absolute inset-0 h-full w-full object-cover transition-opacity duration-150",
            state === "loaded" ? "opacity-100" : "opacity-0",
          ].join(" ")}
          onLoad={() => setState("loaded")}
          onError={() => setState("error")}
        />
      )}
    </div>
  );
}
