import { getOwnerToken } from "./auth";

const BASE = "";

/**
 * Merge the stashed Owner Bearer token into the request headers when
 * one is available. The Tailscale listener is Owner-trusted by
 * middleware so a missing token is fine; the auth-required (--local)
 * listener requires it on every /api/v1/* call.
 */
function withAuth(init?: RequestInit): RequestInit | undefined {
  const tok = getOwnerToken();
  if (!tok) return init;
  const headers = new Headers(init?.headers);
  if (!headers.has("Authorization")) headers.set("Authorization", `Bearer ${tok}`);
  return { ...init, headers };
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE + path, withAuth(init));
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  if (res.status === 204 || res.headers.get("content-length") === "0") {
    return undefined as T;
  }
  return res.json();
}

const jsonHeaders = { "Content-Type": "application/json" } as const;

export function get<T>(path: string): Promise<T> {
  return request<T>(path);
}

export function post<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, {
    method: "POST",
    headers: jsonHeaders,
    body: body ? JSON.stringify(body) : undefined,
  });
}

export function put<T>(path: string, body: unknown): Promise<T> {
  return request<T>(path, {
    method: "PUT",
    headers: jsonHeaders,
    body: JSON.stringify(body),
  });
}

export function patch<T>(path: string, body: unknown): Promise<T> {
  return request<T>(path, {
    method: "PATCH",
    headers: jsonHeaders,
    body: JSON.stringify(body),
  });
}

export function del<T>(path: string): Promise<T> {
  return request<T>(path, { method: "DELETE" });
}

export function upload<T>(path: string, form: FormData): Promise<T> {
  return request<T>(path, { method: "POST", body: form });
}
