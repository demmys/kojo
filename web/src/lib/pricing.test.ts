import { describe, it, expect } from "vitest";
import { priceModel, estimateTurnCost } from "./pricing";

describe("priceModel", () => {
  it("resolves aliases to canonical pricing", () => {
    expect(priceModel("opus")).toEqual(priceModel("claude-opus-4-8"));
    expect(priceModel("sonnet")).toEqual(priceModel("claude-sonnet-5"));
    expect(priceModel("haiku")).toEqual(priceModel("claude-haiku-4-5"));
  });

  it("prices canonical claude ids", () => {
    expect(priceModel("claude-opus-4-8")).toEqual({
      input: 5,
      output: 25,
      cacheRead: 0.5,
      cacheWrite: 6.25,
    });
    expect(priceModel("claude-fable-5")?.input).toBe(10);
    expect(priceModel("claude-sonnet-4-6")?.output).toBe(15);
  });

  it("returns undefined for unpriced / unknown models", () => {
    expect(priceModel("grok-build")).toBeUndefined();
    expect(priceModel("gpt-5.5")).toBeUndefined();
    expect(priceModel("fable-5")).toBeUndefined();
    expect(priceModel("")).toBeUndefined();
    expect(priceModel(undefined)).toBeUndefined();
  });
});

describe("estimateTurnCost", () => {
  it("sums input, output, and cache buckets at their own rates", () => {
    // Opus 4.8: input 5, output 25, cacheRead 0.5, cacheWrite 6.25 per 1M.
    // 1M in, 1M out, 1M cacheRead, 1M cacheWrite = 5 + 25 + 0.5 + 6.25.
    const cost = estimateTurnCost("claude-opus-4-8", {
      inputTokens: 1_000_000,
      outputTokens: 1_000_000,
      cacheReadInputTokens: 1_000_000,
      cacheCreationInputTokens: 1_000_000,
    });
    expect(cost).toBeCloseTo(36.75, 6);
  });

  it("handles missing cache fields as zero", () => {
    // Sonnet 5: input 3, output 15 per 1M. 100k in, 200k out.
    const cost = estimateTurnCost("claude-sonnet-5", {
      inputTokens: 100_000,
      outputTokens: 200_000,
    });
    expect(cost).toBeCloseTo(0.3 + 3.0, 6);
  });

  it("returns undefined for unpriced models", () => {
    expect(
      estimateTurnCost("grok-build", { inputTokens: 100, outputTokens: 100 }),
    ).toBeUndefined();
  });

  it("returns undefined when usage is absent", () => {
    expect(estimateTurnCost("claude-opus-4-8", undefined)).toBeUndefined();
  });
});
