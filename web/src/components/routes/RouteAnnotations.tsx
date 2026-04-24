"use client";

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ChangeEvent,
  type KeyboardEvent,
} from "react";
import { apiFetch } from "@/lib/api";
import type { DeviceTagsResponse } from "@/lib/types";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";
import { Button } from "@/components/ui/Button";
import { Toast } from "@/components/ui/Toast";

/**
 * Maximum free-form note length, matching the CHECK constraint in
 * internal/api/route_annotations.go. We enforce it in the UI so the user
 * gets immediate feedback instead of a 400 from the server on Save.
 */
const MAX_NOTE_LEN = 4000;

/**
 * Max tag length matching internal/api/route_annotations.go. The server
 * rejects tags outside [1, 32] after normalization; we cap the input so
 * the user can't even type a value that will fail.
 */
const MAX_TAG_LEN = 32;

/**
 * normalizeTag mirrors the server-side trim + lowercase normalization so
 * the client's dedupe logic agrees with the database's unique constraint.
 * Kept local to this file to avoid a circular dep with FilterBar.
 */
function normalizeTag(raw: string): string {
  return raw.trim().toLowerCase();
}

export interface RouteAnnotationsProps {
  dongleId: string;
  routeName: string;
  /** Initial starred state from the route detail response. */
  starred: boolean;
  /** Initial note text from the route detail response. */
  note: string;
  /** Initial tag set from the route detail response. */
  tags: string[];
  /**
   * When true the editors render as plain text. Used for JWT-only or
   * share-link viewers where mutations would 401. The route detail page
   * is session-gated in practice, so this defaults to false; callers
   * explicitly flip it when they know the viewer can't write.
   */
  readOnly?: boolean;
}

/**
 * RouteAnnotations is the star + note + tags editor rendered on the
 * route detail page. It owns local copies of all three annotation
 * fields and writes through the PUT endpoints defined in
 * internal/api/route_annotations.go.
 *
 * Optimistic UI: star toggles and tag edits mutate local state first so
 * the UI feels instant; on network failure we roll the local copy back
 * and surface the error via a Toast (for brief successes) or an inline
 * banner (for the persistent failure view). The note is an explicit
 * Save button so accidental reloads don't clobber a half-written note.
 */
