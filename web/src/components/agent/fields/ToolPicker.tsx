import {
  defaultModelForTool,
  effortLevelsForModel,
  type EffortLevel,
} from "../../../lib/toolModels";

const TOOLS = ["claude", "codex", "grok", "custom", "llama.cpp"];

/**
 * The "Tool" (backend) selector. Switching tools resets the model to the
 * backend default (or clears it for custom/llama.cpp) and drops an effort
 * level the new default model can't support.
 *
 * `isDisabled` is optional: when provided (AgentCreate gates on server
 * availability) each button gets a `disabled` attribute plus the
 * `disabled:opacity-30` style; when omitted (AgentSettings) neither is
 * rendered, preserving the exact original markup of each caller.
 */
export function ToolPicker({
  tool,
  setTool,
  setModel,
  effort,
  setEffort,
  isDisabled,
}: {
  tool: string;
  setTool: (t: string) => void;
  setModel: (m: string) => void;
  effort: EffortLevel | "";
  setEffort: (e: EffortLevel | "") => void;
  isDisabled?: (t: string) => boolean;
}) {
  const disabledCls = isDisabled ? " disabled:opacity-30" : "";
  return (
    <div>
      <label className="block text-sm text-neutral-400 mb-2">Tool</label>
      <div className="flex flex-wrap gap-2">
        {TOOLS.map((t) => (
          <button
            key={t}
            onClick={() => {
              if (t !== tool) {
                setTool(t);
                if (t === "custom" || t === "llama.cpp") {
                  setModel("");
                } else {
                  const m = defaultModelForTool(t);
                  setModel(m);
                  if (effort && !effortLevelsForModel(m).includes(effort)) setEffort("");
                }
              }
            }}
            disabled={isDisabled ? isDisabled(t) : undefined}
            className={`px-3 py-2 rounded text-sm font-mono ${
              tool === t
                ? "bg-neutral-700 border border-neutral-500"
                : "bg-neutral-900 border border-neutral-800"
            }${disabledCls}`}
          >
            {t}
          </button>
        ))}
      </div>
    </div>
  );
}
