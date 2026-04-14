"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";

interface NavLink {
  href: string;
  label: string;
  /** When true, only exact pathname match counts as active. */
  exact?: boolean;
}

const navLinks: NavLink[] = [
  { href: "/routes", label: "Routes" },
  { href: "/devices", label: "Devices" },
  { href: "/settings", label: "Settings" },
];

function Navbar() {
  const pathname = usePathname();
  const isHomeActive = pathname === "/";

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
        </nav>
      </div>
    </header>
  );
}

export { Navbar };
