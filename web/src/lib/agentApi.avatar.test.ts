import { afterEach, expect, it, vi } from "vitest";
import { agentApi } from "./agentApi";

vi.mock("./auth", () => ({
  getOwnerToken: vi.fn(() => ""),
  appendTokenQuery: vi.fn((url: string) => url),
}));

afterEach(() => vi.restoreAllMocks());

it("sends the selected image provider and dedicated avatar prompt", async () => {
  const fetchSpy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify({
        avatarPath: "/temp/avatar.png",
        provider: "openai",
        fallback: false,
      }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    ),
  );

  const result = await agentApi.generateAvatar(
    "persona",
    "Toumon",
    "not humanoid",
    "/temp/previous.png",
    "openai",
  );

  expect(result.provider).toBe("openai");
  const [url, init] = fetchSpy.mock.calls[0]!;
  expect(url).toBe("/api/v1/agents/generate-avatar");
  expect(JSON.parse(String((init as RequestInit).body))).toEqual({
    persona: "persona",
    name: "Toumon",
    prompt: "not humanoid",
    previousPath: "/temp/previous.png",
    provider: "openai",
    allowFallback: true,
  });
});
