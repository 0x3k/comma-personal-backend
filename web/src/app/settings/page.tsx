"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { apiFetch, BASE_URL } from "@/lib/api";
import type { DeviceParam } from "@/lib/types";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";

/**
 * Default dongle ID used when none is configured.
 * Set NEXT_PUBLIC_DONGLE_ID to target a specific device.
 */
const DONGLE_ID = process.env.NEXT_PUBLIC_DONGLE_ID ?? "default";

/** Polling interval for checking device connectivity (ms). */
const CONNECTION_POLL_MS = 10_000;

// -- Connection status hook ---------------------------------------------------

type ConnectionStatus = "online" | "offline" | "checking";

function useConnectionStatus(): ConnectionStatus {
  const [status, setStatus] = useState<ConnectionStatus>("checking");

  useEffect(() => {
    let cancelled = false;

    async function check() {
      try {
        const resp = await fetch(`${BASE_URL}/health`, { method: "GET" });
        if (!cancelled) setStatus(resp.ok ? "online" : "offline");
      } catch {
        if (!cancelled) setStatus("offline");
      }
    }

    void check();
    const id = setInterval(() => void check(), CONNECTION_POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, []);

  return status;
}

// -- Inline editable value ----------------------------------------------------

interface EditableCellProps {
  value: string;
  onSave: (newValue: string) => void;
  disabled?: boolean;
}

function EditableCell({ value, onSave, disabled }: EditableCellProps) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(value);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    setDraft(value);
  }, [value]);

  useEffect(() => {
    if (editing) {
      inputRef.current?.focus();
      inputRef.current?.select();
    }
  }, [editing]);

  function commit() {
    setEditing(false);
    const trimmed = draft.trim();
    if (trimmed !== value) {
      onSave(trimmed);
    }
  }

  if (editing) {
    return (
      <input
        ref={inputRef}
        type="text"
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={commit}
        onKeyDown={(e) => {
          if (e.key === "Enter") commit();
          if (e.key === "Escape") {
            setDraft(value);
            setEditing(false);
          }
        }}
        disabled={disabled}
        className="w-full rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-2 py-1 text-sm text-[var(--text-primary)] outline-none focus:ring-2 focus:ring-[var(--ring-focus)]"
      />
    );
  }

  return (
    <button
      type="button"
      onClick={() => setEditing(true)}
      className="w-full cursor-pointer rounded px-2 py-1 text-left text-sm text-[var(--text-primary)] hover:bg-[var(--bg-tertiary)] transition-colors"
      title="Click to edit"
    >
      {value || <span className="text-[var(--text-tertiary)] italic">empty</span>}
    </button>
  );
}

// -- Add param form -----------------------------------------------------------

interface AddParamFormProps {
  onAdd: (key: string, value: string) => void;
  disabled?: boolean;
  existingKeys: Set<string>;
}

function AddParamForm({ onAdd, disabled, existingKeys }: AddParamFormProps) {
  const [key, setKey] = useState("");
  const [value, setValue] = useState("");
  const [error, setError] = useState<string | null>(null);

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    const trimmedKey = key.trim();
    const trimmedValue = value.trim();

    if (!trimmedKey) {
      setError("Key is required");
      return;
    }
    if (existingKeys.has(trimmedKey)) {
      setError("Key already exists. Edit the existing parameter instead.");
      return;
    }

    setError(null);
    onAdd(trimmedKey, trimmedValue);
    setKey("");
    setValue("");
  }

  return (
    <form onSubmit={handleSubmit} className="flex flex-col gap-3 sm:flex-row sm:items-end">
      <div className="flex-1">
        <label
          htmlFor="new-param-key"
          className="mb-1 block text-sm font-medium text-[var(--text-secondary)]"
        >
          Key
        </label>
        <input
          id="new-param-key"
          type="text"
          value={key}
          onChange={(e) => {
            setKey(e.target.value);
            setError(null);
          }}
          placeholder="e.g. OpenpilotEnabledToggle"
          disabled={disabled}
          className="w-full rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-3 py-1.5 text-sm text-[var(--text-primary)] outline-none focus:ring-2 focus:ring-[var(--ring-focus)]"
        />
      </div>
      <div className="flex-1">
        <label
          htmlFor="new-param-value"
          className="mb-1 block text-sm font-medium text-[var(--text-secondary)]"
        >
          Value
        </label>
        <input
          id="new-param-value"
          type="text"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          placeholder="e.g. 1"
          disabled={disabled}
          className="w-full rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-3 py-1.5 text-sm text-[var(--text-primary)] outline-none focus:ring-2 focus:ring-[var(--ring-focus)]"
        />
      </div>
      <Button type="submit" size="md" disabled={disabled}>
        Add Parameter
      </Button>
      {error && (
        <p className="text-xs text-danger-500 sm:self-center">{error}</p>
      )}
    </form>
  );
}

// -- Delete confirmation dialog -----------------------------------------------

interface DeleteConfirmProps {
  paramKey: string;
  onConfirm: () => void;
  onCancel: () => void;
}

