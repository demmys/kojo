import { useCallback, useRef } from "react";

/** Max composer height in px before the textarea starts scrolling. */
const MAX_HEIGHT = 150;

/**
 * A textarea ref plus a `resize()` that grows the element to fit its content
 * up to MAX_HEIGHT. Use `resize` as the onInput handler and call it (typically
 * inside a requestAnimationFrame after a draft restore) to size on mount.
 */
export function useAutoGrowTextarea() {
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  const resize = useCallback(() => {
    if (textareaRef.current) {
      textareaRef.current.style.height = "auto";
      textareaRef.current.style.height = Math.min(textareaRef.current.scrollHeight, MAX_HEIGHT) + "px";
    }
  }, []);

  return { textareaRef, resize };
}
