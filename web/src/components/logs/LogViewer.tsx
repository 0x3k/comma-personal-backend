"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import type { LogEntry, LogSeverity } from "@/lib/types";

// -- Severity colour mapping ------------------------------------------------

const severityClasses: Record<LogSeverity, string> = {
  info: "text-[var(--text-secondary)]",
  warning: "text-warning-600 dark:text-warning-500",
  error: "text-danger-600 dark:text-danger-500",
};

const severityBgClasses: Record<LogSeverity, string> = {
  info: "",
  warning: "bg-warning-500/5",
  error: "bg-danger-500/5",
};

const severityLabels: Record<LogSeverity, string> = {
  info: "INF",
  warning: "WRN",
  error: "ERR",
};

// -- Props -------------------------------------------------------------------

interface LogViewerProps {
  /** Array of log entries to display. */
  logs: LogEntry[];
  /** Additional CSS classes for the outer wrapper. */
  className?: string;
}

// -- Estimated row height (px) for the virtualizer --------------------------

const ESTIMATED_ROW_HEIGHT = 28;

// -- Component ---------------------------------------------------------------

function LogViewer({ logs, className = "" }: LogViewerProps) {
  const parentRef = useRef<HTMLDivElement>(null);
  const [search, setSearch] = useState("");
  const [severityFilter, setSeverityFilter] = useState<LogSeverity | "all">(
    "all",
  );
  const [autoScroll, setAutoScroll] = useState(false);

  // Filtered entries
  const filteredLogs = useMemo(() => {
    const lowerSearch = search.toLowerCase();
    return logs.filter((entry) => {
      if (
        severityFilter !== "all" &&
        entry.severity !== severityFilter
      ) {
        return false;
      }
      if (
        lowerSearch &&
        !entry.message.toLowerCase().includes(lowerSearch) &&
        !entry.timestamp.toLowerCase().includes(lowerSearch)
      ) {
        return false;
      }
      return true;
    });
  }, [logs, search, severityFilter]);

  // Virtualizer
  const virtualizer = useVirtualizer({
    count: filteredLogs.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => ESTIMATED_ROW_HEIGHT,
    overscan: 20,
  });

  // Auto-scroll to bottom when new entries appear
  const scrollToBottom = useCallback(() => {
    if (filteredLogs.length > 0) {
      virtualizer.scrollToIndex(filteredLogs.length - 1, { align: "end" });
    }
  }, [filteredLogs.length, virtualizer]);

  useEffect(() => {
    if (autoScroll) {
      scrollToBottom();
    }
  }, [autoScroll, scrollToBottom]);

  const matchCount = filteredLogs.length;
  const totalCount = logs.length;

  return (
    <div
      className={[
        "flex flex-col rounded-lg border border-[var(--border-primary)] bg-[var(--bg-surface)]",
        className,
      ]
        .filter(Boolean)
        .join(" ")}
    >
      {/* Toolbar */}
      <div className="flex flex-col gap-2 border-b border-[var(--border-primary)] px-3 py-2 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex flex-1 items-center gap-2">
          {/* Search input */}
          <div className="relative flex-1 max-w-sm">
            <input
              type="text"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search logs..."
              className="w-full rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-2.5 py-1.5 text-xs font-mono text-[var(--text-primary)] placeholder:text-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]"
            />
            {search && (
              <button
                type="button"
                onClick={() => setSearch("")}
                className="absolute right-2 top-1/2 -translate-y-1/2 text-[var(--text-tertiary)] hover:text-[var(--text-secondary)]"
                aria-label="Clear search"
              >
                &times;
              </button>
            )}
          </div>

          {/* Severity filter */}
          <select
            value={severityFilter}
            onChange={(e) =>
              setSeverityFilter(e.target.value as LogSeverity | "all")
            }
            className="rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-2 py-1.5 text-xs text-[var(--text-primary)] focus:border-[var(--accent)] focus:outline-none focus:ring-1 focus:ring-[var(--accent)]"
          >
            <option value="all">All levels</option>
            <option value="info">Info</option>
            <option value="warning">Warning</option>
            <option value="error">Error</option>
          </select>
        </div>

        <div className="flex items-center gap-3 text-xs text-[var(--text-secondary)]">
          <span>
            {matchCount === totalCount
              ? `${totalCount} entries`
              : `${matchCount} / ${totalCount} entries`}
          </span>

          {/* Auto-scroll toggle */}
          <label className="flex items-center gap-1.5 cursor-pointer select-none">
            <input
              type="checkbox"
              checked={autoScroll}
              onChange={(e) => setAutoScroll(e.target.checked)}
              className="h-3.5 w-3.5 rounded border-[var(--border-secondary)] accent-[var(--accent)]"
            />
            Auto-scroll
          </label>
        </div>
      </div>

      {/* Virtual scroll container */}
      <div
        ref={parentRef}
        className="overflow-auto"
        style={{ height: "clamp(200px, 50vh, 600px)" }}
      >
        {filteredLogs.length === 0 ? (
          <div className="flex items-center justify-center py-12 text-sm text-[var(--text-tertiary)]">
            {logs.length === 0 ? "No log entries." : "No entries match the current filter."}
          </div>
        ) : (
          <div
            style={{
              height: `${virtualizer.getTotalSize()}px`,
              width: "100%",
              position: "relative",
            }}
          >
            {virtualizer.getVirtualItems().map((virtualRow) => {
              const entry = filteredLogs[virtualRow.index];
              return (
                <div
                  key={entry.id}
                  data-index={virtualRow.index}
                  ref={virtualizer.measureElement}
                  style={{
                    position: "absolute",
                    top: 0,
                    left: 0,
                    width: "100%",
                    transform: `translateY(${virtualRow.start}px)`,
                  }}
                  className={[
                    "flex items-baseline gap-2 px-3 py-0.5 font-mono text-xs leading-relaxed",
                    severityBgClasses[entry.severity],
                    "hover:bg-[var(--bg-tertiary)]",
                  ].join(" ")}
                >
                  {/* Timestamp */}
                  <span className="shrink-0 text-[var(--text-tertiary)] tabular-nums">
                    {entry.timestamp}
                  </span>

                  {/* Severity badge */}
                  <span
                    className={[
                      "shrink-0 w-8 text-center font-semibold",
                      severityClasses[entry.severity],
                    ].join(" ")}
                  >
                    {severityLabels[entry.severity]}
                  </span>

                  {/* Message */}
                  <span className="text-[var(--text-primary)] whitespace-pre-wrap break-all min-w-0">
                    {search
                      ? highlightMatch(entry.message, search)
                      : entry.message}
                  </span>
                </div>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}

// -- Search highlight helper ------------------------------------------------

function highlightMatch(text: string, query: string): React.ReactNode {
  if (!query) return text;
  const lowerText = text.toLowerCase();
  const lowerQuery = query.toLowerCase();
  const parts: React.ReactNode[] = [];
  let cursor = 0;

  while (cursor < text.length) {
    const idx = lowerText.indexOf(lowerQuery, cursor);
    if (idx === -1) {
      parts.push(text.slice(cursor));
      break;
    }
    if (idx > cursor) {
      parts.push(text.slice(cursor, idx));
    }
    parts.push(
      <mark
        key={idx}
        className="bg-warning-500/30 text-[var(--text-primary)] rounded-sm"
      >
        {text.slice(idx, idx + query.length)}
      </mark>,
    );
    cursor = idx + query.length;
  }

  return parts;
}

export { LogViewer, type LogViewerProps };
