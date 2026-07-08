import {
  defaultModelForTool,
  effortLevelsForModel,
  type EffortLevel,
} from "../../../lib/toolModels";
import { Field } from "../../ui/Field";
import { useT } from "../../../lib/i18n";

const TOOLS = ["claude", "codex", "grok", "custom", "llama.cpp"];

/**
 * The "Tool" (backend) selector. Switching tools resets the model to the
 * backend default (or clears it for custom/llama.cpp) and drops an effort
 * level the new default model can't support.
 *
 * `isDisabled` is optional: when provided (AgentCreate gates on server
 * availability) each button gets a `disabled` attribute plus a dimmed style;
 * when omitted (AgentSettings) neither is rendered.
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
  const t = useT();
  return (
    <Field label={t("field.tool")}>
      <div className="flex flex-wrap gap-2">
        {TOOLS.map((name) => {
          const selected = tool === name;
          return (
            <button
              key={name}
              type="button"
              onClick={() => {
                if (name !== tool) {
                  setTool(name);
                  if (name === "custom" || name === "llama.cpp") {
                    setModel("");
                  } else {
                    const m = defaultModelForTool(name);
                    setModel(m);
                    if (effort && !effortLevelsForModel(m).includes(effort)) setEffort("");
                  }
                }
              }}
              disabled={isDisabled ? isDisabled(name) : undefined}
              className={`rounded-lg border px-3 py-2 font-mono text-[13px] transition-colors disabled:opacity-30 ${
                selected
                  ? "border-copper bg-copper/15 text-copper-bright"
                  : "border-hairline bg-raised text-ink-dim hover:border-ink-faint hover:text-ink"
              }`}
            >
              {name}
            </button>
          );
        })}
      </div>
    </Field>
  );
}
