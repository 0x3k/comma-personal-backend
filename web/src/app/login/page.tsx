"use client";

import { Suspense, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { BASE_URL } from "@/lib/api";
import { Button } from "@/components/ui/Button";
import { Card, CardBody, CardHeader } from "@/components/ui/Card";

/**
 * isSafeNext validates that a `next` query param points at a same-origin
 * relative path. Protocol-relative URLs (`//evil.example`) and absolute
 * URLs are rejected so an attacker cannot craft a login link that bounces
 * the user to a phishing page after authentication.
 */
function isSafeNext(next: string | null): next is string {
  if (!next) return false;
  if (!next.startsWith("/")) return false;
  if (next.startsWith("//")) return false;
  return true;
}

/**
 * extractErrorMessage pulls a human-friendly error string out of a JSON
 * response body of the shape `{"error": "...", "code": N}`. Falls back
 * to the response statusText when the body is not parseable.
 */
async function extractErrorMessage(
  response: Response,
  fallback: string,
): Promise<string> {
  try {
    const body = (await response.json()) as { error?: string };
    if (body && typeof body.error === "string" && body.error.length > 0) {
      return body.error;
    }
  } catch {
    // body was not JSON; fall through to the fallback.
  }
  return response.statusText || fallback;
}

function LoginForm() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const rawNext = searchParams?.get("next") ?? null;
  const nextPath = isSafeNext(rawNext) ? rawNext : "/";

  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function handleSubmit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);
    setSubmitting(true);

    try {
      const response = await fetch(`${BASE_URL}/v1/session/login`, {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });

      // When SESSION_SECRET is unset the backend does not register the
      // session routes at all, so we see 404. Treat that as "open mode"
      // and send the user home -- there is nothing to authenticate.
      if (response.status === 404) {
        router.replace(nextPath);
        return;
      }

      if (!response.ok) {
        const message = await extractErrorMessage(
          response,
          "login failed",
        );
        setError(message);
        return;
      }

      // Successful login: drain the body (best-effort) and navigate on.
      await response.json().catch(() => undefined);
      router.replace(nextPath);
    } catch (err) {
      setError(
        err instanceof Error
          ? err.message
          : "network error: failed to reach server",
      );
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <form onSubmit={handleSubmit} className="flex flex-col gap-4">
      <div>
        <label
          htmlFor="login-username"
          className="mb-1 block text-sm font-medium text-[var(--text-secondary)]"
        >
          Username
        </label>
        <input
          id="login-username"
          name="username"
          type="text"
          autoComplete="username"
          required
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          disabled={submitting}
          className="w-full rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-3 py-1.5 text-sm text-[var(--text-primary)] outline-none focus:ring-2 focus:ring-[var(--ring-focus)]"
        />
      </div>

      <div>
        <label
          htmlFor="login-password"
          className="mb-1 block text-sm font-medium text-[var(--text-secondary)]"
        >
          Password
        </label>
        <input
          id="login-password"
          name="password"
          type="password"
          autoComplete="current-password"
          required
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          disabled={submitting}
          className="w-full rounded border border-[var(--border-secondary)] bg-[var(--bg-primary)] px-3 py-1.5 text-sm text-[var(--text-primary)] outline-none focus:ring-2 focus:ring-[var(--ring-focus)]"
        />
      </div>

      {error && (
        <p
          role="alert"
          className="rounded border border-danger-500/25 bg-danger-500/5 px-3 py-2 text-sm text-danger-600 dark:text-danger-500"
        >
          {error}
        </p>
      )}

      <Button type="submit" size="md" disabled={submitting}>
        {submitting ? "Signing in..." : "Sign in"}
      </Button>
    </form>
  );
}

export default function LoginPage() {
  return (
    <main className="mx-auto flex w-full max-w-md flex-1 items-center justify-center px-4 py-12 sm:px-6">
      <Card className="w-full">
        <CardHeader>
          <h1 className="text-subheading">Sign in</h1>
          <p className="mt-1 text-caption">
            Enter your credentials to access the dashboard.
          </p>
        </CardHeader>
        <CardBody>
          {/*
           * useSearchParams must sit inside a Suspense boundary so the
           * page can be statically rendered at build time. Without this
           * `next build` fails with a "deopted into client-side rendering"
           * error on the /login route.
           */}
          <Suspense fallback={null}>
            <LoginForm />
          </Suspense>
        </CardBody>
      </Card>
    </main>
  );
}
