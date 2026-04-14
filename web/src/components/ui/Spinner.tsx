import type { HTMLAttributes } from "react";

type SpinnerSize = "sm" | "md" | "lg";

interface SpinnerProps extends HTMLAttributes<HTMLDivElement> {
  size?: SpinnerSize;
  label?: string;
}

const sizeStyles: Record<SpinnerSize, string> = {
  sm: "h-4 w-4 border-2",
  md: "h-6 w-6 border-2",
  lg: "h-10 w-10 border-3",
};

function Spinner({ size = "md", label, className = "", ...rest }: SpinnerProps) {
  return (
    <div
      className={["flex flex-col items-center gap-2", className]
        .filter(Boolean)
        .join(" ")}
      {...rest}
    >
      <div
        className={[
          "animate-spin rounded-full border-[var(--border-secondary)] border-t-[var(--accent)]",
          sizeStyles[size],
        ].join(" ")}
        role="status"
        aria-label={label ?? "Loading"}
      />
      {label && (
        <span className="text-caption">{label}</span>
      )}
    </div>
  );
}

export { Spinner, type SpinnerProps, type SpinnerSize };
