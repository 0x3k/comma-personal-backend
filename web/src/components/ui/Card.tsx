import type { HTMLAttributes, ReactNode } from "react";

interface CardProps extends HTMLAttributes<HTMLDivElement> {
  children: ReactNode;
}

function Card({ className = "", children, ...rest }: CardProps) {
  return (
    <div
      className={[
        "rounded-lg border border-[var(--border-primary)] bg-[var(--bg-surface)]",
        className,
      ]
        .filter(Boolean)
        .join(" ")}
      {...rest}
    >
      {children}
    </div>
  );
}

interface CardSectionProps extends HTMLAttributes<HTMLDivElement> {
  children: ReactNode;
}

function CardHeader({ className = "", children, ...rest }: CardSectionProps) {
  return (
    <div
      className={[
        "border-b border-[var(--border-primary)] px-4 py-3",
        className,
      ]
        .filter(Boolean)
        .join(" ")}
      {...rest}
    >
      {children}
    </div>
  );
}

function CardBody({ className = "", children, ...rest }: CardSectionProps) {
  return (
    <div
      className={["px-4 py-4", className].filter(Boolean).join(" ")}
      {...rest}
    >
      {children}
    </div>
  );
}

function CardFooter({ className = "", children, ...rest }: CardSectionProps) {
  return (
    <div
      className={[
        "border-t border-[var(--border-primary)] px-4 py-3",
        className,
      ]
        .filter(Boolean)
        .join(" ")}
      {...rest}
    >
      {children}
    </div>
  );
}

export { Card, CardHeader, CardBody, CardFooter, type CardProps };
