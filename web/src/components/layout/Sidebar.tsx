"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";

interface SidebarLink {
  href: string;
  label: string;
  icon?: ReactNode;
}

interface SidebarProps {
  links: SidebarLink[];
  className?: string;
}

function Sidebar({ links, className = "" }: SidebarProps) {
  const pathname = usePathname();

  return (
    <aside
      className={[
        "w-56 shrink-0 border-r border-[var(--border-primary)] bg-[var(--bg-secondary)]",
        "hidden lg:block",
        className,
      ]
        .filter(Boolean)
        .join(" ")}
    >
      <nav className="flex flex-col gap-0.5 p-3">
        {links.map((link) => {
          const isActive =
            pathname === link.href || pathname.startsWith(link.href + "/");
          return (
            <Link
              key={link.href}
              href={link.href}
              className={[
                "flex items-center gap-2 rounded-md px-3 py-2 text-sm font-medium transition-colors",
                isActive
                  ? "bg-[var(--bg-tertiary)] text-[var(--text-primary)]"
                  : "text-[var(--text-secondary)] hover:bg-[var(--bg-tertiary)] hover:text-[var(--text-primary)]",
              ].join(" ")}
            >
              {link.icon}
              {link.label}
            </Link>
          );
        })}
      </nav>
    </aside>
  );
}

export { Sidebar, type SidebarProps, type SidebarLink };
