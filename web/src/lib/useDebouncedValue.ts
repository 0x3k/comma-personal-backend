"use client";

import { useEffect, useState } from "react";

/**
 * useDebouncedValue returns a copy of `value` that only updates after the
 * caller has stopped changing the input for `delayMs` milliseconds. It is
 * intended for fetch-on-change inputs (filter bars, search boxes) where we
 * want to avoid hammering the backend while the user is still typing or
 * dragging a slider.
 *
 * The first render returns the initial value synchronously (no debounce on
 * mount) so pages can use the debounced value as a fetch input without
 * flashing an empty state.
 */
export function useDebouncedValue<T>(value: T, delayMs = 300): T {
  const [debounced, setDebounced] = useState<T>(value);

  useEffect(() => {
    const id = window.setTimeout(() => {
      setDebounced(value);
    }, delayMs);
    return () => window.clearTimeout(id);
  }, [value, delayMs]);

  return debounced;
}
