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
 * Fetch wrapper for making typed API calls to the Go backend.
 * Prepends the configurable base URL and handles JSON serialization.
 */
export async function apiFetch<T>(
  path: string,
  options: RequestOptions = {},
): Promise<T> {
  const { body, headers, ...rest } = options;

  const response = await fetch(`${BASE_URL}${path}`, {
    headers: {
      "Content-Type": "application/json",
      ...headers,
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
    ...rest,
  });

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

export { BASE_URL };