export function RouteAnnotations({
  dongleId,
  routeName,
  starred: initialStarred,
  note: initialNote,
  tags: initialTags,
  readOnly = false,
}: RouteAnnotationsProps) {
  // Confirmed state is what the server acknowledged. Pending state is
  // what the UI shows while a mutation is in flight so we can roll back
  // to "confirmed" on error without asking the server again.
  const [starred, setStarred] = useState(initialStarred);
  const [starPending, setStarPending] = useState(false);

  const [tags, setTags] = useState<string[]>(initialTags);
  const [tagInput, setTagInput] = useState("");
  const [tagsSaving, setTagsSaving] = useState(false);

  const [note, setNote] = useState(initialNote);
  const [noteDraft, setNoteDraft] = useState(initialNote);
  const [noteSaving, setNoteSaving] = useState(false);
  const [noteExpanded, setNoteExpanded] = useState(initialNote.length > 0);

  const [toast, setToast] = useState<{
    variant: "success" | "error";
    message: string;
  } | null>(null);

  const [errorBanner, setErrorBanner] = useState<string | null>(null);

  // Device-wide tag catalog for the autocomplete list. Loaded once per
  // mount; the backend endpoint is cheap (a single DISTINCT query) so
  // refetching on every add/remove would be wasteful. Failure is
  // non-fatal -- the user can still type any tag, they just won't get
  // suggestions.
  const [catalog, setCatalog] = useState<string[]>([]);

  useEffect(() => {
    if (readOnly) return;
    let cancelled = false;
    (async () => {
      try {
        const data = await apiFetch<DeviceTagsResponse>(
          `/v1/devices/${encodeURIComponent(dongleId)}/tags`,
        );
        if (!cancelled) setCatalog(data.tags ?? []);
      } catch {
        if (!cancelled) setCatalog([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [dongleId, readOnly]);

  // Keep local state in sync if the parent refetches the route (e.g.
  // after a Retry). We compare against the previous initial copy via the
  // identity of the array / primitive because React's default Object.is
  // check won't fire on an array with the same contents.
  const lastInitialRef = useRef({ initialStarred, initialNote, initialTags });
  useEffect(() => {
    const prev = lastInitialRef.current;
    if (prev.initialStarred !== initialStarred) setStarred(initialStarred);
    if (prev.initialNote !== initialNote) {
      setNote(initialNote);
      setNoteDraft(initialNote);
    }
    if (
      prev.initialTags.length !== initialTags.length ||
      prev.initialTags.some((t, i) => t !== initialTags[i])
    ) {
      setTags(initialTags);
    }
    lastInitialRef.current = { initialStarred, initialNote, initialTags };
  }, [initialStarred, initialNote, initialTags]);

  const notifyError = useCallback((message: string) => {
    setToast({ variant: "error", message });
    setErrorBanner(message);
  }, []);

  const notifySuccess = useCallback((message: string) => {
    setToast({ variant: "success", message });
    setErrorBanner(null);
  }, []);

  // --- Star toggle --------------------------------------------------------

  const onToggleStar = useCallback(async () => {
    if (readOnly || starPending) return;
    const previous = starred;
    const next = !starred;
    // Optimistic: flip immediately, roll back on failure.
    setStarred(next);
    setStarPending(true);
    try {
      await apiFetch(
        `/v1/routes/${encodeURIComponent(dongleId)}/${encodeURIComponent(routeName)}/starred`,
        {
          method: "PUT",
          body: { starred: next },
        },
      );
      // No body on 204; success is implicit.
    } catch (err) {
      setStarred(previous);
      const msg =
        err instanceof Error ? err.message : "Failed to update star";
      notifyError(msg);
    } finally {
      setStarPending(false);
    }
  }, [dongleId, routeName, readOnly, starPending, starred, notifyError]);

  // --- Tags ---------------------------------------------------------------

  const saveTags = useCallback(
    async (next: string[], previous: string[]) => {
      setTagsSaving(true);
      try {
        await apiFetch(
          `/v1/routes/${encodeURIComponent(dongleId)}/${encodeURIComponent(routeName)}/tags`,
          {
            method: "PUT",
            body: { tags: next },
          },
        );
      } catch (err) {
        setTags(previous);
        const msg =
          err instanceof Error ? err.message : "Failed to update tags";
        notifyError(msg);
      } finally {
        setTagsSaving(false);
      }
    },
    [dongleId, routeName, notifyError],
  );

  const commitTagInput = useCallback(() => {
    if (readOnly) return;
    const raw = tagInput;
    if (!raw.trim()) {
      setTagInput("");
      return;
    }
    const tag = normalizeTag(raw);
    if (tag.length < 1 || tag.length > MAX_TAG_LEN) {
      setTagInput("");
      notifyError(
        `Tag must be between 1 and ${MAX_TAG_LEN} characters after trimming`,
      );
      return;
    }
    // Duplicate after normalization -- silently collapse per spec.
    if (tags.includes(tag)) {
      setTagInput("");
      return;
    }
    const next = [...tags, tag].sort((a, b) => a.localeCompare(b));
    const previous = tags;
    setTags(next);
    setTagInput("");
    void saveTags(next, previous);
  }, [readOnly, tagInput, tags, saveTags, notifyError]);

  const removeTag = useCallback(
    (tag: string) => {
      if (readOnly) return;
      const next = tags.filter((t) => t !== tag);
      const previous = tags;
      setTags(next);
      void saveTags(next, previous);
    },
    [readOnly, tags, saveTags],
  );

  const onTagInputKeyDown = useCallback(
    (e: KeyboardEvent<HTMLInputElement>) => {
      // Enter or comma commits the current token. We consume the key so
      // the comma never lands in the input and Enter doesn't submit any
      // enclosing form.
      if (e.key === "Enter" || e.key === ",") {
        e.preventDefault();
        commitTagInput();
        return;
      }
      // Backspace on an empty input removes the last tag for snappy
      // keyboard editing -- mirrors how most chip-input widgets behave.
      if (e.key === "Backspace" && tagInput === "" && tags.length > 0) {
        e.preventDefault();
        removeTag(tags[tags.length - 1]);
      }
    },
    [commitTagInput, removeTag, tagInput, tags],
  );

  // Autocomplete suggestions narrow as the user types. We filter against
  // the cached catalog minus already-selected tags so the list stays short
  // and never suggests a value that would be dropped as a duplicate.
  const suggestions = useMemo(() => {
    const prefix = normalizeTag(tagInput);
    const selected = new Set(tags.map(normalizeTag));
    if (prefix === "") {
      // When the input is empty, only surface a handful of the most
      // common tags so the listbox doesn't push the rest of the form
      // down the page.
      return catalog
        .filter((t) => !selected.has(t))
        .slice(0, 8);
    }
    return catalog
      .filter((t) => t.startsWith(prefix) && !selected.has(t))
      .slice(0, 8);
  }, [catalog, tagInput, tags]);

  const [suggestionsOpen, setSuggestionsOpen] = useState(false);

  // --- Note ---------------------------------------------------------------

  const noteDirty = noteDraft !== note;
  const noteTooLong = noteDraft.length > MAX_NOTE_LEN;

  const saveNote = useCallback(async () => {
    if (readOnly || noteSaving || !noteDirty || noteTooLong) return;
    const previous = note;
    const next = noteDraft;
    // Optimistic: we flip `note` immediately so the Save button disables
    // and the "dirty" marker clears; roll back on failure.
    setNote(next);
    setNoteSaving(true);
    try {
      await apiFetch(
        `/v1/routes/${encodeURIComponent(dongleId)}/${encodeURIComponent(routeName)}/note`,
        {
          method: "PUT",
          body: { note: next },
        },
      );
      notifySuccess("Note saved");
    } catch (err) {
      setNote(previous);
      setNoteDraft(next);
      const msg =
        err instanceof Error ? err.message : "Failed to save note";
      notifyError(msg);
    } finally {
      setNoteSaving(false);
    }
  }, [
    dongleId,
    routeName,
    readOnly,
    noteSaving,
    noteDirty,
    noteTooLong,
    note,
    noteDraft,
    notifyError,
    notifySuccess,
  ]);

  // -----------------------------------------------------------------------

  return (
    <Card className="mb-6">
      <CardHeader>
        <div className="flex items-center justify-between gap-3">
          <h2 className="text-subheading text-[var(--text-primary)]">
            Annotations
          </h2>
          <StarToggleButton
            starred={starred}
            onToggle={onToggleStar}
            disabled={readOnly || starPending}
          />
        </div>
      </CardHeader>
      <CardBody>
        {errorBanner && (
          <div
            role="alert"
            className="mb-3 rounded border border-danger-500/25 bg-danger-500/5 px-3 py-2 text-xs text-danger-600 dark:text-danger-500"
          >
            {errorBanner}
          </div>
        )}

        {/* Tags editor -------------------------------------------------- */}
        <div className="mb-4">
          <div className="mb-1 flex items-center justify-between">
            <span className="text-xs font-medium text-[var(--text-secondary)]">
              Tags
            </span>
            {tagsSaving && (
              <span className="text-xs text-[var(--text-secondary)]">
                Saving...
              </span>
            )}
          </div>
          <div
            className={[
              "flex flex-wrap items-center gap-2 rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-2 py-1.5",
              readOnly ? "opacity-70" : "",
            ]
              .filter(Boolean)
              .join(" ")}
          >
            {tags.length === 0 && readOnly && (
              <span className="text-xs text-[var(--text-secondary)]">
                No tags
              </span>
            )}
            {tags.map((tag) => (
              <span
                key={tag}
                className="inline-flex items-center gap-1 rounded-full border border-[var(--border-secondary)] bg-[var(--bg-secondary)] px-2 py-0.5 text-xs text-[var(--text-primary)]"
              >
                <span>{tag}</span>
                {!readOnly && (
                  <button
                    type="button"
                    aria-label={`Remove tag ${tag}`}
                    onClick={() => removeTag(tag)}
                    className="leading-none text-[var(--text-secondary)] hover:text-[var(--text-primary)] focus:outline-none"
                  >
                    &times;
                  </button>
                )}
              </span>
            ))}

            {!readOnly && (
              <div className="relative flex-1 min-w-[10rem]">
                <input
                  type="text"
                  value={tagInput}
                  maxLength={MAX_TAG_LEN * 2}
                  onChange={(e: ChangeEvent<HTMLInputElement>) =>
                    setTagInput(e.target.value)
                  }
                  onKeyDown={onTagInputKeyDown}
                  onFocus={() => setSuggestionsOpen(true)}
                  // Delay hiding so a click on a suggestion button still
                  // fires before the list unmounts.
                  onBlur={() =>
                    window.setTimeout(() => setSuggestionsOpen(false), 100)
                  }
                  placeholder={
                    tags.length === 0 ? "Add a tag..." : "Add another..."
                  }
                  aria-label="Add tag"
                  className="w-full bg-transparent px-1 py-0.5 text-xs text-[var(--text-primary)] outline-none placeholder:text-[var(--text-secondary)]"
                />
                {suggestionsOpen && suggestions.length > 0 && (
                  <ul
                    role="listbox"
                    aria-label="Tag suggestions"
                    className="absolute left-0 right-0 top-full z-10 mt-1 max-h-48 overflow-auto rounded border border-[var(--border-secondary)] bg-[var(--bg-surface)] shadow-sm"
                  >
                    {suggestions.map((s) => (
                      <li key={s}>
                        <button
                          type="button"
                          // onMouseDown fires before onBlur so the input
                          // doesn't lose focus + hide the list before
                          // the click commits.
                          onMouseDown={(e) => {
                            e.preventDefault();
                            const previous = tags;
                            if (tags.includes(s)) {
                              setTagInput("");
                              return;
                            }
                            const next = [...tags, s].sort((a, b) =>
                              a.localeCompare(b),
                            );
                            setTags(next);
                            setTagInput("");
                            void saveTags(next, previous);
                          }}
                          className="block w-full px-2 py-1 text-left text-xs text-[var(--text-primary)] hover:bg-[var(--bg-tertiary)]"
                        >
                          {s}
                        </button>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            )}
          </div>
          {!readOnly && (
            <p className="mt-1 text-xs text-[var(--text-secondary)]">
              Press Enter or comma to add. Tags are lower-cased and
              de-duplicated automatically.
            </p>
          )}
        </div>

        {/* Note editor -------------------------------------------------- */}
        <div>
          <div className="mb-1 flex items-center justify-between">
            <span className="text-xs font-medium text-[var(--text-secondary)]">
              Note
            </span>
            {!readOnly && (note.length > 0 || noteExpanded) && (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setNoteExpanded((v) => !v)}
                aria-expanded={noteExpanded}
                aria-controls="route-note-editor"
              >
                {noteExpanded ? "Collapse" : "Edit"}
              </Button>
            )}
            {!readOnly && note.length === 0 && !noteExpanded && (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setNoteExpanded(true)}
              >
                Add note
              </Button>
            )}
          </div>

          {/* When collapsed we show the saved text (or a muted
              placeholder for empty); when expanded we show the textarea
              + Save button. Read-only mode always shows the saved copy
              as plain text, never the textarea. */}
          {readOnly ? (
            <p className="whitespace-pre-wrap rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-3 py-2 text-sm text-[var(--text-primary)]">
              {note.length > 0 ? note : (
                <span className="text-[var(--text-secondary)]">No note</span>
              )}
            </p>
          ) : noteExpanded ? (
            <div id="route-note-editor">
              <textarea
                value={noteDraft}
                onChange={(e: ChangeEvent<HTMLTextAreaElement>) =>
                  setNoteDraft(e.target.value)
                }
                rows={4}
                placeholder="Add a note about this route..."
                className="w-full rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-3 py-2 text-sm text-[var(--text-primary)] outline-none focus:border-[var(--accent)] focus:ring-1 focus:ring-[var(--accent)]"
              />
              <div className="mt-2 flex items-center justify-between">
                <span
                  className={[
                    "text-xs",
                    noteTooLong
                      ? "text-danger-600 dark:text-danger-500"
                      : "text-[var(--text-secondary)]",
                  ].join(" ")}
                >
                  {noteDraft.length} / {MAX_NOTE_LEN}
                </span>
                <div className="flex items-center gap-2">
                  {noteDirty && (
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => setNoteDraft(note)}
                      disabled={noteSaving}
                    >
                      Cancel
                    </Button>
                  )}
                  <Button
                    variant="primary"
                    size="sm"
                    onClick={() => {
                      void saveNote();
                    }}
                    disabled={noteSaving || !noteDirty || noteTooLong}
                  >
                    {noteSaving ? "Saving..." : "Save"}
                  </Button>
                </div>
              </div>
            </div>
          ) : note.length > 0 ? (
            <p className="whitespace-pre-wrap rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-3 py-2 text-sm text-[var(--text-primary)]">
              {note}
            </p>
          ) : (
            <p className="text-xs text-[var(--text-secondary)]">
              No note yet.
            </p>
          )}
        </div>
      </CardBody>
      {toast && (
        <div className="px-4 pb-3">
          <Toast
            message={toast.message}
            variant={toast.variant}
            onDismiss={() => setToast(null)}
          />
        </div>
      )}
    </Card>
  );
}

interface StarToggleButtonProps {
  starred: boolean;
  onToggle: () => void;
  disabled?: boolean;
}

/**
 * Star icon button shared with the route cards. Filled amber when
 * starred, hollow otherwise. The SVG is inlined because a single glyph
 * isn't worth adding an icon-set dependency for.
 */
export function StarToggleButton({
  starred,
  onToggle,
  disabled,
}: StarToggleButtonProps) {
  return (
    <button
      type="button"
      onClick={onToggle}
      disabled={disabled}
      aria-pressed={starred}
      aria-label={starred ? "Unstar route" : "Star route"}
      title={starred ? "Unstar" : "Star"}
      className={[
        "inline-flex items-center justify-center rounded p-1.5 transition-colors",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]",
        "disabled:opacity-50 disabled:cursor-not-allowed",
        starred
          ? "text-warning-500 hover:text-warning-600"
          : "text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-tertiary)]",
      ].join(" ")}
    >
      <StarIcon filled={starred} />
    </button>
  );
}

interface StarIconProps {
  filled: boolean;
  className?: string;
}

/**
 * Small star glyph. The filled variant colours the interior via `fill`
 * while the hollow variant only strokes the outline, matching the
 * visual language of the rest of the dashboard (Badge, Toast, etc).
 */
export function StarIcon({ filled, className = "h-5 w-5" }: StarIconProps) {
  if (filled) {
    return (
      <svg
        viewBox="0 0 24 24"
        fill="currentColor"
        stroke="currentColor"
        strokeWidth={1.5}
        strokeLinejoin="round"
        aria-hidden="true"
        className={className}
      >
        <path d="M12 2.75l2.69 5.45 6.02.87-4.36 4.25 1.03 5.99L12 16.98l-5.38 2.83 1.03-5.99L3.29 9.57l6.02-.87L12 2.75z" />
      </svg>
    );
  }
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.6}
      strokeLinejoin="round"
      strokeLinecap="round"
      aria-hidden="true"
      className={className}
    >
      <path d="M12 2.75l2.69 5.45 6.02.87-4.36 4.25 1.03 5.99L12 16.98l-5.38 2.83 1.03-5.99L3.29 9.57l6.02-.87L12 2.75z" />
    </svg>
  );
}
