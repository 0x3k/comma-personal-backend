"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useState } from "react";
import { BASE_URL } from "@/lib/api";

interface NavLink {
  href: string;
  label: string;
}

const navLinks: NavLink[] = [
  { href: "/routes", label: "Routes" },
  { href: "/moments", label: "Moments" },
  { href: "/devices", label: "Devices" },
  { href: "/settings", label: "Settings" },
];

function Navbar() {
  const pathname = usePathname();
  const isHomeActive = pathname === "/";
  const isLoginPage = pathname === "/login";
  const [loggingOut, setLoggingOut] = useState(false);

  async function handleLogout() {
    if (loggingOut) return;
    setLoggingOut(true);
    try {
      // Best-effort: even if the backend has session auth disabled (404)
      // or the request fails, we still send the user to the login page.
      // The cookie (if any) is cleared server-side on success.
      await fetch(`${BASE_URL}/v1/session/logout`, {
        method: "POST",
        credentials: "include",
      }).catch(() => undefined);
    } finally {
      // Use a full navigation so any in-memory state (React tree, caches,
      // etc.) is discarded. The login page will re-bootstrap from scratch.
      if (typeof window !== "undefined") {
        window.location.assign("/login");
      }
    }
  }

  return (
    <header className="sticky top-0 z-40 border-b border-[var(--border-primary)] bg-[var(--bg-nav)] backdrop-blur-sm">
      <div className="mx-auto flex h-14 max-w-7xl items-center justify-between px-4 sm:px-6">
        <Link
          href="/"
          className={[
            "text-base font-semibold tracking-tight transition-colors",
            isHomeActive
              ? "text-[var(--accent)]"
              : "text-[var(--text-primary)] hover:text-[var(--accent)]",
          ].join(" ")}
        >
          comma personal
        </Link>

        <nav className="flex items-center gap-1">
          {navLinks.map((link) => {
            const isActive =
              pathname === link.href || pathname.startsWith(link.href + "/");
            return (
              <Link
                key={link.href}
                href={link.href}
                className={[
                  "rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
                  isActive
                    ? "bg-[var(--bg-tertiary)] text-[var(--text-primary)]"
                    : "text-[var(--text-secondary)] hover:bg-[var(--bg-tertiary)] hover:text-[var(--text-primary)]",
                ].join(" ")}
              >
                {link.label}
              </Link>
            );
          })}

          {!isLoginPage && (
            <button
              type="button"
              onClick={() => {
                void handleLogout();
              }}
              disabled={loggingOut}
              className={[
                "ml-1 rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
                "text-[var(--text-secondary)] hover:bg-[var(--bg-tertiary)] hover:text-[var(--text-primary)]",
                "disabled:pointer-events-none disabled:opacity-50",
              ].join(" ")}
              aria-label="Log out"
            >
              {loggingOut ? "Logging out..." : "Log out"}
            </button>
          )}
        </nav>
      </div>
    </header>
  );
}

export { Navbar };
