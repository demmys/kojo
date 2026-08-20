import { useState } from "react";
import type { TransferSkip } from "../../lib/agentApi";
import { useT } from "../../lib/i18n";

// TransferSkipsNotice renders the owner-facing "skipped during
// transfer" warning for an agent whose most recent §3.7 device-switch
// left session files behind (oversized JSONL, unreadable codex ref,
// …). The server stamps AgentInfo.lastTransferSkips on the target
// during agent-sync and clears it on the next clean transfer. The owner may
// also acknowledge it manually; a later lossy transfer creates a fresh notice.
//
// Collapsed by default to a one-line summary; click to expand the
// per-file detail (path, reason, size).
export function TransferSkipsNotice({
  skips,
  onDismiss,
}: {
  skips?: TransferSkip[];
  onDismiss?: () => Promise<void>;
}) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [dismissing, setDismissing] = useState(false);
  const [dismissFailed, setDismissFailed] = useState(false);
  if (!skips || skips.length === 0) return null;
  return (
    <div className="mt-1 rounded border border-lamp-warn/40 bg-lamp-warn/5 px-2 py-1 text-[11px] text-lamp-warn">
      <div className="flex items-center gap-1">
        <button
          type="button"
          onClick={(e) => {
            e.stopPropagation();
            setOpen((v) => !v);
          }}
          className="flex min-w-0 flex-1 items-center gap-1 text-left"
          title={t("skips.title")}
        >
          <span aria-hidden>⚠</span>
          <span>{t("skips.summary", { count: skips.length })}</span>
          <span className="ml-auto" aria-hidden>{open ? "▾" : "▸"}</span>
        </button>
        {onDismiss && (
          <button
            type="button"
            disabled={dismissing}
            onClick={async (e) => {
              e.stopPropagation();
              setDismissing(true);
              setDismissFailed(false);
              try {
                await onDismiss();
              } catch {
                setDismissFailed(true);
              } finally {
                setDismissing(false);
              }
            }}
            className="shrink-0 rounded px-0.5 text-lamp-warn/70 transition-colors hover:text-lamp-warn disabled:opacity-40"
            title={t("common.dismiss")}
            aria-label={t("common.dismiss")}
          >
            <span aria-hidden>×</span>
          </button>
        )}
      </div>
      {dismissFailed && (
        <div className="mt-1 text-lamp-err">{t("skips.dismissFailed")}</div>
      )}
      {open && (
        <ul className="mt-1 space-y-0.5">
          {skips.map((s) => (
            <li key={s.path} className="truncate font-mono">
              {s.path}
              <span className="ml-1 text-lamp-warn/70">
                ({s.reason}
                {s.sizeBytes ? `, ${formatBytes(s.sizeBytes)}` : ""})
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function formatBytes(n: number): string {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GiB`;
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MiB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KiB`;
  return `${n} B`;
}
