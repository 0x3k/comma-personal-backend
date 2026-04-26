import type {
  RouteDataRequestKind,
  RouteDataRequestPostResponse,
  RouteDataRequestStatusResponse,
} from "@/lib/types";

const BASE_URL =
  process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";

interface ApiError {
  error: string;
  code: number;
}

interface RequestOptions extends Omit<RequestInit, "body"> {
  body?: unknown;
  /**
   * When true, a 401 response does NOT trigger the login redirect and
   * no ApiUnauthorizedError is thrown; instead the 401 is surfaced as
   * a normal Error with the server-provided message. Used by public
   * pages (e.g. the /share/[token] view) that must not bounce a
   * visitor to /login just because their token is invalid.
   */
  skipAuthRedirect?: boolean;
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
  const { body, headers, credentials, skipAuthRedirect, ...rest } = options;

  const response = await fetch(`${BASE_URL}${path}`, {
    credentials: credentials ?? "include",
    headers: {
      "Content-Type": "application/json",
      ...headers,
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
    ...rest,
  });

  if (response.status === 401 && !skipAuthRedirect) {
    redirectToLogin();
    throw new ApiUnauthorizedError();
  }

  if (!response.ok) {
    const errorBody: ApiError = await response.json().catch(() => ({
      error: response.statusText,
      code: response.status,
    }));
    const err = new Error(errorBody.error) as Error & { status?: number };
    err.status = response.status;
    throw err;
  }

  // Handle 204 No Content
  if (response.status === 204) {
    return undefined as T;
  }

  return response.json() as Promise<T>;
}

/**
 * Trigger a full-resolution data pull for the given route. The backend
 * (internal/api/route_data_request.go) returns 201 when the device was
 * online and the dispatch went out, 202 when the device is offline so
 * the dispatcher worker will retry, and 200 when an existing non-failed
 * request inside the idempotency window is reused. apiFetch flattens
 * all three success codes to the parsed body, so callers only need to
 * inspect `response.request.status` to decide what to render.
 */
export function requestFullRouteData(
  dongleId: string,
  routeName: string,
  kind: RouteDataRequestKind,
): Promise<RouteDataRequestPostResponse> {
  return apiFetch<RouteDataRequestPostResponse>(
    `/v1/route/${encodeURIComponent(dongleId)}/${encodeURIComponent(routeName)}/request_full_data`,
    {
      method: "POST",
      body: { kind },
    },
  );
}

/**
 * Poll the status of an in-flight (or completed) route data request.
 * The backend re-derives progress from per-segment upload flags on
 * every call, so the UI can poll this endpoint without worrying about
 * stale progress columns.
 */
export function getFullRouteDataRequest(
  dongleId: string,
  routeName: string,
  requestId: number,
): Promise<RouteDataRequestStatusResponse> {
  return apiFetch<RouteDataRequestStatusResponse>(
    `/v1/route/${encodeURIComponent(dongleId)}/${encodeURIComponent(routeName)}/request_full_data/${requestId}`,
  );
}

export { BASE_URL, ApiUnauthorizedError };
