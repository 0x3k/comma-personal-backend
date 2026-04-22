"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { apiFetch, BASE_URL } from "@/lib/api";
import type {
  DeviceParam,
  RetentionSetting,
  StorageUsageReport,
} from "@/lib/types";
import { formatBytes } from "@/lib/format";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Spinner } from "@/components/ui/Spinner";
import { ErrorMessage } from "@/components/ui/ErrorMessage";
import { Toast } from "@/components/ui/Toast";

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

// -- Storage panel ------------------------------------------------------------

interface FilesystemBarProps {
  totalBytes: number;
  availableBytes: number;
}

/** Progress bar showing filesystem used vs. available. */
function FilesystemBar({ totalBytes, availableBytes }: FilesystemBarProps) {
  const usedBytes = Math.max(totalBytes - availableBytes, 0);
  const pct =
    totalBytes > 0 ? Math.min(100, Math.max(0, (usedBytes / totalBytes) * 100)) : 0;

  // Color shifts from brand (ok) to warning (>=75%) to danger (>=90%).
  const barColor =
    pct >= 90
      ? "bg-danger-500"
      : pct >= 75
        ? "bg-warning-500"
        : "bg-[var(--accent)]";

  return (
    <div>
      <div className="mb-1 flex items-center justify-between text-sm">
        <span className="text-[var(--text-secondary)]">Filesystem</span>
        <span className="font-mono text-xs text-[var(--text-primary)]">
          {formatBytes(usedBytes)} used / {formatBytes(totalBytes)} total (
          {formatBytes(availableBytes)} free)
        </span>
      </div>
      <div
        className="h-2 w-full overflow-hidden rounded-full bg-[var(--bg-tertiary)]"
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(pct)}
        aria-label="Filesystem usage"
      >
        <div
          className={["h-full transition-all", barColor].join(" ")}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  );
}

interface RetentionEditorProps {
  initialDays: number;
  onSaved: (days: number) => void;
}

