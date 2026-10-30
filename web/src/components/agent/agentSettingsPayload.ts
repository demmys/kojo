import type { AgentUpdateParams } from "../../lib/agentApi";
import { supportsEffort } from "../../lib/toolModels";

/**
 * Snapshot of every form field that contributes to the AgentSettings
 * save payload. Stays a plain data object so buildAgentSavePayload
 * can be unit-tested without rendering the form. Keep this in sync
 * with the useState declarations at the top of AgentSettings.tsx —
 * if a field is added to the form, add it here too and let TypeScript
 * surface the missing wire-up.
 */
export interface AgentSettingsFormState {
  name: string;
  persona: string;
  publicProfile: string;
  publicProfileOverride: boolean;
  model: string;
  effort: string;
  tool: string;
  customBaseURL: string;
  thinkingMode: string;
  responseLanguage: string;
  workDir: string;
  cronExpr: string;
  timeoutMinutes: number;
  resumeIdleMinutes: number;
  backgroundMaxMinutes: number;
  silentStart: string;
  silentEnd: string;
  notifyDuringSilent: boolean;
  cronMessage: string;
  allowedTools: string[];
  allowProtectedPaths: string[];
  tts: {
    enabled: boolean;
    provider?: "gemini" | "grok";
    model: string;
    voice: string;
    stylePrompt: string;
  };
  disabledInjections: string[];
  autoEffort: boolean;
  /**
   * Explicit codex "apps" choice to send, or undefined to leave the
   * stored value (and the per-tool default) untouched.
   */
  codexApps?: boolean;
  /** gemini maxOutputTokens; 0 = server default. */
  maxOutputTokens: number;
}

/** Server default for custom-opencode maxOutputTokens (Go CustomOpenCodeDefaultMaxOutputTokens). */
export const CUSTOM_OPENCODE_DEFAULT_MAX_OUTPUT_TOKENS = 8192;

/** True for the tools whose output cap the maxOutputTokens setting drives. */
export function supportsMaxOutputTokens(tool: string): boolean {
  return tool === "gemini-api" || tool === "custom-opencode";
}

/** Server default maxOutputTokens for the tool. */
export function defaultMaxOutputTokensFor(tool: string): number {
  return tool === "custom-opencode"
    ? CUSTOM_OPENCODE_DEFAULT_MAX_OUTPUT_TOKENS
    : GEMINI_DEFAULT_MAX_OUTPUT_TOKENS;
}

/** Server default for gemini maxOutputTokens (Go GeminiDefaultMaxOutputTokens). */
export const GEMINI_DEFAULT_MAX_OUTPUT_TOKENS = 65536;
/** Server upper bound (Go GeminiMaxOutputTokensLimit). */
export const GEMINI_MAX_OUTPUT_TOKENS_LIMIT = 65536;

/** True for the backends driven by the codex CLI (codexApps applies). */
export function isCodexCLITool(tool: string): boolean {
  return tool === "codex" || tool === "custom-codex";
}

/** Effective codexApps when the agent has no explicit value. */
export function defaultCodexApps(tool: string): boolean {
  return tool === "codex";
}

/**
 * True when `tool` requires the operator to supply a CustomBaseURL.
 * All three custom-* backends point at an operator-supplied endpoint:
 * custom-claude drives it with the claude CLI (Anthropic Messages API),
 * custom-codex with the codex CLI (OpenAI-compatible), and custom-bare
 * has kojo speak OpenAI-compatible HTTP to it directly with no CLI and
 * no tools. Legacy names ("custom", "llama.cpp") are still accepted here
 * so a settings screen loaded against an un-migrated agent row behaves.
 */
export function needsCustomURLFor(tool: string): boolean {
  return (
    tool === "custom-claude" ||
    tool === "custom-codex" ||
    tool === "custom-bare" ||
    tool === "custom" ||
    tool === "llama.cpp"
  );
}

/**
 * True when the tool stores a CustomBaseURL (and an endpoint-bound custom
 * API key): every needsCustomURLFor tool plus custom-opencode, whose base
 * URL is one of the server's whitelisted provider presets rather than
 * free text (model discovery only for its local presets; see
 * discoversCustomModels).
 */
export function usesCustomBaseURL(tool: string): boolean {
  return needsCustomURLFor(tool) || tool === "custom-opencode";
}

