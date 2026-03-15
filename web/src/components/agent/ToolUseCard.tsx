import { useState } from "react";
import type { ToolUse } from "../../lib/agentApi";

interface ToolUseCardProps {
  toolUse: ToolUse;
  defaultExpanded?: boolean;
}

function extractPreview(input: string): string | undefined {
  if (!input) return undefined;
  try {
    const parsed = JSON.parse(input);
    if (typeof parsed === "object" && parsed !== null) {
      if (typeof parsed.description === "string") return parsed.description;
      if (typeof parsed.command === "string") return parsed.command;
      if (typeof parsed.file_path === "string") return parsed.file_path;
      if (typeof parsed.pattern === "string") return parsed.pattern;
      if (typeof parsed.prompt === "string") return parsed.prompt.slice(0, 80);
    }
  } catch {
    // not JSON — use raw input preview
  }
  const line = input.split("\n")[0].trim();
  return line.length > 80 ? line.slice(0, 80) + "…" : line || undefined;
}

export function ToolUseCard({ toolUse, defaultExpanded = false }: ToolUseCardProps) {
  const [expanded, setExpanded] = useState(defaultExpanded);
  const preview = extractPreview(toolUse.input);

  return (
    <div className="my-1 border border-neutral-700 rounded-lg overflow-hidden text-xs">
      <button
        onClick={() => setExpanded(!expanded)}
        className="w-full flex items-center gap-2 px-3 py-1.5 bg-neutral-800/50 hover:bg-neutral-800 text-neutral-400 min-w-0"
      >
        <svg
          className={`w-3 h-3 shrink-0 transition-transform ${expanded ? "rotate-90" : ""}`}
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
        >
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 5l7 7-7 7" />
        </svg>
        <span className="font-mono text-neutral-300 shrink-0">
          {toolUse.name}
        </span>
        {preview && (
          <span className="text-neutral-500 truncate min-w-0">
            {preview}
          </span>
        )}
      </button>
      {expanded && (
        <div className="px-3 py-2 space-y-2 bg-neutral-900/50">
          {toolUse.input && (
            <div>
              <div className="text-neutral-500 mb-0.5">Input</div>
              <pre className="text-neutral-300 whitespace-pre-wrap break-all max-h-60 overflow-y-auto">
                {toolUse.input}
              </pre>
            </div>
          )}
          {toolUse.output && (
            <div>
              <div className="text-neutral-500 mb-0.5">Output</div>
              <pre className="text-neutral-300 whitespace-pre-wrap break-all max-h-60 overflow-y-auto">
                {toolUse.output}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
