import { describe, expect, it } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { MarkdownRenderer } from "./MarkdownRenderer";

describe("MarkdownRenderer math", () => {
  it("renders strict inline and display math with KaTeX", async () => {
    const { container } = render(
      <MarkdownRenderer content={"line one\ninline $E=mc^2$ and price $5 and $10\n\n$$\n\\int_0^1 x\\,dx\n$$"} />,
    );
    await waitFor(() => expect(container.querySelector(".katex")).not.toBeNull());
    expect(container.querySelectorAll(".katex-display")).toHaveLength(1);
    expect(container.querySelectorAll(".katex")).toHaveLength(2);
    expect(container.textContent).toContain("price $5 and $10");
  });

  it("leaves currency-only text untouched", () => {
    const { container } = render(<MarkdownRenderer content={"costs $5 and $10"} />);
    expect(container.textContent).toBe("costs $5 and $10");
    expect(container.querySelector(".katex")).toBeNull();
  });

  it("does not touch code or link destinations", async () => {
    const { container } = render(
      <MarkdownRenderer content={"[x](https://e.com/$id$) `$a$`\n\n    echo $b$\n\n$c$"} />,
    );
    await waitFor(() => expect(container.querySelector(".katex")).not.toBeNull());
    expect(container.querySelectorAll(".katex")).toHaveLength(1);
    expect(container.querySelector("a")?.getAttribute("href")).toBe("https://e.com/$id$");
    expect(container.textContent).toContain("$a$");
    expect(container.textContent).toContain("echo $b$");
  });
});
