import { render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useChatScroll } from "./useChatScroll";

type Msg = { id: string };

function Harness({ messages, convo }: { messages: Msg[]; convo: string }) {
  const { scrollContainerRef } = useChatScroll(messages, convo);
  return (
    <div ref={scrollContainerRef} data-testid="scroller">
      {messages.map((m) => (
        <div key={m.id}>{m.id}</div>
      ))}
    </div>
  );
}

// jsdom has no layout; give the scroller fixed metrics so the hook's
// near-bottom arithmetic has something to work with.
function stubMetrics(el: HTMLElement, m: { scrollHeight: number; clientHeight: number }) {
  Object.defineProperty(el, "scrollHeight", { configurable: true, get: () => m.scrollHeight });
  Object.defineProperty(el, "clientHeight", { configurable: true, get: () => m.clientHeight });
}

const origScrollIntoView = Element.prototype.scrollIntoView;
const origScrollTo = Element.prototype.scrollTo;

afterEach(() => {
  Element.prototype.scrollIntoView = origScrollIntoView;
  Element.prototype.scrollTo = origScrollTo;
});

describe("useChatScroll", () => {
  it("scrolls only the container, never via scrollIntoView", () => {
    const scrollIntoView = vi.fn();
    Element.prototype.scrollIntoView = scrollIntoView;
    const scrollTo = vi.fn();
    Element.prototype.scrollTo = scrollTo as unknown as typeof Element.prototype.scrollTo;

    // Mount empty so we can stub metrics before the first populated paint.
    const { getByTestId, rerender } = render(<Harness messages={[]} convo="a" />);
    const el = getByTestId("scroller");
    const metrics = { scrollHeight: 1000, clientHeight: 400 };
    stubMetrics(el, metrics);

    // First populated paint jumps straight to the bottom.
    rerender(<Harness messages={[{ id: "1" }]} convo="a" />);
    expect(el.scrollTop).toBe(1000);

    // jsdom clamps nothing, so mimic "at bottom" and grow the content.
    el.scrollTop = 600;
    metrics.scrollHeight = 1100;
    rerender(<Harness messages={[{ id: "1" }, { id: "2" }]} convo="a" />);
    expect(scrollTo).toHaveBeenCalledWith({ top: 1100, behavior: "smooth" });

    // Reader scrolled far up: a new message leaves the viewport alone.
    scrollTo.mockClear();
    el.scrollTop = 0;
    metrics.scrollHeight = 1200;
    rerender(<Harness messages={[{ id: "1" }, { id: "2" }, { id: "3" }]} convo="a" />);
    expect(scrollTo).not.toHaveBeenCalled();

    expect(scrollIntoView).not.toHaveBeenCalled();
  });
});
