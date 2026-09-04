/** Per-backend model whitelist and defaults. */

export interface ToolModelConfig {
  default: string;
  models: string[];
}

export const toolModels: Record<string, ToolModelConfig> = {
  claude: {
    default: "sonnet",
    models: ["sonnet", "claude-sonnet-5", "claude-sonnet-4-6", "opus", "claude-opus-5", "claude-fable-5-1", "claude-fable-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "haiku"],
  },
  codex: {
    default: "gpt-5.6-sol",
    models: [
      "gpt-5.6-sol",
      "gpt-5.6-terra",
      "gpt-5.6-luna",
      "gpt-5.5",
      "gpt-5.4",
      "gpt-5.4-mini",
      "gpt-5.3-codex",
      "gpt-5.2",
    ],
  },
  grok: {
    default: "grok-4.6",
    models: ["grok-4.6", "grok-4.5"],
  },
  // The custom-* backends have no fixed model list: the operator supplies
  // the endpoint and useCustomModels fetches whatever it advertises.
  "custom-claude": {
    default: "",
    models: [],
  },
  "custom-codex": {
    default: "",
    models: [],
  },
  "custom-bare": {
    default: "",
    models: [],
  },
};

/** Return the default model for a given tool. */
export function defaultModelForTool(tool: string): string {
  return toolModels[tool]?.default ?? "sonnet";
}

/** Return available models for a given tool. */
export function modelsForTool(tool: string): string[] {
  return toolModels[tool]?.models ?? [];
}

/** Effort levels shared by Claude/Grok. Codex models before gpt-5.6 omit "max". */
export const effortLevels = ["low", "medium", "high", "xhigh", "max"] as const;
export type EffortLevel = (typeof effortLevels)[number];

/** Models that support the xhigh effort level. */
const xhighModels = new Set(["opus", "claude-sonnet-5", "claude-opus-5", "claude-fable-5-1", "claude-fable-5", "claude-opus-4-8", "claude-opus-4-7"]);
const codexEffortModels = new Set(toolModels.codex.models);
// codex CLI 0.144.1 models_cache.json: the gpt-5.6 family advertises
// low/medium/high/xhigh/max (sol and terra also list "ultra", which kojo's
// effort scale doesn't model). Older gpt-5.x models stop at xhigh.
// Keep in sync with agent.go codexMaxEffortModels.
const codexMaxModels = new Set(["gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"]);
// gpt-5.6-sol advertises default_reasoning_level "low"; every other codex
// model defaults to medium.
const codexLowDefaultModels = new Set(["gpt-5.6-sol"]);
// grok CLI 1.0.3 models_cache.json: grok-4.6 lists efforts
// [xhigh,high,medium,low]; grok-4.5 lists [high,medium,low]. Neither
// advertises "max". Keep in sync with agent.go grokEffortModels /
// grokXhighModels.
const grokEffortModels = new Set(toolModels.grok.models);
const grokXhighModels = new Set(["grok-4.6"]);

/**
 * Models whose default effort is xhigh (rather than high).
 * Opus 5 / 4.8 and both Fable models support xhigh and max but default to
 * high; only Opus 4.7 defaults to xhigh. The "opus" alias is treated as
 * Opus 5, so it defaults to high. grok-4.6 advertises low/medium/high/xhigh and grok-4.5
 * low/medium/high; both carry reasoning_effort "high" as the CLI default,
 * so neither is listed here.
 */
const defaultXhighModels = new Set(["claude-opus-4-7"]);

export function supportsEffort(tool: string): boolean {
  return tool === "claude" || tool === "grok" || tool === "codex";
}

// codex CLI 0.144.1: gpt-5.6-sol and gpt-5.6-terra additionally advertise
// the "ultra" reasoning level (multi-agent orchestration mode). It is a
// different beast from the plain effort ladder — long-running and
// expensive — so kojo's per-agent effort scale intentionally stops at
// "max"; ultra is offered ONLY as a per-session choice in NewSession.
const codexUltraModels = new Set(["gpt-5.6-sol", "gpt-5.6-terra"]);

/**
 * Effort levels offered when starting an ad-hoc session for the given
 * model: the model's regular ladder plus "ultra" where the codex CLI
 * advertises it. NOT used for agent settings.
 */
export function sessionEffortLevelsForModel(model: string): string[] {
  const levels: string[] = [...effortLevelsForModel(model)];
  if (codexUltraModels.has(model)) levels.push("ultra");
  return levels;
}

/** Return available effort levels for a given model. */
export function effortLevelsForModel(model: string): readonly EffortLevel[] {
  if (codexMaxModels.has(model)) return effortLevels;
  if (codexEffortModels.has(model)) return ["low", "medium", "high", "xhigh"] as const;
  if (grokXhighModels.has(model)) return ["low", "medium", "high", "xhigh"] as const;
  if (grokEffortModels.has(model)) return ["low", "medium", "high"] as const;
  if (xhighModels.has(model)) return effortLevels;
  return effortLevels.filter((e) => e !== "xhigh");
}

/** Return the default effort level label for a given model. */
export function defaultEffortForModel(model: string): string {
  if (codexLowDefaultModels.has(model)) return "low";
  if (codexEffortModels.has(model)) return "medium";
  return defaultXhighModels.has(model) ? "xhigh" : "high";
}
