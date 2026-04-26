import { useEffect, useState } from "react";
import { agentApi, type AgentInfo } from "../../lib/agentApi";
import { AgentAvatar } from "../agent/AgentAvatar";

interface Props {
  setError: (msg: string) => void;
  flashSuccess: () => void;
}

/**
 * ArchivedAgentsSection lists agents that have been archived (data retained
 * but runtime stopped) and lets the user restore or permanently delete each.
 *
 * Hidden behind global Settings on purpose: archive is a "soft delete" that
 * normal users shouldn't have to think about during day-to-day use, but power
 * users still need a place to recover or finally clear out archived agents.
 */
export function ArchivedAgentsSection({ setError, flashSuccess }: Props) {
  const [agents, setAgents] = useState<AgentInfo[] | null>(null);
  const [busy, setBusy] = useState<Record<string, "unarchive" | "delete" | undefined>>({});

  const reload = () => {
    agentApi
      .listArchived()
      .then(setAgents)
      .catch((e) => {
        setError(e instanceof Error ? e.message : String(e));
        setAgents([]);
      });
  };

  useEffect(reload, []); // eslint-disable-line react-hooks/exhaustive-deps

  const setAgentBusy = (id: string, op: "unarchive" | "delete" | undefined) =>
    setBusy((prev) => ({ ...prev, [id]: op }));

  const handleUnarchive = async (a: AgentInfo) => {
    setAgentBusy(a.id, "unarchive");
    try {
      await agentApi.unarchive(a.id);
      setAgents((prev) => (prev ?? []).filter((x) => x.id !== a.id));
      flashSuccess();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setAgentBusy(a.id, undefined);
    }
  };

  const handleDelete = async (a: AgentInfo) => {
    if (
      !confirm(
        `Permanently delete "${a.name}" and all of its data? This cannot be undone.`,
      )
    ) {
      return;
    }
    setAgentBusy(a.id, "delete");
    try {
      await agentApi.delete(a.id);
      setAgents((prev) => (prev ?? []).filter((x) => x.id !== a.id));
      flashSuccess();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setAgentBusy(a.id, undefined);
    }
  };

  return (
    <div>
      <h2 className="text-xs font-semibold text-neutral-500 uppercase tracking-wider mb-3">
        Archived Agents
      </h2>
      <p className="text-xs text-neutral-600 mb-3">
        Archived agents are hidden from the main list and have no runtime activity. The agent's own data (1:1 chat history, memory, persona, credentials, notify tokens) is preserved. Group DM memberships are not — the agent was removed from every group on archive (2-person groups were dissolved and their transcripts deleted), and memberships are NOT restored on unarchive. Delete wipes everything permanently.
      </p>

      {agents === null && (
        <p className="text-xs text-neutral-600 py-4 text-center">Loading...</p>
      )}
      {agents !== null && agents.length === 0 && (
        <p className="text-xs text-neutral-600 py-4 text-center">
          No archived agents
        </p>
      )}

      <div className="space-y-2">
        {(agents ?? []).map((a) => {
          const op = busy[a.id];
          return (
            <div
              key={a.id}
              className="flex items-center gap-3 p-3 bg-neutral-900 border border-neutral-800 rounded-lg"
            >
              <AgentAvatar
                agentId={a.id}
                name={a.name}
                size="sm"
                cacheBust={a.avatarHash}
              />
              <div className="flex-1 min-w-0">
                <div className="text-sm font-medium truncate">{a.name}</div>
                <div className="text-[10px] text-neutral-600 font-mono truncate">
                  {a.tool}
                  {a.archivedAt ? ` · archived ${a.archivedAt.slice(0, 10)}` : ""}
                </div>
              </div>
              <div className="flex gap-2 shrink-0">
                <button
                  onClick={() => handleUnarchive(a)}
                  disabled={op !== undefined}
                  className="px-3 py-1.5 text-xs bg-neutral-800 hover:bg-neutral-700 border border-neutral-700 rounded disabled:opacity-40"
                >
                  {op === "unarchive" ? "..." : "Restore"}
                </button>
                <button
                  onClick={() => handleDelete(a)}
                  disabled={op !== undefined}
                  className="px-3 py-1.5 text-xs bg-red-950 hover:bg-red-900 border border-red-800 text-red-300 rounded disabled:opacity-40"
                >
                  {op === "delete" ? "..." : "Delete"}
                </button>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
