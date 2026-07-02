import { useCallback, useEffect, useState } from "react";
import type { AgentMessageAttachment } from "../../lib/agentApi";
import { AgentAvatar } from "./AgentAvatar";
import { MarkdownRenderer } from "./MarkdownRenderer";
import { ToolUseCard } from "./ToolUseCard";
import { AttachmentList } from "./MessageAttachments";
import { FilePathChip, splitFilePaths } from "./filePaths";
import { MediaOverlay } from "./MediaOverlay";
import type { StreamingTool } from "./chatEventReducer";

export function actionBtnClass(isUser: boolean): string {
  return `flex items-center gap-1 px-1.5 py-0.5 rounded text-[10px] transition-colors ${
    isUser
      ? "text-blue-200/50 hover:text-blue-100 hover:bg-blue-500/20"
      : "text-neutral-500 hover:text-neutral-300 hover:bg-neutral-700/50"
  }`;
}

/** Collapsible thinking/reasoning block */
export function ThinkingBlock({ text, streaming = false }: { text: string; streaming?: boolean }) {
  const [expanded, setExpanded] = useState(false);

  if (!text) return null;

  return (
    <div className="mb-2">
      <button
        onClick={() => setExpanded(!expanded)}
        className="flex items-center gap-1.5 text-[11px] text-neutral-500 hover:text-neutral-400 transition-colors"
      >
        <svg
          className={`w-3 h-3 transition-transform ${expanded ? "rotate-90" : ""}`}
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
          strokeWidth={2}
        >
          <path strokeLinecap="round" strokeLinejoin="round" d="M8.25 4.5l7.5 7.5-7.5 7.5" />
        </svg>
        {streaming ? (
          <span className="flex items-center gap-1">
            <span className="w-1 h-1 bg-neutral-500 rounded-full animate-pulse" />
            Thinking…
          </span>
        ) : (
          "Thought"
        )}
      </button>
      {expanded && (
        <div className="mt-1 pl-4 border-l border-neutral-700/50 text-xs text-neutral-500 leading-relaxed whitespace-pre-wrap wrap-anywhere">
          {text}
        </div>
      )}
    </div>
  );
}

/** Streaming bubble for assistant response in progress */
interface StreamingMessageProps {
  text: string;
  thinking: string;
  toolUses: StreamingTool[];
  attachments?: AgentMessageAttachment[];
  agentName: string;
  agentId: string;
  status: string;
  avatarHash?: string;
  startTime: number;
  viewMode: "markdown" | "plain";
  onViewModeChange: (mode: "markdown" | "plain") => void;
}

