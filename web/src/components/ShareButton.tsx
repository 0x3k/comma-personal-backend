"use client";

import { useState } from "react";
import { apiFetch } from "@/lib/api";
import type { CreateShareResponse } from "@/lib/types";
import { Button } from "@/components/ui/Button";

interface ShareButtonProps {
  /** Dongle ID the route belongs to. */
  dongleId: string;
  /** Route name (comma convention, unencoded). */
  routeName: string;
  /** How long the minted link should live, in hours. Default 72. */
  expiresInHours?: number;
}

/**
 * ShareButton mints a read-only share link for the given route and copies
 * it to the clipboard. It never leaves the current page -- the response
 * URL is only displayed as an inline status message so the operator can
 * see (and re-copy) what was generated.
 *
 * The "Blur license plates" checkbox toggles whether the resulting
 * token has redact_plates=true (the default, privacy-respecting
 * outbound default) or false. The flag is signed into the token
 * server-side so recipients cannot bypass it client-side.
 */
export function ShareButton({
  dongleId,
  routeName,
  expiresInHours = 72,
}: ShareButtonProps) {
  const [busy, setBusy] = useState(false);
  const [share, setShare] = useState<CreateShareResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  // Default ON: outbound shares should not broadcast other people's
  // license plates. Operator can opt out per-link via the checkbox.
  const [redactPlates, setRedactPlates] = useState(true);

  async function createLink() {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      const data = await apiFetch<CreateShareResponse>(
        `/v1/routes/${encodeURIComponent(dongleId)}/${encodeURIComponent(routeName)}/share`,
        {
          method: "POST",
          body: { expires_in_hours: expiresInHours, redact_plates: redactPlates },
        },
      );
      setShare(data);
      try {
        await navigator.clipboard.writeText(data.url);
        setCopied(true);
      } catch {
        // Clipboard access can be denied (insecure context, permission
        // not granted); we still show the URL so the operator can copy
        // manually.
        setCopied(false);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create share link");
    } finally {
      setBusy(false);
    }
  }

  async function copyAgain() {
    if (!share) return;
    try {
      await navigator.clipboard.writeText(share.url);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  }

  return (
    <div className="flex flex-col gap-2">
      <label className="flex cursor-pointer items-center gap-2 text-xs text-[var(--text-secondary)]">
        <input
          type="checkbox"
          checked={redactPlates}
          onChange={(e) => setRedactPlates(e.target.checked)}
          disabled={busy}
          aria-label="Blur license plates in shared video"
        />
        Blur license plates in shared video
      </label>
      <div className="flex items-center gap-2">
        <Button
          variant="secondary"
          size="sm"
          onClick={() => {
            void createLink();
          }}
          disabled={busy}
        >
          {busy ? "Creating..." : share ? "Re-generate link" : "Create share link"}
        </Button>
        {share && (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => {
              void copyAgain();
            }}
          >
            {copied ? "Copied" : "Copy URL"}
          </Button>
        )}
      </div>
      {share && (
        <div className="rounded border border-[var(--border-primary)] bg-[var(--bg-tertiary)] px-3 py-2 text-xs">
          <div className="mb-1 text-[var(--text-secondary)]">
            Expires {new Date(share.expires_at).toLocaleString()}
            {share.redact_plates ? " - plates blurred" : " - no plate blur"}
          </div>
          <code className="break-all font-mono text-[var(--text-primary)]">
            {share.url}
          </code>
        </div>
      )}
      {error && (
        <p className="text-xs text-[color:var(--error-text,#d33)]" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
