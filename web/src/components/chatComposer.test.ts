import { describe, expect, it } from "vitest";
import { enterToSend } from "./chatComposer";

// Minimal keydown-event stand-in for the fields enterToSend reads.
function keyEvent(over: {
  key?: string;
  ctrlKey?: boolean;
  metaKey?: boolean;
  shiftKey?: boolean;
  isComposing?: boolean;
}) {
  let prevented = false;
  return {
    e: {
      key: over.key ?? "Enter",
      ctrlKey: over.ctrlKey ?? false,
      metaKey: over.metaKey ?? false,
      shiftKey: over.shiftKey ?? false,
      nativeEvent: { isComposing: over.isComposing ?? false },
      preventDefault: () => {
        prevented = true;
      },
    } as unknown as React.KeyboardEvent,
    wasPrevented: () => prevented,
  };
}

function sends(over: Parameters<typeof keyEvent>[0], enterSends: boolean): boolean {
  let sent = false;
  const { e, wasPrevented } = keyEvent(over);
  enterToSend(e, enterSends, () => {
    sent = true;
  });
  // A send must always preventDefault (no stray newline in the textarea).
  if (sent) expect(wasPrevented()).toBe(true);
  return sent;
}

describe("enterToSend", () => {
  it("Ctrl+Enter sends in both modes", () => {
    expect(sends({ ctrlKey: true }, false)).toBe(true);
    expect(sends({ ctrlKey: true }, true)).toBe(true);
  });

  it("Cmd+Enter (metaKey) sends in both modes", () => {
    expect(sends({ metaKey: true }, false)).toBe(true);
    expect(sends({ metaKey: true }, true)).toBe(true);
  });

  it("plain Enter sends only when enterSends is on", () => {
    expect(sends({}, true)).toBe(true);
    expect(sends({}, false)).toBe(false);
  });

  it("Shift+Enter never sends (newline in both modes)", () => {
    expect(sends({ shiftKey: true }, true)).toBe(false);
    expect(sends({ shiftKey: true }, false)).toBe(false);
  });

  it("Enter during IME composition never sends", () => {
    expect(sends({ isComposing: true }, true)).toBe(false);
    expect(sends({ isComposing: true, ctrlKey: true }, true)).toBe(false);
    expect(sends({ isComposing: true, metaKey: true }, false)).toBe(false);
  });

  it("non-Enter keys are ignored", () => {
    expect(sends({ key: "a", ctrlKey: true }, true)).toBe(false);
  });
});
