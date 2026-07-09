// Per-model API pricing, in USD per million tokens.
//
// Anthropic source: the claude-api skill's authoritative pricing table (cached
// 2026-06-24) plus the standard cache multipliers documented in
// shared/prompt-caching.md:
//   - cache read      = 0.1x  the base input rate
//   - cache write 5m  = 1.25x the base input rate
//
// Anthropic base input/output rates (USD / 1M tokens):
//   claude-fable-5   : 10 / 50
//   claude-opus-4-8  :  5 / 25
//   claude-opus-4-7  :  5 / 25
//   claude-opus-4-6  :  5 / 25
//   claude-sonnet-5  :  3 / 15   (standard rate; an intro $2/$10 runs
//                                 through 2026-08-31 — we bill the standard
//                                 rate so the figure is stable past that date)
//   claude-sonnet-4-6:  3 / 15
//   claude-haiku-4-5 :  1 /  5
//
// xAI source: https://docs.x.ai/developers/models/grok-4.5 and
// https://docs.x.ai/developers/pricing (fetched 2026-07-10).
// xAI publishes an explicit cached-input rate and has no cache-write surcharge
// (cacheCreation tokens bill at the plain input rate when reported).
//   grok-4.5: input $2.00 / cached input $0.50 / output $6.00 per 1M
//
// Models not in the table (fable-5 bare alias, grok-composer-*, gpt-*, custom,
// llama.cpp, "") return undefined from priceModel → no cost is shown.
// grok-composer-2.5-fast has no authoritative public API rate as of 2026-07-10.

export interface ModelPricing {
  /** USD per 1M input (uncached) tokens. */
  input: number;
  /** USD per 1M output tokens. */
  output: number;
  /** USD per 1M cache-read input tokens. */
  cacheRead: number;
  /** USD per 1M cache-write / cache-creation input tokens. */
  cacheWrite: number;
}

/** Anthropic: cache read 0.1x, cache write 5m 1.25x base input. */
function pricedAnthropic(input: number, output: number): ModelPricing {
  return {
    input,
    output,
    cacheRead: input * 0.1,
    cacheWrite: input * 1.25,
  };
}

/**
 * xAI: explicit cached-input rate from docs; no cache-write surcharge so
 * cacheCreation tokens (if reported) bill at the plain input rate.
 */
function pricedXai(
  input: number,
  output: number,
  cacheRead: number,
): ModelPricing {
  return {
    input,
    output,
    cacheRead,
    cacheWrite: input,
  };
}

// Keyed by the canonical (full) model id.
const CANONICAL_PRICING: Record<string, ModelPricing> = {
  "claude-fable-5": pricedAnthropic(10, 50),
  "claude-opus-4-8": pricedAnthropic(5, 25),
  "claude-opus-4-7": pricedAnthropic(5, 25),
  "claude-opus-4-6": pricedAnthropic(5, 25),
  "claude-sonnet-5": pricedAnthropic(3, 15),
  "claude-sonnet-4-6": pricedAnthropic(3, 15),
  "claude-haiku-4-5": pricedAnthropic(1, 5),
  // xAI — https://docs.x.ai/developers/models/grok-4.5 (2026-07-10)
  "grok-4.5": pricedXai(2, 6, 0.5),
};

// kojo agent.model aliases (see web/src/lib/toolModels.ts) mapped to a
// canonical id. "opus" → Opus 4.8, "sonnet" → Sonnet 5, "haiku" → Haiku 4.5.
const ALIASES: Record<string, string> = {
  opus: "claude-opus-4-8",
  sonnet: "claude-sonnet-5",
  haiku: "claude-haiku-4-5",
};

/**
 * Resolve a kojo agent.model value to its pricing, or undefined when the
 * model is unpriced (composer/gpt/codex, custom, llama.cpp, unknown aliases).
 */
export function priceModel(model: string | undefined): ModelPricing | undefined {
  if (!model) return undefined;
  const canonical = ALIASES[model] ?? model;
  return CANONICAL_PRICING[canonical];
}

export interface TurnUsage {
  inputTokens: number;
  outputTokens: number;
  cacheReadInputTokens?: number;
  cacheCreationInputTokens?: number;
  /** Backend-reported exact cost (Claude CLI total_cost_usd), covering
   *  subagent usage and per-model rates. Preferred over the estimate. */
  costUSD?: number;
}

/**
 * Approximate USD cost of a single turn's token usage for the given model.
 * Returns undefined when the model has no known pricing so the caller can
 * suppress the cost display entirely.
 *
 * Cache-read and cache-creation tokens are billed separately from plain
 * input tokens; `inputTokens` here is the uncached remainder (matches the
 * server's Usage struct / the API's usage.input_tokens semantics), so the
 * three input buckets are summed at their own rates and not double-counted.
 */
export function estimateTurnCost(
  model: string | undefined,
  usage: TurnUsage | undefined,
): number | undefined {
  if (!usage) return undefined;
  // A backend-reported cost is exact (includes subagent usage billed at
  // each model's own rate) — always prefer it over the estimate.
  if (usage.costUSD && usage.costUSD > 0) return usage.costUSD;
  const p = priceModel(model);
  if (!p) return undefined;
  const input = usage.inputTokens || 0;
  const output = usage.outputTokens || 0;
  const cacheRead = usage.cacheReadInputTokens || 0;
  const cacheWrite = usage.cacheCreationInputTokens || 0;
  return (
    (input * p.input +
      output * p.output +
      cacheRead * p.cacheRead +
      cacheWrite * p.cacheWrite) /
    1_000_000
  );
}