/**
 * True when baseURL is a custom-opencode local preset (Ollama / LM Studio /
 * llama-server on http://127.0.0.1:<port>/v1). Those run unauthenticated
 * and the server lists their models from <baseURL>/models.
 */
export function isLocalOpenCodeBaseURL(baseURL: string): boolean {
  try {
    const u = new URL(baseURL.trim());
    // URL drops an explicit :80, so an empty port is fine; the server
    // re-validates the exact preset shape anyway.
    return u.protocol === "http:" && u.hostname === "127.0.0.1" && u.pathname === "/v1" &&
      !u.search && !u.hash && !u.username && !u.password;
  } catch {
    return false;
  }
}

/**
 * Whether the model field is populated from the endpoint's model list:
 * every free-URL custom backend, plus custom-opencode on a local preset.
 */
export function discoversCustomModels(tool: string, baseURL: string): boolean {
  return needsCustomURLFor(tool) || (tool === "custom-opencode" && isLocalOpenCodeBaseURL(baseURL));
}

/**
 * Build the PATCH /agents/{id} body from form state. Pure — no I/O,
 * no React state, no clocks. Mirrors the conditional shape the
 * server expects:
 *
 *   - name / persona / workDir / cronMessage / model: always trimmed
 *   - publicProfile: only included when publicProfileOverride is true
 *   - effort: included only when the chosen tool supports an effort
 *     selector (otherwise the field would silently lock in an
 *     incompatible value).
 *   - customBaseURL: trimmed and included only for tools that need
 *     it; otherwise undefined so the server clears the field.
 *   - thinkingMode: only emitted for custom-bare.
 *   - responseLanguage: always sent, trimmed ("" = auto).
 *   - allowedTools: only emitted for custom-claude (where the
 *     operator picks per-tool permissions).
 *   - allowProtectedPaths: only emitted for claude / custom-claude — those
 *     are the only backends whose Bash/Edit gating respects the
 *     server-side allowlist.
 *   - maxOutputTokens: only emitted for gemini / custom-opencode (0 = default).
 *   - tts: nested object; empty strings collapse to undefined so the
 *     server treats them as "use default".
 */
export function buildAgentSavePayload(state: AgentSettingsFormState): AgentUpdateParams {
  const trimmed = {
    name: state.name.trim(),
    persona: state.persona.trim(),
    model: state.model.trim(),
    tool: state.tool.trim(),
    workDir: state.workDir.trim(),
  };
  const publicProfilePart = state.publicProfileOverride
    ? { publicProfile: state.publicProfile.trim() }
    : {};
  return {
    name: trimmed.name,
    persona: trimmed.persona,
    ...publicProfilePart,
    publicProfileOverride: state.publicProfileOverride,
    model: trimmed.model,
    effort: supportsEffort(state.tool) ? state.effort : undefined,
    tool: trimmed.tool,
    customBaseURL: usesCustomBaseURL(state.tool) ? state.customBaseURL.trim() : undefined,
    thinkingMode: state.tool === "custom-bare" ? state.thinkingMode : undefined,
    responseLanguage: state.responseLanguage.trim(),
    workDir: trimmed.workDir,
    cronExpr: state.cronExpr,
    timeoutMinutes: state.timeoutMinutes,
    resumeIdleMinutes: state.resumeIdleMinutes,
    backgroundMaxMinutes: state.backgroundMaxMinutes,
    silentStart: state.silentStart,
    silentEnd: state.silentEnd,
    notifyDuringSilent: state.notifyDuringSilent,
    cronMessage: state.cronMessage,
    allowedTools: state.tool === "custom-claude" ? state.allowedTools : undefined,
    allowProtectedPaths:
      state.tool === "claude" || state.tool === "custom-claude"
        ? state.allowProtectedPaths
        : undefined,
    tts: {
      enabled: state.tts.enabled,
      provider: state.tts.provider === "grok" ? "grok" : undefined,
      model: state.tts.model || undefined,
      voice: state.tts.voice || undefined,
      stylePrompt: state.tts.stylePrompt.trim() || undefined,
    },
    disabledInjections: state.disabledInjections,
    autoEffort: state.autoEffort,
    codexApps: isCodexCLITool(state.tool) ? state.codexApps : undefined,
    maxOutputTokens: supportsMaxOutputTokens(state.tool) ? state.maxOutputTokens : undefined,
  };
}
