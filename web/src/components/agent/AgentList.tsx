import { useEffect, useState } from "react";
import { useNavigate } from "react-router";
import { agentApi, type AgentInfo } from "../../lib/agentApi";
import { timeAgo } from "../../lib/utils";

export function AgentList() {
  const [agents, setAgents] = useState<AgentInfo[]>([]);
  const navigate = useNavigate();

  useEffect(() => {
    const load = () => agentApi.list().then(setAgents).catch(console.error);
    load();
    const interval = setInterval(load, 5000);
    return () => clearInterval(interval);
  }, []);

  // Sort by updatedAt descending
  const sorted = [...agents].sort(
    (a, b) => new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime(),
  );

  return (
    <div className="space-y-2">
      {sorted.length === 0 && (
        <p className="text-neutral-500 text-center py-12">No agents yet</p>
      )}
      {sorted.map((agent) => (
        <button
          key={agent.id}
          onClick={() => navigate(`/agents/${agent.id}`)}
          className="w-full flex items-center gap-3 p-3 bg-neutral-900 hover:bg-neutral-800 rounded-lg border border-neutral-800 text-left"
        >
          <img
            src={agentApi.avatarUrl(agent.id)}
            alt={agent.name}
            className="w-12 h-12 rounded-full object-cover bg-neutral-800 shrink-0"
          />
          <div className="flex-1 min-w-0">
            <div className="flex items-center justify-between">
              <span className="font-medium text-sm truncate">{agent.name}</span>
              <span className="text-[10px] text-neutral-600 shrink-0 ml-2">
                {agent.lastMessage
                  ? timeAgo(agent.lastMessage.timestamp)
                  : timeAgo(agent.createdAt)}
              </span>
            </div>
            <div className="text-xs text-neutral-500 truncate mt-0.5">
              {agent.lastMessage
                ? `${agent.lastMessage.role === "user" ? "You: " : ""}${agent.lastMessage.content}`
                : agent.persona
                  ? agent.persona.slice(0, 60) + (agent.persona.length > 60 ? "..." : "")
                  : "No messages yet"}
            </div>
            <div className="flex items-center gap-2 mt-1">
              <span className="text-[10px] text-neutral-600 font-mono">{agent.tool}</span>
              {agent.model && (
                <span className="text-[10px] text-neutral-600 font-mono">{agent.model}</span>
              )}
            </div>
          </div>
        </button>
      ))}
    </div>
  );
}
