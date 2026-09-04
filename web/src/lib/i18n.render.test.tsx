import { describe, it, expect, afterEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { useT, setLocale, registerLocale } from "./i18n";

afterEach(() => {
  setLocale("en");
});

function Label() {
  const t = useT();
  return <span data-testid="label">{t("common.cancel")}</span>;
}

describe("useT re-rendering", () => {
  // useSyncExternalStore compares snapshots with Object.is and skips the
  // render when they match. A catalogue arriving does not change the
  // locale, so without a revision in the snapshot the notification is
  // swallowed and the UI keeps showing English forever.
  it("picks up a catalogue that arrives after the first render", () => {
    setLocale("zh-Hans");
    render(<Label />);
    expect(screen.getByTestId("label").textContent).toBe("Cancel");
    act(() => registerLocale("zh-Hans", { "common.cancel": "取消" }));
    expect(screen.getByTestId("label").textContent).toBe("取消");
  });

  it("re-renders on a plain locale switch", () => {
    render(<Label />);
    expect(screen.getByTestId("label").textContent).toBe("Cancel");
    act(() => setLocale("ja"));
    expect(screen.getByTestId("label").textContent).toBe("キャンセル");
  });
});
