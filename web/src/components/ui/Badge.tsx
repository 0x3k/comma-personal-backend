import type { HTMLAttributes } from "react";

type BadgeVariant = "success" | "warning" | "error" | "info" | "neutral";

interface BadgeProps extends HTMLAttributes<HTMLSpanElement> {
  variant?: BadgeVariant;
}

const variantStyles: Record<BadgeVariant, string> = {
  success:
    "bg-success-500/15 text-success-600 dark:text-success-500",
  warning:
    "bg-warning-500/15 text-warning-600 dark:text-warning-500",
  error:
    "bg-danger-500/15 text-danger-600 dark:text-danger-500",
  info:
    "bg-info-500/15 text-info-600 dark:text-info-500",
  neutral:
    "bg-[var(--bg-tertiary)] text-[var(--text-secondary)]",
};

function Badge({
  variant = "neutral",
  className = "",
  children,
  ...rest
}: BadgeProps) {
  return (
    <span
      className={[
        "inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium",
        variantStyles[variant],
        className,
      ]
        .filter(Boolean)
        .join(" ")}
      {...rest}
    >
      {children}
    </span>
  );
}

export { Badge, type BadgeProps, type BadgeVariant };
