import { describe, expect, it } from "vitest";
import { splitStrictInlineMath } from "./mathDelimiters";

const math = (s: string) => splitStrictInlineMath(s).filter((p) => p.math).map((p) => p.value);

describe("splitStrictInlineMath", () => {
  it("accepts tight single-dollar math", () => {
    expect(math("energy $E=mc^2$ here")).toEqual(["E=mc^2"]);
    expect(math("$x$ and $y$")).toEqual(["x", "y"]);
    expect(math("$5$")).toEqual(["5"]);
  });
  it("rejects currency and loose spans", () => {
    for (const s of ["costs $5 and $10", "$5, $6", "$ x$", "$x $", "$x$5", "\\$x$", "$a\nb$"]) {
      expect(math(s)).toEqual([]);
    }
  });
});
