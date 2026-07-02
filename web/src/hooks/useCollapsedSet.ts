import { useCallback, useEffect, useState } from "react";

/**
 * A localStorage-persisted set of collapsed ids with a click-toggle handler.
 * The stored value is a JSON array of ids; read/write failures (quota, private
 * mode) are swallowed so the UI still works with an in-memory set.
 */
export function useCollapsedSet(storageKey: string) {
  const [collapsed, setCollapsed] = useState<Set<string>>(() => {
    try {
      const saved = localStorage.getItem(storageKey);
      return saved ? new Set<string>(JSON.parse(saved)) : new Set<string>();
    } catch {
      return new Set<string>();
    }
  });

  useEffect(() => {
    try {
      localStorage.setItem(storageKey, JSON.stringify([...collapsed]));
    } catch {
      /* quota / private mode */
    }
  }, [storageKey, collapsed]);

  const toggle = useCallback((id: string, e: React.MouseEvent) => {
    e.stopPropagation();
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  return [collapsed, toggle] as const;
}