function DeleteConfirm({ paramKey, onConfirm, onCancel }: DeleteConfirmProps) {
  return (
    <div className="flex items-center gap-2">
      <span className="text-xs text-[var(--text-secondary)]">
        Delete <strong>{paramKey}</strong>?
      </span>
      <Button variant="danger" size="sm" onClick={onConfirm}>
        Delete
      </Button>
      <Button variant="ghost" size="sm" onClick={onCancel}>
        Cancel
      </Button>
    </div>
  );
}

// -- Main page ----------------------------------------------------------------

export default function SettingsPage() {
  const [params, setParams] = useState<DeviceParam[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);

  const connectionStatus = useConnectionStatus();

  const fetchParams = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await apiFetch<DeviceParam[]>(
        `/v1/devices/${DONGLE_ID}/params`,
      );
      setParams(data);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load parameters");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void fetchParams();
  }, [fetchParams]);

  async function handleSave(key: string, value: string) {
    setSaving(true);
    try {
      const updated = await apiFetch<DeviceParam>(
        `/v1/devices/${DONGLE_ID}/params/${encodeURIComponent(key)}`,
        { method: "PUT", body: { value } },
      );
      setParams((prev) =>
        prev.map((p) => (p.key === key ? updated : p)),
      );
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to save parameter");
    } finally {
      setSaving(false);
    }
  }

  async function handleAdd(key: string, value: string) {
    setSaving(true);
    try {
      const created = await apiFetch<DeviceParam>(
        `/v1/devices/${DONGLE_ID}/params/${encodeURIComponent(key)}`,
        { method: "PUT", body: { value } },
      );
      setParams((prev) => [...prev, created]);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to add parameter");
    } finally {
      setSaving(false);
    }
  }

  async function handleDelete(key: string) {
    setSaving(true);
    setDeleteTarget(null);
    try {
      await apiFetch<void>(
        `/v1/devices/${DONGLE_ID}/params/${encodeURIComponent(key)}`,
        { method: "DELETE" },
      );
      setParams((prev) => prev.filter((p) => p.key !== key));
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to delete parameter");
    } finally {
      setSaving(false);
    }
  }

  const existingKeys = new Set(params.map((p) => p.key));

  const connectionBadge = {
    online: { variant: "success" as const, label: "Connected" },
    offline: { variant: "error" as const, label: "Disconnected" },
    checking: { variant: "neutral" as const, label: "Checking..." },
  }[connectionStatus];

  return (
    <PageWrapper
      title="Settings"
      description={`Device configuration for ${DONGLE_ID}`}
    >
      {/* Connection status */}
      <div className="mb-6 flex items-center gap-3">
        <span className="text-sm font-medium text-[var(--text-secondary)]">
          Backend Status
        </span>
        <Badge variant={connectionBadge.variant}>{connectionBadge.label}</Badge>
      </div>

      {/* Error banner */}
      {error && (
        <div className="mb-4">
          <ErrorMessage
            title="Operation failed"
            message={error}
            retry={() => setError(null)}
          />
        </div>
      )}

      {/* Loading state */}
      {loading && (
        <div className="flex items-center justify-center py-16">
          <Spinner size="lg" label="Loading parameters" />
        </div>
      )}

      {/* Params table */}
      {!loading && (
        <Card>
          <CardHeader>
            <div className="flex items-center justify-between">
              <h2 className="text-subheading">Device Parameters</h2>
              <Badge variant="info">{params.length}</Badge>
            </div>
          </CardHeader>
          <CardBody className="p-0">
            {params.length === 0 ? (
              <p className="px-4 py-8 text-center text-caption">
                No parameters configured. Add one below.
              </p>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-[var(--border-primary)]">
                      <th className="px-4 py-2 text-left font-medium text-[var(--text-secondary)]">
                        Key
                      </th>
                      <th className="px-4 py-2 text-left font-medium text-[var(--text-secondary)]">
                        Value
                      </th>
                      <th className="px-4 py-2 text-right font-medium text-[var(--text-secondary)]">
                        Actions
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {params.map((param) => (
                      <tr
                        key={param.key}
                        className="border-b border-[var(--border-primary)] last:border-b-0"
                      >
                        <td className="px-4 py-2 font-mono text-xs text-[var(--text-primary)]">
                          {param.key}
                        </td>
                        <td className="px-4 py-1">
                          <EditableCell
                            value={param.value}
                            onSave={(v) => void handleSave(param.key, v)}
                            disabled={saving}
                          />
                        </td>
                        <td className="px-4 py-2 text-right">
                          {deleteTarget === param.key ? (
                            <DeleteConfirm
                              paramKey={param.key}
                              onConfirm={() => void handleDelete(param.key)}
                              onCancel={() => setDeleteTarget(null)}
                            />
                          ) : (
                            <Button
                              variant="ghost"
                              size="sm"
                              onClick={() => setDeleteTarget(param.key)}
                              disabled={saving}
                            >
                              Delete
                            </Button>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </CardBody>
        </Card>
      )}

      {/* Add new param */}
      {!loading && (
        <Card className="mt-4">
          <CardHeader>
            <h2 className="text-subheading">Add Parameter</h2>
          </CardHeader>
          <CardBody>
            <AddParamForm
              onAdd={(k, v) => void handleAdd(k, v)}
              disabled={saving}
              existingKeys={existingKeys}
            />
          </CardBody>
        </Card>
      )}
    </PageWrapper>
  );
}
