import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useCustomModels } from "./useCustomModels";

const mocks = vi.hoisted(() => ({ customModels: vi.fn() }));

vi.mock("../../../lib/api", () => ({
  api: { customModels: mocks.customModels },
}));

describe("useCustomModels", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    mocks.customModels.mockReset();
  });

  afterEach(() => vi.useRealTimers());

  it("waits until discovery prerequisites are ready", async () => {
    const setModel = vi.fn();
    const { result, rerender } = renderHook(
      ({ enabled }) => useCustomModels(
        "custom",
        "http://model-host:8080",
        setModel,
        { apiKey: "secret", enabled },
      ),
      { initialProps: { enabled: false } },
    );

    expect(result.current.status).toBe("idle");
    await act(async () => vi.advanceTimersByTime(500));
    expect(mocks.customModels).not.toHaveBeenCalled();

    mocks.customModels.mockResolvedValue(["model-a"]);
    rerender({ enabled: true });
    expect(result.current.status).toBe("loading");
    await act(async () => {
      vi.advanceTimersByTime(300);
      await Promise.resolve();
    });

    expect(mocks.customModels).toHaveBeenCalledWith(
      "http://model-host:8080",
      { agentId: undefined, apiKey: "secret" },
    );
    expect(result.current.status).toBe("success");
    expect(result.current.customModels).toEqual(["model-a"]);
  });

  it("exposes discovery failures instead of silently falling back", async () => {
    mocks.customModels.mockRejectedValue(new Error("endpoint returned 401"));
    const { result } = renderHook(() => useCustomModels(
      "custom",
      "http://model-host:8080",
      vi.fn(),
      { apiKey: "secret", enabled: true },
    ));

    await act(async () => {
      vi.advanceTimersByTime(300);
      await Promise.resolve();
    });

    expect(result.current.status).toBe("error");
    expect(result.current.error).toBe("endpoint returned 401");
    expect(result.current.customModels).toEqual([]);
  });

  it("can explicitly discover an unauthenticated custom endpoint", async () => {
    mocks.customModels.mockResolvedValue(["public-model"]);
    const { result } = renderHook(() => useCustomModels(
      "custom",
      "http://model-host:8080",
      vi.fn(),
      { apiKey: "", enabled: true },
    ));

    await act(async () => {
      vi.advanceTimersByTime(300);
      await Promise.resolve();
    });

    expect(mocks.customModels).toHaveBeenCalledWith(
      "http://model-host:8080",
      { agentId: undefined, apiKey: "" },
    );
    expect(result.current.status).toBe("success");
  });

  it("treats an empty successful response as a visible failure", async () => {
    mocks.customModels.mockResolvedValue([]);
    const { result } = renderHook(() => useCustomModels(
      "custom",
      "http://model-host:8080",
      vi.fn(),
      { apiKey: "secret", enabled: true },
    ));

    await act(async () => {
      vi.advanceTimersByTime(300);
      await Promise.resolve();
    });

    expect(result.current.status).toBe("error");
    expect(result.current.errorCode).toBe("no_models");
    expect(result.current.error).toBe("");
  });
});
