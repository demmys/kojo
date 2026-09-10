import { useEffect, useState } from "react";
import { get } from "../lib/httpClient";

type GoalBinding = {
  desiredPaused: boolean;
  state?: { objective: string; status: string; tokensUsed: number; tokenBudget: number | null; timeUsedSeconds: number };
};

export function GoalControls({ agentId, sessionKey = "", enabled, onToggle, running, onCommand, budget, onBudget }: {
  budget: string; onBudget: (value: string) => void;
  agentId: string; sessionKey?: string | null; enabled: boolean; onToggle: (value: boolean) => void;
  running: boolean; onCommand: (command: string) => void;
}) {
  const [binding, setBinding] = useState<GoalBinding | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let cancelled = false;
    setBinding(null);
    if (sessionKey === null) return;
    const load = async () => {
      try {
        const value = await get<GoalBinding | null>(`/api/v1/agents/${encodeURIComponent(agentId)}/goal?sessionKey=${encodeURIComponent(sessionKey)}`);
        if (!cancelled) { setBinding(value); setError(""); }
      } catch { if (!cancelled) setError("Goal status unavailable"); }
    };
    void load();
    const timer = setInterval(() => void load(), 5000);
    return () => { cancelled = true; clearInterval(timer); };
  }, [agentId, sessionKey, running]);
  const goal = binding?.state;
  const unfinished = !!goal && goal.status !== "complete";
  return <div className="mb-2 rounded-lg border border-hairline p-2 text-xs text-ink-dim">
    <label className="flex items-center gap-2">
      <input type="checkbox" checked={enabled} disabled={running || unfinished} onChange={(e) => onToggle(e.target.checked)} />
      Goal · Codex native
    </label>
    {error && <p role="status">{error}</p>}
    {goal && <>
      <p className="mt-1 whitespace-pre-wrap break-words">{goal.objective}</p>
      <p>{binding?.desiredPaused ? "paused" : goal.status} · {goal.tokensUsed.toLocaleString()}{goal.tokenBudget !== null ? ` / ${goal.tokenBudget.toLocaleString()}` : ""} tokens · {goal.timeUsedSeconds}s</p>
      <div className="mt-1 flex gap-3">
        <button type="button" onClick={() => onCommand("!goal status")}>Status</button>
        {running && unfinished ? <button type="button" onClick={() => onCommand("!goal pause")}>Pause</button> : unfinished && <button type="button" onClick={() => onCommand("!goal resume")}>Resume</button>}
        <button type="button" onClick={() => onCommand("!goal clear")}>Clear</button>
      </div>
    </>}
    {enabled && <label className="mt-1 block">Token budget (optional) <input className="w-28 border border-hairline bg-raised p-1" type="number" min="1" step="1" value={budget} onChange={(e) => onBudget(e.target.value)} /></label>}
    {enabled && <p>Continues until the goal stops. Native token budgets may be exceeded. Set a budget with !goal budget N.</p>}
  </div>;
}
