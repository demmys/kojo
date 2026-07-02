// Presentational pieces shared by AgentChat and GroupDMChat composers.
// Every element here is byte-identical between the two call sites; the only
// parametrized difference is PendingAttachments' `thumb` (AgentChat previews
// via the thumbnail endpoint, GroupDMChat loads the raw blob).

import type { AgentMessageAttachment } from "../lib/agentApi";
import { api, isThumbSupported } from "../lib/api";

/** The repeated 16x16 "×" glyph used on dismiss/remove buttons. */
export function CloseIcon() {
  return (
    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" className="w-3 h-3">
      <path d="M5.28 4.22a.75.75 0 00-1.06 1.06L6.94 8l-2.72 2.72a.75.75 0 101.06 1.06L8 9.06l2.72 2.72a.75.75 0 101.06-1.06L9.06 8l2.72-2.72a.75.75 0 00-1.06-1.06L8 6.94 5.28 4.22z" />
    </svg>
  );
}

/** The generic (non-image) file glyph used in the pending-attachments chips. */
export function FileIcon() {
  return (
    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-4 h-4 text-neutral-500">
      <path d="M3 3.5A1.5 1.5 0 014.5 2h6.879a1.5 1.5 0 011.06.44l4.122 4.12A1.5 1.5 0 0117 7.622V16.5a1.5 1.5 0 01-1.5 1.5h-11A1.5 1.5 0 013 16.5v-13z" />
    </svg>
  );
}

/** A red inline error banner with a dismiss button. */
export function DismissibleError({ message, onDismiss }: { message: string; onDismiss: () => void }) {
  return (
    <div className="flex items-center gap-2 mb-2 px-3 py-1.5 bg-red-950/50 border border-red-900/50 rounded-lg text-xs text-red-300">
      <span className="flex-1">{message}</span>
      <button onClick={onDismiss} className="text-red-400 hover:text-red-200">
        <CloseIcon />
      </button>
    </div>
  );
}

/**
 * The pending-file chip row above the composer. `thumb` toggles the image
 * preview strategy to match each caller exactly:
 *   - thumb=true  → thumbnail endpoint when supported, lazy/async decode.
 *   - thumb=false → raw blob URL, no lazy/decoding hints.
 */
export function PendingAttachments({
  files,
  onRemove,
  thumb,
}: {
  files: AgentMessageAttachment[];
  onRemove: (index: number) => void;
  thumb: boolean;
}) {
  if (files.length === 0) return null;
  return (
    <div className="flex flex-wrap gap-2 mb-2">
      {files.map((file, i) => (
        <div
          key={file.path}
          className="flex items-center gap-1.5 px-2 py-1 bg-neutral-800 border border-neutral-700 rounded-lg text-xs text-neutral-300"
        >
          {file.mime.startsWith("image/") ? (
            thumb ? (
              <img
                src={
                  isThumbSupported(file.path)
                    ? api.files.thumbUrl(file.path, 64)
                    : api.files.rawUrl(file.path)
                }
                alt={file.name}
                className="w-6 h-6 rounded object-cover"
                loading="lazy"
                decoding="async"
              />
            ) : (
              <img
                src={api.files.rawUrl(file.path)}
                alt={file.name}
                className="w-6 h-6 rounded object-cover"
              />
            )
          ) : (
            <FileIcon />
          )}
          <span className="max-w-[120px] truncate">{file.name}</span>
          <button
            onClick={() => onRemove(i)}
            className="text-neutral-500 hover:text-neutral-300 ml-0.5"
          >
            <CloseIcon />
          </button>
        </div>
      ))}
    </div>
  );
}

/**
 * Shared Enter-to-send keydown behavior. When `enterSends` is true, plain
 * Enter sends and Shift+Enter inserts a newline; when false the mapping is
 * reversed. IME composition (isComposing) never triggers a send.
 */
export function enterToSend(
  e: React.KeyboardEvent,
  enterSends: boolean,
  onSend: () => void,
): void {
  if (e.key === "Enter" && !e.nativeEvent.isComposing) {
    if (enterSends ? !e.shiftKey : e.shiftKey) {
      e.preventDefault();
      onSend();
    }
  }
}
