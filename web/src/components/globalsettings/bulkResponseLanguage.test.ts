import { describe, expect, it, vi } from "vitest";
import { PreconditionFailedError } from "../../lib/httpClient";
import { applyResponseLanguageToAll, bulkFailureReason } from "./bulkResponseLanguage";

const get = vi.fn(async (id: string) => ({ etag: "e-" + id }));

describe("applyResponseLanguageToAll", () => {
  it("PATCHes each non-archived agent, continues past failures, reports them", async () => {
    const update = vi.fn(async (id: string) => {
      if (id === "b") {
        throw new Error('502: {"error":{"code":"peer_offline","message":"holder peer is offline"}}');
      }
      return {};
    });
    const res = await applyResponseLanguageToAll(
      [
        { id: "a", name: "A" },
        { id: "b", name: "B" },
        { id: "c", name: "C", archived: true },
        { id: "d", name: "D" },
      ],
      "  English ",
      { get, update },
    );
    expect(update.mock.calls.map((c) => c[0])).toEqual(["a", "b", "d"]);
    expect(update).toHaveBeenCalledWith("a", { responseLanguage: "English" }, "e-a");
    expect(res.updated).toBe(2);
    expect(res.failed).toEqual([{ id: "b", name: "B", reason: "peer_offline: holder peer is offline" }]);
  });

  it("sends empty string for auto", async () => {
    const update = vi.fn(async () => ({}));
    await applyResponseLanguageToAll([{ id: "a", name: "A" }], "   ", { get, update });
    expect(update).toHaveBeenCalledWith("a", { responseLanguage: "" }, "e-a");
  });

  it("retries once on 412 with a re-read etag", async () => {
    let n = 0;
    const g = vi.fn(async () => ({ etag: "v" + ++n }));
    const update = vi.fn(async (_id: string, _cfg: unknown, etag?: string) => {
      if (etag === "v1") throw new PreconditionFailedError("etag mismatch");
      return {};
    });
    const res = await applyResponseLanguageToAll([{ id: "a", name: "A" }], "ja", { get: g, update });
    expect(update).toHaveBeenLastCalledWith("a", { responseLanguage: "ja" }, "v2");
    expect(res.updated).toBe(1);
  });
});

describe("bulkFailureReason", () => {
  it("falls back to the raw message", () => {
    expect(bulkFailureReason(new Error("network down"))).toBe("network down");
    expect(bulkFailureReason(new Error("500: oops"))).toBe("500: oops");
  });
});
