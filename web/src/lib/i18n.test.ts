import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import {
  t,
  setLocale,
  getLocale,
  registerLocale,
  availableLocales,
  bootstrapLocales,
} from "./i18n";

// Every test leaves the module-level locale where it found it: i18n is a
// singleton by design, so a stray "zh-Hans" would leak into other suites.
afterEach(() => {
  setLocale("en");
  vi.unstubAllGlobals();
  localStorage.clear();
});

beforeEach(() => {
  setLocale("en");
});

describe("builtin locales", () => {
  it("switches between the compiled-in languages", () => {
    setLocale("ja");
    expect(getLocale()).toBe("ja");
    expect(t("common.cancel")).toBe("キャンセル");
    setLocale("en");
    expect(t("common.cancel")).toBe("Cancel");
  });
});

describe("extension locales", () => {
  it("uses the overlay and falls back to English for missing keys", () => {
    registerLocale("zh-Hans", { "common.cancel": "取消" });
    setLocale("zh-Hans");
    expect(t("common.cancel")).toBe("取消");
    // Not in the overlay: English rather than the raw key, so a partial
    // translation is still a usable UI.
    expect(t("common.close")).toBe("Close");
  });

  it("interpolates params in an overlaid string", () => {
    registerLocale("zh-Hant", { "common.removeName": "移除 {name}" });
    setLocale("zh-Hant");
    expect(t("common.removeName", { name: "demo" })).toBe("移除 demo");
  });
});

describe("bootstrapLocales", () => {
  it("ignores a response that a newer call has already superseded", async () => {
    // Two overlapping calls: the first resolves last, carrying the
    // older list. Without generation tracking it would resurrect the
    // language the second call reported as gone.
    let resolveFirst: (v: Response) => void = () => {};
    const responses = [
      new Promise<Response>((r) => {
        resolveFirst = r;
      }),
      Promise.resolve(
        new Response(JSON.stringify([{ tag: "zh-Hant", name: "繁體中文" }]), {
          status: 200,
        }),
      ),
    ];
    let call = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(() => responses[Math.min(call++, responses.length - 1)]),
    );
    const stale = bootstrapLocales();
    const fresh = bootstrapLocales();
    await fresh;
    resolveFirst(
      new Response(JSON.stringify([{ tag: "zh-Hans", name: "简体中文" }]), {
        status: 200,
      }),
    );
    await stale;
    expect(availableLocales().map((l) => l.tag)).toEqual([
      "ja",
      "en",
      "zh-Hant",
    ]);
  });

  it("lists contributed languages and loads the active one", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url === "/api/v1/locales") {
          return new Response(
            JSON.stringify([{ tag: "zh-Hans", name: "简体中文" }]),
            { status: 200 },
          );
        }
        return new Response(JSON.stringify({ "common.cancel": "取消" }), {
          status: 200,
        });
      }),
    );
    setLocale("zh-Hans");
    await bootstrapLocales();
    expect(availableLocales().map((l) => l.tag)).toEqual([
      "ja",
      "en",
      "zh-Hans",
    ]);
    expect(t("common.cancel")).toBe("取消");
  });

  it("drops catalogues whose package is gone", async () => {
    registerLocale("zh-Hant", { "common.cancel": "取消" });
    setLocale("zh-Hant");
    expect(t("common.cancel")).toBe("取消");
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("[]", { status: 200 })),
    );
    await bootstrapLocales();
    expect(availableLocales().map((l) => l.tag)).toEqual(["ja", "en"]);
    expect(t("common.cancel")).toBe("Cancel");
  });

  it("ignores a catalogue that lands after its package was removed", async () => {
    // The catalogue fetch is started by setLocale and outlives the list
    // refresh that pruned the language. Registering it anyway would bring
    // back a language the instance no longer offers.
    let resolveCatalogue: (v: Response) => void = () => {};
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url === "/api/v1/locales")
          return new Response("[]", { status: 200 });
        return new Promise<Response>((r) => {
          resolveCatalogue = r;
        });
      }),
    );
    setLocale("zh-Hans");
    await bootstrapLocales();
    resolveCatalogue(
      new Response(JSON.stringify({ "common.cancel": "取消" }), {
        status: 200,
      }),
    );
    await new Promise((r) => setTimeout(r, 0));
    expect(t("common.cancel")).toBe("Cancel");
  });

  it("keeps an in-flight catalogue when the list refresh fails", async () => {
    // A failed refresh replaced nothing, so the language is still offered
    // and the catalogue already on its way is still the right one.
    let resolveCatalogue: (v: Response) => void = () => {};
    let listCalls = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url === "/api/v1/locales") {
          if (listCalls++ === 0) {
            return new Response(
              JSON.stringify([{ tag: "zh-Hans", name: "简体中文" }]),
              { status: 200 },
            );
          }
          throw new Error("offline");
        }
        return new Promise<Response>((r) => {
          resolveCatalogue = r;
        });
      }),
    );
    await bootstrapLocales();
    setLocale("zh-Hans");
    await bootstrapLocales();
    resolveCatalogue(
      new Response(JSON.stringify({ "common.cancel": "取消" }), {
        status: 200,
      }),
    );
    await new Promise((r) => setTimeout(r, 0));
    expect(t("common.cancel")).toBe("取消");
  });

  it("applies a slow list even if a later refresh failed meanwhile", async () => {
    // The second call errors out and replaces nothing, so the first call's
    // response is still the freshest list there is.
    let resolveList: (v: Response) => void = () => {};
    let calls = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        if (calls++ === 0) {
          return new Promise<Response>((r) => {
            resolveList = r;
          });
        }
        throw new Error("offline");
      }),
    );
    const slow = bootstrapLocales();
    await bootstrapLocales();
    resolveList(
      new Response(JSON.stringify([{ tag: "zh-Hant", name: "繁體中文" }]), {
        status: 200,
      }),
    );
    await slow;
    expect(availableLocales().map((l) => l.tag)).toEqual([
      "ja",
      "en",
      "zh-Hant",
    ]);
  });

  it("leaves the builtin languages working when the server errors", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new Error("offline");
      }),
    );
    await bootstrapLocales();
    setLocale("ja");
    expect(t("common.cancel")).toBe("キャンセル");
  });
});
