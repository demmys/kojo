import { Component, lazy, Suspense, type ComponentType, type ReactNode } from "react";

// Catches a chunk that failed to load (after the one-shot auto reload
// was already spent) and offers a manual retry instead of leaving the
// pane — or the whole app — blank.
class ChunkErrorBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() {
    return { failed: true };
  }
  render() {
    if (!this.state.failed) return this.props.children;
    return (
      <div className="p-6 text-sm text-neutral-400">
        <p>Failed to load this page (network error or outdated build).</p>
        <button
          type="button"
          className="mt-3 rounded border border-neutral-600 px-3 py-1 text-neutral-200"
          onClick={() => window.location.reload()}
        >
          Reload
        </button>
      </div>
    );
  }
}

const RELOAD_KEY = "kojo:chunk-reload-at";

// A dynamic import can fail when the tab is older than the running
// build (hashed chunk no longer exists) or on a flaky network. Reload
// once to pick up the fresh index.html; the timestamp guard prevents a
// reload loop if the failure persists.
function reloadOnce(err: unknown): never {
  try {
    const last = Number(sessionStorage.getItem(RELOAD_KEY) || 0);
    if (Date.now() - last > 30_000) {
      sessionStorage.setItem(RELOAD_KEY, String(Date.now()));
      window.location.reload();
    }
  } catch {
    // sessionStorage unavailable: just surface the error.
  }
  throw err;
}

/**
 * lazyRoute wraps a named export in React.lazy + its own Suspense
 * boundary, so a loading pane never blanks the surrounding layout.
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function lazyRoute<M extends Record<string, any>, K extends keyof M & string>(
  loader: () => Promise<M>,
  name: K,
): ComponentType<React.ComponentProps<M[K]>> {
  const Lazy = lazy(() =>
    loader().then(
      (m) => ({ default: m[name] as ComponentType<React.ComponentProps<M[K]>> }),
      reloadOnce,
    ),
  );
  function Wrapped(props: React.ComponentProps<M[K]>) {
    return (
      <ChunkErrorBoundary>
        <Suspense fallback={null}>
          <Lazy {...props} />
        </Suspense>
      </ChunkErrorBoundary>
    );
  }
  Wrapped.displayName = `Lazy(${name})`;
  return Wrapped;
}
