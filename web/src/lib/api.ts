const BASE_URL =
  process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";

interface ApiError {
  error: string;
  code: number;
}

interface RequestOptions extends Omit<RequestInit, "body"> {
  body?: unknown;
}

/**
 * ApiUnauthorizedError is thrown after the client has already initiated a
 * redirect to the login page. Handlers can catch it to short-circuit any
 * follow-up UI work (e.g. showing an inline error), since the browser is
 * already navigating away.
 */
class ApiUnauthorizedError extends Error {
  constructor(message: string = "unauthorized") {
    super(message);
    this.name = "ApiUnauthorizedError";
  }
}

/**
 * redirectToLogin navigates the browser to the login page, preserving the
 * current path + query as the `next` parameter so the login handler can
 * bounce back. No-op on the server (SSR) and on the login page itself, to
 * avoid redirect loops.
 */
function redirectToLogin(): void {
  if (typeof window === "undefined") {
    return;
  }
  const { pathname, search } = window.location;
  // Do not loop if we're already on /login.
  if (pathname === "/login") {
    return;
  }
  const next = pathname + search;
  const target = `/login?next=${encodeURIComponent(next)}`;
  window.location.assign(target);
}

/**
 * Fetch wrapper for making typed API calls to the Go backend.
 * Prepends the configurable base URL, sends credentials so the session
 * cookie is included on every request, and redirects to /login on 401.
 */
export async function apiFetch<T>(
  path: string,
  options: RequestOptions = {},
): Promise<T> {
  const { body, headers, credentials, ...rest } = options;

  const response = await fetch(`${BASE_URL}${path}`, {
    credentials: credentials ?? "include",
    headers: {
      "Content-Type": "application/json",
      ...headers,
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
    ...rest,
  });

  if (response.status === 401) {
    redirectToLogin();
    throw new ApiUnauthorizedError();
  }

  if (!response.ok) {
    const errorBody: ApiError = await response.json().catch(() => ({
      error: response.statusText,
      code: response.status,
    }));
    throw new Error(errorBody.error);
  }

  // Handle 204 No Content
  if (response.status === 204) {
    return undefined as T;
  }

  return response.json() as Promise<T>;
}

export { BASE_URL, ApiUnauthorizedError };
