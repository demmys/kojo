// Auth bootstrap for the kojo UI.
//
// kojo's `--local` mode and any future hardened deployment serve the
// UI behind an auth-required listener that demands a Bearer token on
// every /api/v1/* request. The UI receives the Owner token through a
// one-shot `?token=…` query parameter that kojo prints on stdout
// during startup; that param is consumed on the first page load,
// stashed in localStorage, and stripped from the URL bar so it cannot
// leak via screenshots / URL history.
//
// In Tailscale mode the listener is Owner-trusted by middleware so
// these helpers no-op gracefully when no token is present.

const STORAGE_KEY = "kojoOwnerToken";

let cachedToken: string | null | undefined;

/** Read and remove `?token=…` from window.location, persisting it. */
export function bootstrapTokenFromURL() {
  if (typeof window === "undefined") return;
  const url = new URL(window.location.href);
  const tok = url.searchParams.get("token");
  if (!tok) return;
  cachedToken = tok;
  // Try persistent storage first, then sessionStorage as a fallback so
  // a private-mode browser still survives a same-tab reload.
  let persisted = false;
  try {
    window.localStorage.setItem(STORAGE_KEY, tok);
    persisted = true;
  } catch {
    try {
      window.sessionStorage.setItem(STORAGE_KEY, tok);
      persisted = true;
    } catch {
      // Both storages are blocked — the token will live only in
      // `cachedToken` for this page lifetime. Leaving ?token= in the
      // URL would let the user bookmark/refresh, but it also means a
      // screenshot leaks the secret. Cleanup is the lesser evil.
    }
  }
  if (persisted) {
    url.searchParams.delete("token");
    // Replace the URL so the secret does not linger in the address
    // bar / history. The hash and any other params are preserved.
    const newHref = url.pathname + (url.search ? url.search : "") + url.hash;
    window.history.replaceState(null, "", newHref);
  }
}

/** Returns the stored Owner token (or empty string if none). */
export function getOwnerToken(): string {
  if (cachedToken !== undefined) return cachedToken ?? "";
  if (typeof window === "undefined") return "";
  try {
    cachedToken = window.localStorage.getItem(STORAGE_KEY);
  } catch {
    cachedToken = null;
  }
  if (!cachedToken) {
    try {
      cachedToken = window.sessionStorage.getItem(STORAGE_KEY);
    } catch {
      // ignore
    }
  }
  return cachedToken ?? "";
}

/**
 * Returns a Headers object with `Authorization: Bearer …` set when a
 * token is present. For use with raw fetch() calls (image blobs, HEAD
 * probes) that bypass httpClient.
 */
export function authHeaders(extra?: HeadersInit): Headers {
  const h = new Headers(extra);
  const tok = getOwnerToken();
  if (tok && !h.has("Authorization")) h.set("Authorization", `Bearer ${tok}`);
  return h;
}

/**
 * Append `?token=…` to a path when a token is stored. Used for `<img src>`
 * and similar contexts where setting headers is impossible.
 */
export function appendTokenQuery(path: string): string {
  const tok = getOwnerToken();
  if (!tok) return path;
  const sep = path.includes("?") ? "&" : "?";
  return `${path}${sep}token=${encodeURIComponent(tok)}`;
}

/** Wipe the stored token (used by a future "log out" flow). */
export function clearOwnerToken() {
  cachedToken = null;
  try {
    window.localStorage.removeItem(STORAGE_KEY);
  } catch {
    // ignore
  }
  try {
    window.sessionStorage.removeItem(STORAGE_KEY);
  } catch {
    // ignore
  }
}
