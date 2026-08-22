import { describe, it, expect } from "vitest";
import {
  CONTEXT_INJECTION_KEYS,
  TOOL_ONLY_INJECTION_KEYS,
  injectionSupportedByTool,
  toolHasAgenticTools,
} from "./agentApi";

describe("injectionSupportedByTool", () => {
  it("keeps every section for tool-capable backends", () => {
    for (const tool of ["claude", "codex", "grok", "custom-claude", "custom-codex"]) {
      expect(toolHasAgenticTools(tool)).toBe(true);
      for (const key of CONTEXT_INJECTION_KEYS) {
        expect(injectionSupportedByTool(key, tool)).toBe(true);
      }
    }
  });

  it.each(["custom-bare", "llama.cpp"])(
    "drops only the tool-gated sections for %s",
    (tool) => {
      expect(toolHasAgenticTools(tool)).toBe(false);
      const unsupported = CONTEXT_INJECTION_KEYS.filter(
        (key) => !injectionSupportedByTool(key, tool),
      );
      expect([...unsupported].sort()).toEqual([...TOOL_ONLY_INJECTION_KEYS].sort());
    },
  );

  it("only lists known injection keys as tool-gated", () => {
    for (const key of TOOL_ONLY_INJECTION_KEYS) {
      expect(CONTEXT_INJECTION_KEYS).toContain(key);
    }
  });
});
