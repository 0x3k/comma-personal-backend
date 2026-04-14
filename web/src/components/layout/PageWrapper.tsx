import type { ReactNode } from "react";

interface PageWrapperProps {
  children: ReactNode;
  /** Optional page title rendered as a heading. */
  title?: string;
  /** Optional description below the title. */
  description?: string;
  /** Extra classes on the outer wrapper. */
  className?: string;
}

function PageWrapper({
  children,
  title,
  description,
  className = "",
}: PageWrapperProps) {
  return (
    <main
      className={[
        "mx-auto w-full max-w-7xl flex-1 px-4 py-6 sm:px-6 lg:px-8",
        className,
      ]
        .filter(Boolean)
        .join(" ")}
    >
      {(title || description) && (
        <div className="mb-6">
          {title && <h1 className="text-heading">{title}</h1>}
          {description && (
            <p className="mt-1 text-caption">{description}</p>
          )}
        </div>
      )}
      {children}
    </main>
  );
}

export { PageWrapper, type PageWrapperProps };
