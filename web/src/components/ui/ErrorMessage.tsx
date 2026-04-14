import type { HTMLAttributes, ReactNode } from "react";
import { Button } from "./Button";

interface ErrorMessageProps extends HTMLAttributes<HTMLDivElement> {
  title?: string;
  message: string;
  retry?: () => void;
  children?: ReactNode;
}

function ErrorMessage({
  title = "Something went wrong",
  message,
  retry,
  children,
  className = "",
  ...rest
}: ErrorMessageProps) {
  return (
    <div
      className={[
        "rounded-lg border border-danger-500/25 bg-danger-500/5 p-4",
        className,
      ]
        .filter(Boolean)
        .join(" ")}
      role="alert"
      {...rest}
    >
      <div className="flex items-start gap-3">
        <svg
          className="mt-0.5 h-5 w-5 shrink-0 text-danger-500"
          viewBox="0 0 20 20"
          fill="currentColor"
          aria-hidden="true"
        >
          <path
            fillRule="evenodd"
            d="M10 18a8 8 0 100-16 8 8 0 000 16zM8.28 7.22a.75.75 0 00-1.06 1.06L8.94 10l-1.72 1.72a.75.75 0 101.06 1.06L10 11.06l1.72 1.72a.75.75 0 101.06-1.06L11.06 10l1.72-1.72a.75.75 0 00-1.06-1.06L10 8.94 8.28 7.22z"
            clipRule="evenodd"
          />
        </svg>
        <div className="flex-1">
          <p className="text-sm font-medium text-danger-600 dark:text-danger-500">
            {title}
          </p>
          <p className="mt-1 text-sm text-[var(--text-secondary)]">{message}</p>
          {children}
          {retry && (
            <div className="mt-3">
              <Button variant="secondary" size="sm" onClick={retry}>
                Retry
              </Button>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

export { ErrorMessage, type ErrorMessageProps };
