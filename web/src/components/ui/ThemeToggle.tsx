"use client";

import { useCallback, useEffect, useState } from "react";

/**
 * Three-state theme preference persisted in localStorage. "system" follows
 * the OS's prefers-color-scheme and tracks changes to it; "light"/"dark"
 * are explicit overrides. Mirrors the value read by the pre-hydration
 * script in app/layout.tsx -- keep these two in sync.
 */
export type ThemePreference = "system" | "light" | "dark";

const THEME_STORAGE_KEY = "theme";

const THEMES: readonly ThemePreference[] = ["system", "light", "dark"] as const;

function isThemePreference(value: unknown): value is ThemePreference {
  return value === "system" || value === "light" || value === "dark";
}

function readStoredPreference(): ThemePreference {
  if (typeof window === "undefined") return "system";
  try {
    const raw = window.localStorage.getItem(THEME_STORAGE_KEY);
    if (isThemePreference(raw)) return raw;
  } catch {
    // localStorage can throw in private-mode / sandboxed contexts. Fall through.
  }
  return "system";
}

function prefersDark(): boolean {
  if (typeof window === "undefined") return false;
  return window.matchMedia("(prefers-color-scheme: dark)").matches;
}

/**
 * Apply the given preference to the <html> element. Always writes either
 * `light` or `dark` (never both, never neither) so the CSS in globals.css
 * has a deterministic target and there is no flash on re-render.
 */
function applyPreference(pref: ThemePreference): void {
  if (typeof document === "undefined") return;
  const html = document.documentElement;
  const effective = pref === "system" ? (prefersDark() ? "dark" : "light") : pref;
  html.classList.toggle("dark", effective === "dark");
  html.classList.toggle("light", effective === "light");
}

function nextPreference(pref: ThemePreference): ThemePreference {
  const idx = THEMES.indexOf(pref);
  return THEMES[(idx + 1) % THEMES.length];
}

function SunIcon() {
  return (
    <svg
      viewBox="0 0 20 20"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
      className="h-4 w-4"
      aria-hidden="true"
    >
      <circle cx="10" cy="10" r="3.25" />
      <path d="M10 2.5v1.5M10 16v1.5M2.5 10H4M16 10h1.5M4.7 4.7l1.05 1.05M14.25 14.25l1.05 1.05M4.7 15.3l1.05-1.05M14.25 5.75l1.05-1.05" />
    </svg>
  );
}

function MoonIcon() {
  return (
    <svg
      viewBox="0 0 20 20"
      fill="currentColor"
      className="h-4 w-4"
      aria-hidden="true"
    >
      <path d="M15.5 12.25a6 6 0 01-7.75-7.75.5.5 0 00-.66-.6A7.5 7.5 0 1016.1 12.91a.5.5 0 00-.6-.66z" />
    </svg>
  );
}

function SystemIcon() {
  return (
    <svg
      viewBox="0 0 20 20"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
      className="h-4 w-4"
      aria-hidden="true"
    >
      <rect x="3" y="4" width="14" height="10" rx="1.5" />
      <path d="M7 17h6M10 14v3" />
    </svg>
  );
}

const ICONS: Record<ThemePreference, () => React.JSX.Element> = {
  system: SystemIcon,
  light: SunIcon,
  dark: MoonIcon,
};

const LABELS: Record<ThemePreference, string> = {
  system: "System",
  light: "Light",
  dark: "Dark",
};

interface ThemeToggleProps {
  className?: string;
  /** When true, renders the label text next to the icon (default: icon-only). */
  showLabel?: boolean;
}

/**
 * Cycling three-state theme toggle (system -> light -> dark -> system).
 *
 * On mount the component hydrates its state from localStorage so the button
 * label reflects the user's current choice. While rendering server-side and
 * during the very first client render we intentionally return a neutral
 * placeholder: any text we guessed would mismatch once we read storage and
 * React would warn. The pre-hydration script in app/layout.tsx already set
 * the html class, so the page is visually correct either way.
 */
function ThemeToggle({ className = "", showLabel = false }: ThemeToggleProps) {
  const [mounted, setMounted] = useState(false);
  const [preference, setPreference] = useState<ThemePreference>("system");

  useEffect(() => {
    setPreference(readStoredPreference());
    setMounted(true);
  }, []);

  // When the user is on "system", live-update the html class as the OS theme
  // changes so the UI does not get stuck on the value that was active at load.
  useEffect(() => {
    if (typeof window === "undefined") return undefined;
    if (preference !== "system") return undefined;
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const listener = () => applyPreference("system");
    mq.addEventListener("change", listener);
    return () => mq.removeEventListener("change", listener);
  }, [preference]);

  const cycle = useCallback(() => {
    setPreference((current) => {
      const next = nextPreference(current);
      try {
        window.localStorage.setItem(THEME_STORAGE_KEY, next);
      } catch {
        // Ignore storage failures (private mode); the class still applies.
      }
      applyPreference(next);
      return next;
    });
  }, []);

  // Before hydration we do not know the stored preference. Render a neutral
  // placeholder with suppressHydrationWarning so React does not complain
  // about the server/client text mismatch after useEffect populates state.
  const displayPref: ThemePreference = mounted ? preference : "system";
  const Icon = ICONS[displayPref];
  const label = LABELS[displayPref];
  const ariaLabel = `Theme: ${label}. Click to cycle (system, light, dark).`;

  return (
    <button
      type="button"
      onClick={cycle}
      aria-label={ariaLabel}
      title={ariaLabel}
      className={[
        "inline-flex items-center gap-1.5 rounded-md px-2 py-1.5 text-sm font-medium",
        "text-[var(--text-secondary)] transition-colors",
        "hover:bg-[var(--bg-tertiary)] hover:text-[var(--text-primary)]",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]",
        className,
      ]
        .filter(Boolean)
        .join(" ")}
      suppressHydrationWarning
    >
      <Icon />
      {showLabel && <span suppressHydrationWarning>{label}</span>}
    </button>
  );
}

export { ThemeToggle, THEME_STORAGE_KEY };
