import { render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useChatScroll } from "./useChatScroll";

type Msg = { id: string };
type Restore = { prevScrollHeight: number; prevScrollTop: number } | null;

function Harness({ messages, convo, restore }: { messages: Msg[]; convo: string; restore?: Restore }) {
  const { scrollContainerRef, suppressAutoScrollRef, scrollRestoreRef } = useChatScroll(messages, convo);
  // Arm the restore branch the way useChatPagination does: the refs are set
  // before the prepend commits, so the hook's layout effect sees them.
  if (restore) {
    suppressAutoScrollRef.current = true;
    scrollRestoreRef.current = restore;
  }
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
let scrollIntoView: ReturnType<typeof vi.fn>;
let scrollTo: ReturnType<typeof vi.fn>;

beforeEach(() => {
  scrollIntoView = vi.fn();
  scrollTo = vi.fn();
  Element.prototype.scrollIntoView = scrollIntoView as unknown as typeof Element.prototype.scrollIntoView;
  Element.prototype.scrollTo = scrollTo as unknown as typeof Element.prototype.scrollTo;
});

afterEach(() => {
  Element.prototype.scrollIntoView = origScrollIntoView;
  Element.prototype.scrollTo = origScrollTo;
});

function mountEmpty(convo = "a") {
  const utils = render(<Harness messages={[]} convo={convo} />);
  const el = utils.getByTestId("scroller");
  const metrics = { scrollHeight: 1000, clientHeight: 400 };
  stubMetrics(el, metrics);
  return { ...utils, el, metrics };
}

describe("useChatScroll", () => {
  it("scrolls only the container itself, never via scrollIntoView", () => {
    const { el, metrics, rerender } = mountEmpty();

    // First populated paint jumps straight to the bottom.
    rerender(<Harness messages={[{ id: "1" }]} convo="a" />);
    expect(el.scrollTop).toBe(1000);

    // At the bottom: content grows → smooth-follow on the scroller.
    el.scrollTop = 600;
    metrics.scrollHeight = 1100;
    rerender(<Harness messages={[{ id: "1" }, { id: "2" }]} convo="a" />);
    expect(scrollTo).toHaveBeenCalledTimes(1);
    expect(scrollTo).toHaveBeenCalledWith({ top: 1100, behavior: "smooth" });
    expect(scrollTo.mock.contexts[0]).toBe(el);

    // Reader scrolled far up: a new message leaves the viewport alone.
    scrollTo.mockClear();
    el.scrollTop = 0;
    metrics.scrollHeight = 1200;
    rerender(<Harness messages={[{ id: "1" }, { id: "2" }, { id: "3" }]} convo="a" />);
    expect(scrollTo).not.toHaveBeenCalled();
    expect(el.scrollTop).toBe(0);

    expect(scrollIntoView).not.toHaveBeenCalled();
  });

  it("re-pins to the bottom when the conversation changes", () => {
    const { el, metrics, rerender } = mountEmpty();
    rerender(<Harness messages={[{ id: "1" }]} convo="a" />);
    el.scrollTop = 0;
    metrics.scrollHeight = 3000;
    rerender(<Harness messages={[{ id: "x" }]} convo="b" />);
    expect(el.scrollTop).toBe(3000);
    expect(scrollTo).not.toHaveBeenCalled();
  });

  it("restores the reader's offset when older rows are prepended", () => {
    const { el, metrics, rerender } = mountEmpty();
    rerender(<Harness messages={[{ id: "2" }]} convo="a" />);
    el.scrollTop = 50;
    metrics.scrollHeight = 1500;
    rerender(
      <Harness
        messages={[{ id: "1" }, { id: "2" }]}
        convo="a"
        restore={{ prevScrollHeight: 1000, prevScrollTop: 50 }}
      />,
    );
    expect(el.scrollTop).toBe(550);
    expect(scrollTo).not.toHaveBeenCalled();
  });
});
