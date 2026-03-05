import type { AgentMessage } from "../../lib/agentApi";
import { ToolUseCard } from "./ToolUseCard";

interface ChatMessageProps {
  message: AgentMessage;
  agentName: string;
  agentId: string;
}

export function ChatMessage({ message, agentName, agentId }: ChatMessageProps) {
  const isUser = message.role === "user";

  return (
    <div className={`flex gap-2 ${isUser ? "flex-row-reverse" : "flex-row"}`}>
      {/* Avatar */}
      {!isUser && (
        <img
          src={`/api/v1/agents/${agentId}/avatar`}
          alt={agentName}
          className="w-8 h-8 rounded-full object-cover bg-neutral-800 shrink-0 mt-1"
        />
      )}

      {/* Bubble */}
      <div
        className={`max-w-[80%] ${
          isUser
            ? "bg-blue-600 text-white rounded-2xl rounded-tr-sm"
            : "bg-neutral-800 text-neutral-200 rounded-2xl rounded-tl-sm"
        } px-3.5 py-2.5`}
      >
        {/* Message content with simple markdown-like rendering */}
        <div className="text-sm whitespace-pre-wrap break-words leading-relaxed">
          {message.content}
        </div>

        {/* Tool uses */}
        {message.toolUses && message.toolUses.length > 0 && (
          <div className="mt-2">
            {message.toolUses.map((tu, i) => (
              <ToolUseCard key={i} toolUse={tu} />
            ))}
          </div>
        )}

        {/* Timestamp */}
        <div
          className={`text-[10px] mt-1 ${
            isUser ? "text-blue-200" : "text-neutral-500"
          }`}
        >
          {formatTime(message.timestamp)}
        </div>
      </div>
    </div>
  );
}

/** Streaming bubble for assistant response in progress */
interface StreamingMessageProps {
  text: string;
  toolUses: Array<{ name: string; input: string; output: string }>;
  agentName: string;
  agentId: string;
  status: string;
}

export function StreamingMessage({
  text,
  toolUses,
  agentName,
  agentId,
  status,
}: StreamingMessageProps) {
  return (
    <div className="flex gap-2 flex-row">
      <img
        src={`/api/v1/agents/${agentId}/avatar`}
        alt={agentName}
        className="w-8 h-8 rounded-full object-cover bg-neutral-800 shrink-0 mt-1"
      />
      <div className="max-w-[80%] bg-neutral-800 text-neutral-200 rounded-2xl rounded-tl-sm px-3.5 py-2.5">
        {status === "thinking" && !text && toolUses.length === 0 && (
          <div className="flex items-center gap-1">
            <span className="w-1.5 h-1.5 bg-neutral-400 rounded-full animate-bounce" style={{ animationDelay: "0ms" }} />
            <span className="w-1.5 h-1.5 bg-neutral-400 rounded-full animate-bounce" style={{ animationDelay: "150ms" }} />
            <span className="w-1.5 h-1.5 bg-neutral-400 rounded-full animate-bounce" style={{ animationDelay: "300ms" }} />
          </div>
        )}
        {text && (
          <div className="text-sm whitespace-pre-wrap break-words leading-relaxed">
            {text}
            <span className="inline-block w-0.5 h-4 bg-neutral-400 animate-pulse ml-0.5 align-text-bottom" />
          </div>
        )}
        {toolUses.length > 0 && (
          <div className="mt-2">
            {toolUses.map((tu, i) => (
              <ToolUseCard key={i} toolUse={tu} />
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function formatTime(timestamp: string): string {
  try {
    const d = new Date(timestamp);
    const now = new Date();
    const isToday =
      d.getDate() === now.getDate() &&
      d.getMonth() === now.getMonth() &&
      d.getFullYear() === now.getFullYear();
    if (isToday) {
      return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    }
    return d.toLocaleDateString([], {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  } catch {
    return "";
  }
}