/** Number input + Save button for the retention window. */
function RetentionEditor({ initialDays, onSaved }: RetentionEditorProps) {
  const [draft, setDraft] = useState<string>(String(initialDays));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setDraft(String(initialDays));
  }, [initialDays]);

  const parsed = Number.parseInt(draft, 10);
  const validNumber = Number.isFinite(parsed) && parsed >= 0;
  const current = Number.parseInt(String(initialDays), 10);
  const dirty = validNumber && parsed !== current;

  async function handleSave() {
    if (!validNumber) {
      setError("Enter 0 or a positive integer");
      return;
    }
    setSaving(true);
    setError(null);
    try {
      const resp = await apiFetch<RetentionSetting>(
        `/v1/settings/retention`,
        { method: "PUT", body: { retention_days: parsed } },
      );
      onSaved(resp.retention_days);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to save retention");
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-end">
        <div className="flex-1">
          <label
            htmlFor="retention-days"
            className="mb-1 block text-sm font-medium text-[var(--text-secondary)]"
          >
            Retention (days)
          </label>
          <input
            id="retention-days"
            type="number"
            min={0}
            step={1}
            value={draft}
            onChange={(e) => {
              setDraft(e.target.value);
              setError(null);
            }}
            disabled={saving}
            className="w-full rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-3 py-1.5 text-sm text-[var(--text-primary)] outline-none focus:ring-2 focus:ring-[var(--ring-focus)] sm:max-w-[12rem]"
          />
          <p className="mt-1 text-xs text-[var(--text-tertiary)]">
            {validNumber && parsed === 0
              ? "Never delete automatically"
              : "Routes older than this are eligible for deletion (preserved routes are exempt)."}
          </p>
        </div>
        <Button
          type="button"
          onClick={() => void handleSave()}
          disabled={saving || !dirty || !validNumber}
        >
          {saving ? "Saving..." : "Save"}
        </Button>
      </div>
      {error && (
        <p className="text-xs text-danger-500" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}

interface StoragePanelProps {
  /** Shows a success toast; reset by the caller via onDismissToast. */
  toast: string | null;
  onToast: (message: string) => void;
  onDismissToast: () => void;
}

function StoragePanel({ toast, onToast, onDismissToast }: StoragePanelProps) {
  const [usage, setUsage] = useState<StorageUsageReport | null>(null);
  const [usageError, setUsageError] = useState<string | null>(null);
  const [usageLoading, setUsageLoading] = useState(true);

  const [retention, setRetention] = useState<number | null>(null);
  const [retentionError, setRetentionError] = useState<string | null>(null);
  const [retentionLoading, setRetentionLoading] = useState(true);

  const fetchUsage = useCallback(async () => {
    setUsageLoading(true);
    setUsageError(null);
    try {
      const data = await apiFetch<StorageUsageReport>(`/v1/storage/usage`);
      setUsage(data);
    } catch (err) {
      setUsageError(err instanceof Error ? err.message : "Failed to load usage");
    } finally {
      setUsageLoading(false);
    }
  }, []);

  const fetchRetention = useCallback(async () => {
    setRetentionLoading(true);
    setRetentionError(null);
    try {
      const data = await apiFetch<RetentionSetting>(`/v1/settings/retention`);
      setRetention(data.retention_days);
    } catch (err) {
      setRetentionError(
        err instanceof Error ? err.message : "Failed to load retention setting",
      );
    } finally {
      setRetentionLoading(false);
    }
  }, []);

  useEffect(() => {
    void fetchUsage();
    void fetchRetention();
  }, [fetchUsage, fetchRetention]);

  return (
    <Card className="mt-4">
      <CardHeader>
        <div className="flex items-center justify-between">
          <h2 className="text-subheading">Storage</h2>
          {usage && (
            <Badge variant="info">{formatBytes(usage.totalBytes)} stored</Badge>
          )}
        </div>
      </CardHeader>
      <CardBody className="space-y-6">
        {toast && (
          <Toast
            variant="success"
            message={toast}
            onDismiss={onDismissToast}
          />
        )}

        {/* Filesystem progress bar */}
        {usageLoading && (
          <div className="flex items-center justify-center py-4">
            <Spinner size="md" label="Loading storage usage" />
          </div>
        )}
        {usageError && !usageLoading && (
          <ErrorMessage
            title="Failed to load storage usage"
            message={usageError}
            retry={() => void fetchUsage()}
          />
        )}
        {usage && !usageLoading && !usageError && (
          <FilesystemBar
            totalBytes={usage.filesystemTotalBytes}
            availableBytes={usage.filesystemAvailableBytes}
          />
        )}

        {/* Per-device byte breakdown */}
        {usage && !usageLoading && !usageError && (
          <div>
            <h3 className="mb-2 text-sm font-medium text-[var(--text-secondary)]">
              Per-device usage
            </h3>
            {usage.devices.length === 0 ? (
              <p className="text-caption">No device data stored yet.</p>
            ) : (
              <div className="overflow-x-auto rounded border border-[var(--border-primary)]">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-[var(--border-primary)]">
                      <th className="px-4 py-2 text-left font-medium text-[var(--text-secondary)]">
                        Dongle ID
                      </th>
                      <th className="px-4 py-2 text-right font-medium text-[var(--text-secondary)]">
                        Routes
                      </th>
                      <th className="px-4 py-2 text-right font-medium text-[var(--text-secondary)]">
                        Bytes
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {usage.devices.map((d) => (
                      <tr
                        key={d.dongleId}
                        className="border-b border-[var(--border-primary)] last:border-b-0"
                      >
                        <td className="px-4 py-2 font-mono text-xs text-[var(--text-primary)]">
                          {d.dongleId}
                        </td>
                        <td className="px-4 py-2 text-right text-[var(--text-primary)]">
                          {d.routeCount}
                        </td>
                        <td className="px-4 py-2 text-right font-mono text-xs text-[var(--text-primary)]">
                          {formatBytes(d.bytes)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        )}

        {/* Retention subsection */}
        <div className="border-t border-[var(--border-primary)] pt-4">
          <h3 className="mb-2 text-sm font-medium text-[var(--text-secondary)]">
            Retention
          </h3>
          {retentionLoading && (
            <div className="flex items-center py-2">
              <Spinner size="sm" label="Loading retention" />
            </div>
          )}
          {retentionError && !retentionLoading && (
            <ErrorMessage
              title="Failed to load retention setting"
              message={retentionError}
              retry={() => void fetchRetention()}
            />
          )}
          {retention !== null && !retentionLoading && !retentionError && (
            <RetentionEditor
              initialDays={retention}
              onSaved={(days) => {
                setRetention(days);
                onToast(
                  days === 0
                    ? "Retention saved (never delete automatically)"
                    : `Retention saved (${days} day${days === 1 ? "" : "s"})`,
                );
              }}
            />
          )}
        </div>
      </CardBody>
    </Card>
  );
}

// -- Main page ----------------------------------------------------------------

export default function SettingsPage() {
  const [params, setParams] = useState<DeviceParam[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [storageToast, setStorageToast] = useState<string | null>(null);

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

      {/* Storage usage + retention */}
      <StoragePanel
        toast={storageToast}
        onToast={setStorageToast}
        onDismissToast={() => setStorageToast(null)}
      />
    </PageWrapper>
  );
}
