import { useCallback, useEffect, useState } from "react";

/**
 * A composer text input whose draft is persisted to sessionStorage under
 * `${keyPrefix}:${id}`. The draft is restored whenever `id` changes; when
 * `id` is undefined the input is empty and nothing is persisted.
 *
 * `setInput` writes through to storage (mirrors the old inline onChange).
 * `clearDraft` empties the input and removes the stored key. Callers keep
 * ownership of any co-located side effects (autosize, error reset).
 */
export function useDraftInput(keyPrefix: string, id: string | undefined) {
  const key = id ? `${keyPrefix}:${id}` : null;

  const [input, setInputState] = useState(() => (key ? sessionStorage.getItem(key) ?? "" : ""));

  useEffect(() => {
    setInputState(key ? sessionStorage.getItem(key) ?? "" : "");
  }, [key]);

  const setInput = useCallback((v: string) => {
    setInputState(v);
    if (key) sessionStorage.setItem(key, v);
  }, [key]);

  const clearDraft = useCallback(() => {
    setInputState("");
    if (key) sessionStorage.removeItem(key);
  }, [key]);

  return { input, setInput, clearDraft };
}
