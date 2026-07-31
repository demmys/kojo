import { act, renderHook, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useAvatarImageProviders } from "./useAvatarImageProviders";

const mocks = vi.hoisted(() => ({ getKey: vi.fn() }));

vi.mock("../../lib/agentApi", () => ({
  agentApi: { apiKeys: { get: mocks.getKey } },
}));

describe("useAvatarImageProviders", () => {
  beforeEach(() => mocks.getKey.mockReset());

  it("automatically uses the only configured provider", async () => {
    mocks.getKey.mockImplementation((provider: string) =>
      Promise.resolve(
        provider === "gemini"
          ? { provider, configured: true, hasFallback: false }
          : { provider, configured: false, hasFallback: false },
      ),
    );
    const { result } = renderHook(() => useAvatarImageProviders());
    await waitFor(() => expect(result.current.loaded).toBe(true));
    expect(result.current.available).toEqual(["gemini"]);
    expect(result.current.providerForRequest).toBe("gemini");
  });

  it("defaults to GPT Image 2 and allows selection when both are configured", async () => {
    mocks.getKey.mockImplementation((provider: string) =>
      Promise.resolve({ provider, configured: true, hasFallback: false }),
    );
    const { result } = renderHook(() => useAvatarImageProviders());
    await waitFor(() => expect(result.current.available).toEqual(["gemini", "openai"]));
    expect(result.current.providerForRequest).toBe("openai");

    act(() => result.current.setSelected("gemini"));
    expect(result.current.providerForRequest).toBe("gemini");
  });

  it("treats environment and legacy key fallbacks as available", async () => {
    mocks.getKey.mockImplementation((provider: string) =>
      Promise.resolve({ provider, configured: false, hasFallback: provider === "openai" }),
    );
    const { result } = renderHook(() => useAvatarImageProviders());
    await waitFor(() => expect(result.current.loaded).toBe(true));
    expect(result.current.providerForRequest).toBe("openai");
  });

  it("keeps generation disabled when provider status cannot be loaded", async () => {
    mocks.getKey.mockResolvedValue(null);
    const { result } = renderHook(() => useAvatarImageProviders());
    await waitFor(() => expect(result.current.error).toBe(true));
    expect(result.current.loaded).toBe(false);
    expect(result.current.providerForRequest).toBeUndefined();
  });
});
