"use client";

import { useEffect } from "react";
import type { HTMLAttributes } from "react";

type ToastVariant = "success" | "error" | "info";

interface ToastProps extends HTMLAttributes<HTMLDivElement> {
  /** Message to display. */
  message: string;
  /** Visual style; defaults to "success". */
  variant?: ToastVariant;
  /** Called when the auto-dismiss timer fires or the close button is clicked. */
  onDismiss?: () => void;
  /** Auto-dismiss delay in ms. Defaults to 3000. Pass 0 to disable. */
  dismissAfterMs?: number;
}

const variantStyles: Record<ToastVariant, string> = {
  success: "border-success-500/25 bg-success-500/10 text-success-600",
  error: "border-danger-500/25 bg-danger-500/10 text-danger-600",
  info: "border-info-500/25 bg-info-500/10 text-info-600",
};

/**
 * Lightweight toast notification. Rendered inline by the caller; there is no
 * portal or provider. The host component controls visibility via conditional
 * rendering and the onDismiss callback.
 */
function Toast({
  message,
  variant = "success",
  onDismiss,
  dismissAfterMs = 3000,
  className = "",
  ...rest
}: ToastProps) {
  useEffect(() => {
    if (!dismissAfterMs || !onDismiss) return undefined;
    const id = setTimeout(onDismiss, dismissAfterMs);
    return () => clearTimeout(id);
  }, [dismissAfterMs, onDismiss]);

  return (
    <div
      role="status"
      aria-live="polite"
      className={[
        "flex items-center gap-3 rounded-md border px-3 py-2 text-sm",
        variantStyles[variant],
        className,
      ]
        .filter(Boolean)
        .join(" ")}
      {...rest}
    >
      <span className="flex-1">{message}</span>
      {onDismiss && (
        <button
          type="button"
          onClick={onDismiss}
          className="rounded px-2 py-0.5 text-xs font-medium opacity-80 hover:opacity-100"
          aria-label="Dismiss"
        >
          Dismiss
        </button>
      )}
    </div>
  );
}

export { Toast, type ToastProps, type ToastVariant };
