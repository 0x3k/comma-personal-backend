"use client";

import { useEffect, useRef, useState } from "react";
import { BASE_URL } from "@/lib/api";
import { Button } from "@/components/ui/Button";

interface ExportMP4MenuProps {
  /** Dongle ID the route belongs to. */
  dongleId: string;
  /** Route name (comma convention, unencoded). */
  routeName: string;
}

/**
 * ExportMP4Menu surfaces the GET /v1/routes/:dongle/:route/export.mp4
 * endpoint as a small action dropdown on the route detail page. Two
 * options are exposed:
 *
 *   - "Export MP4 (no blur)": the unredacted source. Default for local
 *     exports because the user owns their own data and the unredacted
 *     copy preserves the full visual context.
 *   - "Export MP4 (with plate blur)": the redacted variant. Useful
 *     when the export will leave the operator's machine (sharing the
 *     file with a third party, posting on social media, etc.).
 *
 * Selecting an option opens the URL in a new tab; the browser handles
 * the download via the Content-Disposition: attachment response. The
 * menu closes on outside-click and on selection.
 */
export function ExportMP4Menu({ dongleId, routeName }: ExportMP4MenuProps) {
  const [open, setOpen] = useState(false);
  const wrapperRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    function onDocumentMouseDown(e: MouseEvent) {
      const target = e.target as Node | null;
      if (!wrapperRef.current || !target) return;
      if (!wrapperRef.current.contains(target)) {
        setOpen(false);
      }
    }
    document.addEventListener("mousedown", onDocumentMouseDown);
    return () => document.removeEventListener("mousedown", onDocumentMouseDown);
  }, [open]);

  function exportUrl(redact: boolean): string {
    // The export endpoint is on the same origin as the API; honour
    // BASE_URL so the docker prod stack (frontend on :80, API on
    // :7070) still works.
    const params = new URLSearchParams({ camera: "f" });
    if (redact) {
      params.set("redact_plates", "true");
    }
    return `${BASE_URL}/v1/routes/${encodeURIComponent(dongleId)}/${encodeURIComponent(routeName)}/export.mp4?${params.toString()}`;
  }

  return (
    <div ref={wrapperRef} className="relative inline-flex">
      <Button
        variant="secondary"
        size="sm"
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="menu"
        aria-expanded={open}
      >
        Export MP4
      </Button>
      {open && (
        <div
          role="menu"
          className="absolute right-0 top-full z-10 mt-1 w-56 rounded border border-[var(--border-primary)] bg-[var(--bg-secondary)] py-1 shadow-lg"
        >
          <a
            href={exportUrl(false)}
            target="_blank"
            rel="noreferrer"
            role="menuitem"
            className="block px-3 py-2 text-sm text-[var(--text-primary)] hover:bg-[var(--bg-tertiary)]"
            onClick={() => setOpen(false)}
          >
            Export MP4 (no blur)
          </a>
          <a
            href={exportUrl(true)}
            target="_blank"
            rel="noreferrer"
            role="menuitem"
            className="block px-3 py-2 text-sm text-[var(--text-primary)] hover:bg-[var(--bg-tertiary)]"
            onClick={() => setOpen(false)}
          >
            Export MP4 (with plate blur)
          </a>
        </div>
      )}
    </div>
  );
}
