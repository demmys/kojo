// Goal mode (Codex native) control for the chat composer.
//
// Renders as a single icon button in the composer's button row; everything
// else (the mode toggle, live goal status, pause/resume/clear, the optional
// token budget) lives in a popover anchored above the button so nothing sits
// permanently between the transcript and the textarea. The button tints
// copper while goal mode is armed or a goal is still running, so the state
// stays visible with the popover closed.
import { useEffect, useId, useRef, useState } from "react";
import { get } from "../lib/httpClient";
import { useT, type MessageKey } from "../lib/i18n";
import { Toggle } from "./ui/Toggle";

// Native goal statuses the server can report (see codex_goal_transfer.go).
// Anything outside this set falls back to the raw value.
const STATUS_LABEL: Record<string, MessageKey> = {
  active: "goal.state.active",
  paused: "goal.state.paused",
  blocked: "goal.state.blocked",
  usage_limited: "goal.state.usageLimited",
  budget_limited: "goal.state.budgetLimited",
  complete: "goal.state.complete",
};
const STATUS_DOT: Record<string, string> = {
  active: "bg-lamp-run",
  paused: "bg-lamp-warn",
  blocked: "bg-lamp-err",
  usage_limited: "bg-lamp-err",
  budget_limited: "bg-lamp-err",
};

type GoalBinding = {
  desiredPaused: boolean;
  state?: { objective: string; status: string; tokensUsed: number; tokenBudget: number | null; timeUsedSeconds: number };
};

export function GoalControls({ agentId, sessionKey = "", enabled, onToggle, running, onCommand, budget, onBudget }: {
  budget: string; onBudget: (value: string) => void;
  agentId: string; sessionKey?: string | null; enabled: boolean; onToggle: (value: boolean) => void;
  running: boolean; onCommand: (command: string) => void;
}) {
  const t = useT();
  const [binding, setBinding] = useState<GoalBinding | null>(null);
  const [error, setError] = useState("");
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const dialogRef = useRef<HTMLDivElement>(null);
  const descId = useId();
  useEffect(() => {
    let cancelled = false;
    setBinding(null);
    if (sessionKey === null) return;
    const load = async () => {
      try {
        const value = await get<GoalBinding | null>(`/api/v1/agents/${encodeURIComponent(agentId)}/goal?sessionKey=${encodeURIComponent(sessionKey)}`);
        if (!cancelled) { setBinding(value); setError(""); }
      } catch { if (!cancelled) setError(t("goal.statusUnavailable")); }
    };
    void load();
    const timer = setInterval(() => void load(), 5000);
    return () => { cancelled = true; clearInterval(timer); };
  }, [agentId, sessionKey, running, t]);
  useEffect(() => {
    if (!open) return;
    const onPointer = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onPointer);
    // Land keyboard focus inside the popover so Tab/Escape work from it.
    dialogRef.current?.focus();
    return () => document.removeEventListener("mousedown", onPointer);
  }, [open]);
  const goal = binding?.state;
  const unfinished = !!goal && goal.status !== "complete";
  const active = enabled || unfinished;
  // What the goal is doing right now; a pending pause request shows as paused.
  const shown = goal ? (binding?.desiredPaused ? "paused" : goal.status) : "";
  const shownLabel = shown ? (STATUS_LABEL[shown] ? t(STATUS_LABEL[shown]) : shown) : "";
  const dotClass = unfinished ? STATUS_DOT[shown] ?? "bg-lamp-err" : "";
  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") { e.preventDefault(); setOpen(false); triggerRef.current?.focus(); }
  };
  // Tab (or a click) that moves focus outside the wrapper collapses the popover.
  const onBlur = (e: React.FocusEvent) => {
    if (open && !wrapRef.current?.contains(e.relatedTarget as Node | null)) setOpen(false);
  };
  const actionClass = "rounded-[8px] border border-hairline px-2 py-1 text-ink-dim transition-colors hover:border-copper/50 hover:text-ink";
  return <div ref={wrapRef} className="relative shrink-0" onKeyDown={onKeyDown} onBlur={onBlur}>
    <button
      ref={triggerRef}
      type="button"
      onClick={() => setOpen((v) => !v)}
      aria-label={t("goal.title")}
      aria-expanded={open}
      aria-haspopup="dialog"
      aria-describedby={descId}
      title={t("goal.title")}
      className={`relative rounded-[10px] p-2 transition-colors ${active ? "text-copper hover:text-copper" : "text-ink-faint hover:text-ink"}`}
    >
      <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" className="h-5 w-5">
        <circle cx="12" cy="12" r="9" />
        <circle cx="12" cy="12" r="5" />
        <circle cx="12" cy="12" r="1.2" fill="currentColor" />
      </svg>
      {unfinished && <span className={`absolute right-1 top-1 h-2 w-2 rounded-full ${dotClass}`} aria-hidden="true" />}
    </button>
    <span id={descId} className="sr-only">{enabled ? t("goal.armed") : t("goal.off")}{unfinished ? ` · ${shownLabel}` : ""}</span>
    {open && <div ref={dialogRef} tabIndex={-1} role="dialog" aria-label={t("goal.title")} className="absolute bottom-full left-0 z-40 mb-2 max-h-[60vh] w-72 max-w-[calc(100vw-6rem)] overflow-y-auto rounded-[10px] border border-hairline bg-raised p-3 text-xs text-ink-dim shadow-xl shadow-black/40 outline-none">
      <div className="flex items-center justify-between gap-3">
        <span className="text-[13px] text-ink">{t("goal.title")}</span>
        <Toggle checked={enabled} disabled={running || unfinished} onChange={onToggle} aria-label={t("goal.title")} />
      </div>
      {error && <p role="status" className="mt-2 text-lamp-err">{error}</p>}
      {goal && <div className="mt-2 border-t border-hairline pt-2">
        <p className="whitespace-pre-wrap break-words text-ink">{goal.objective}</p>
        <p className="mt-1">{shownLabel} · {t("goal.usage", { tokens: goal.tokensUsed.toLocaleString() + (goal.tokenBudget !== null ? ` / ${goal.tokenBudget.toLocaleString()}` : ""), seconds: String(goal.timeUsedSeconds) })}</p>
        <div className="mt-2 flex gap-2">
          <button type="button" className={actionClass} onClick={() => onCommand("!goal status")}>{t("goal.status")}</button>
          {running && unfinished ? <button type="button" className={actionClass} onClick={() => onCommand("!goal pause")}>{t("goal.pause")}</button> : unfinished && <button type="button" className={actionClass} onClick={() => onCommand("!goal resume")}>{t("goal.resume")}</button>}
          <button type="button" className={actionClass} onClick={() => onCommand("!goal clear")}>{t("goal.clear")}</button>
        </div>
      </div>}
      {enabled && <div className="mt-2 border-t border-hairline pt-2">
        <label className="flex items-center justify-between gap-2">{t("goal.budget")} <input className="w-28 rounded-[8px] border border-hairline bg-surface px-2 py-1 text-ink focus:border-copper/50 focus:outline-none" type="number" min="1" step="1" value={budget} onChange={(e) => onBudget(e.target.value)} /></label>
        <p className="mt-2 text-ink-faint">{t("goal.hint")}</p>
      </div>}
    </div>}
  </div>;
}