export function StreamingMessage({
  text,
  thinking,
  toolUses,
  attachments,
  agentName,
  agentId,
  status,
  avatarHash,
  startTime,
  viewMode,
  onViewModeChange,
}: StreamingMessageProps) {
  const [preview, setPreview] = useState<{ path: string; type: "image" | "video" } | null>(null);

  const processText = useCallback(
    (t: string): React.ReactNode => {
      const segs = splitFilePaths(t);
      if (segs.length === 1 && segs[0].type === "text") return t;
      return segs.map((seg, i) =>
        seg.type === "text" ? (
          seg.value
        ) : (
          <FilePathChip key={i} path={seg.value} onPreview={setPreview} />
        ),
      );
    },
    [],
  );

  let activeTool: string | null = null;
  for (let i = toolUses.length - 1; i >= 0; i--) {
    if (toolUses[i].output === null) {
      activeTool = toolUses[i].name;
      break;
    }
  }

  const btnCls = actionBtnClass(false);

  return (
    <div className="flex gap-3 flex-row">
      <AgentAvatar agentId={agentId} name={agentName} size="sm" className="mt-1" cacheBust={avatarHash} />
      <div className="max-w-[80%] min-w-0 bg-neutral-800/80 text-neutral-200 rounded-2xl rounded-tl-sm px-3.5 py-2.5">
        {status === "thinking" && !text && !thinking && toolUses.length === 0 && (
          <div className="flex items-center gap-1.5 py-1">
            <span className="w-1.5 h-1.5 bg-neutral-400 rounded-full animate-bounce" style={{ animationDelay: "0ms" }} />
            <span className="w-1.5 h-1.5 bg-neutral-400 rounded-full animate-bounce" style={{ animationDelay: "150ms" }} />
            <span className="w-1.5 h-1.5 bg-neutral-400 rounded-full animate-bounce" style={{ animationDelay: "300ms" }} />
            <ElapsedTimer startTime={startTime} threshold={3} className="text-xs text-neutral-500 ml-2" />
          </div>
        )}
        {thinking && <ThinkingBlock text={thinking} streaming={!text} />}
        {/* Streaming attachments — rendered as they arrive from kojo-attach */}
        {attachments && attachments.length > 0 && (
          <AttachmentList attachments={attachments} isUser={false} />
        )}
        {text && (
          <div className="relative">
            {viewMode === "markdown" ? (
              <MarkdownRenderer content={text} processText={processText} />
            ) : (
              <div className="text-sm whitespace-pre-wrap wrap-anywhere leading-relaxed">
                {text}
              </div>
            )}
            <span className="inline-block w-0.5 h-4 bg-neutral-400 animate-pulse ml-0.5 align-text-bottom" />
          </div>
        )}
        {toolUses.length > 0 && (
          <div className="mt-2">
            {toolUses.map((tu, i) => (
              <ToolUseCard key={i} toolUse={{ ...tu, output: tu.output ?? "" }} />
            ))}
          </div>
        )}
        {/* Status bar: elapsed time + active tool + view toggle */}
        {(text || toolUses.length > 0) && (
          <div className="flex items-center gap-2 mt-1.5 text-[10px] text-neutral-500">
            <ElapsedTimer startTime={startTime} className="" />
            {activeTool && (
              <span className="flex items-center gap-1">
                <span className="w-1 h-1 bg-blue-400 rounded-full animate-pulse" />
                {activeTool}
              </span>
            )}
            {text && (
              <button
                onClick={() => onViewModeChange(viewMode === "markdown" ? "plain" : "markdown")}
                className={`${btnCls} ml-auto`}
                title={viewMode === "markdown" ? "Show plain text" : "Show rendered"}
              >
                {viewMode === "markdown" ? (
                  <>
                    <svg className="w-3 h-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                      <path strokeLinecap="round" strokeLinejoin="round" d="M17.25 6.75L22.5 12l-5.25 5.25m-10.5 0L1.5 12l5.25-5.25m7.5-3l-4.5 16.5" />
                    </svg>
                    Raw
                  </>
                ) : (
                  <>
                    <svg className="w-3 h-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
                      <path strokeLinecap="round" strokeLinejoin="round" d="M19.5 14.25v-2.625a3.375 3.375 0 00-3.375-3.375h-1.5A1.125 1.125 0 0113.5 7.125v-1.5a3.375 3.375 0 00-3.375-3.375H8.25m0 12.75h7.5m-7.5 3H12M10.5 2.25H5.625c-.621 0-1.125.504-1.125 1.125v17.25c0 .621.504 1.125 1.125 1.125h12.75c.621 0 1.125-.504 1.125-1.125V11.25a9 9 0 00-9-9z" />
                    </svg>
                    Render
                  </>
                )}
              </button>
            )}
          </div>
        )}
      </div>
      {preview && (
        <MediaOverlay
          path={preview.path}
          type={preview.type}
          onClose={() => setPreview(null)}
        />
      )}
    </div>
  );
}

/** Self-contained ticking elapsed timer. Only this component re-renders each second. */
function ElapsedTimer({ startTime, threshold = 0, className }: { startTime: number; threshold?: number; className?: string }) {
  const [elapsed, setElapsed] = useState(() => Math.floor((Date.now() - startTime) / 1000));

  useEffect(() => {
    const timer = setInterval(() => {
      setElapsed(Math.floor((Date.now() - startTime) / 1000));
    }, 1000);
    return () => clearInterval(timer);
  }, [startTime]);

  if (elapsed < threshold) return null;
  return <span className={className}>{formatElapsed(elapsed)}</span>;
}

function formatElapsed(s: number): string {
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  const sec = s % 60;
  return `${m}m${sec > 0 ? `${sec}s` : ""}`;
}
