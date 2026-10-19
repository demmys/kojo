import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { AgentSettings } from "./AgentSettings";

// The Integrations section used to hold the built-in Slack bot's settings.
// Slack is an extension package now, so the section (and its nav entry)
// exists only to carry the notice for a bot that has not been moved yet.

const mocks = vi.hoisted(() => ({
  agentGet: vi.fn(),
  checkinFile: vi.fn(),
  userContext: vi.fn(),
  credentialsList: vi.fn(),
  apiCustomModels: vi.fn(),
  getSlackMigration: vi.fn(),
}));

vi.mock("../../hooks/useTTS", () => ({
  useTTSAutoToggle: () => [false, vi.fn()],
  useTTSPlayer: () => ({ play: vi.fn(), state: {} }),
  useTTSCapability: () => null,
}));

vi.mock("../../lib/api", () => ({
  api: { customModels: mocks.apiCustomModels, upload: vi.fn() },
  isThumbSupported: vi.fn(() => false),
}));

vi.mock("../../lib/agentApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/agentApi")>();
  return {
    ...actual,
    agentApi: {
      ...actual.agentApi,
      get: mocks.agentGet,
      getCheckinFile: mocks.checkinFile,
      getUserContext: mocks.userContext,
      avatarUrl: vi.fn((id: string) => `/avatar/${id}`),
      update: vi.fn(),
      credentials: { ...actual.agentApi.credentials, list: mocks.credentialsList },
    },
  };
});

vi.mock("../../lib/slackMigrationApi", () => ({
  getSlackMigration: mocks.getSlackMigration,
  retrySlackMigration: vi.fn(),
}));

function demoAgent() {
  const now = "2026-06-08T00:00:00Z";
  return {
    id: "demo",
    name: "Demo Agent",
    persona: "Demo persona",
    model: "",
    effort: "",
    tool: "claude",
    workDir: "/tmp",
    timeoutMinutes: 10,
    createdAt: now,
    updatedAt: now,
    publicProfile: "",
    publicProfileOverride: false,
    hasAvatar: false,
    etag: "agent-etag",
  };
}

function renderSettings() {
  const router = createMemoryRouter(
    [{ path: "/agents/:id/settings", element: <AgentSettings /> }],
    { initialEntries: ["/agents/demo/settings"] },
  );
  render(<RouterProvider router={router} />);
}

beforeEach(() => {
  Element.prototype.scrollIntoView = vi.fn();
  Element.prototype.scrollTo = vi.fn() as unknown as typeof Element.prototype.scrollTo;
  mocks.agentGet.mockResolvedValue(demoAgent());
  mocks.checkinFile.mockResolvedValue({ value: { content: "", isDefault: true, etag: "" }, etag: "" });
  mocks.userContext.mockResolvedValue({ value: { content: "", isDefault: true, etag: "" }, etag: "" });
  mocks.credentialsList.mockResolvedValue([]);
  mocks.apiCustomModels.mockResolvedValue([]);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("agent settings Integrations section", () => {
  it("is absent when no Slack migration is pending", async () => {
    mocks.getSlackMigration.mockResolvedValue({ pending: [], source: "" });
    renderSettings();
    await waitFor(() => expect(document.getElementById("identity")).not.toBeNull());
    await waitFor(() => expect(mocks.getSlackMigration).toHaveBeenCalled());
    expect(document.getElementById("integrations")).toBeNull();
    expect(screen.queryByRole("button", { name: /Integrations|連携/ })).toBeNull();
  });

  it("is absent when another agent is the one still pending", async () => {
    mocks.getSlackMigration.mockResolvedValue({
      pending: [{ agentId: "other", reason: "install_failed" }],
      source: "https://example.invalid/slack",
    });
    renderSettings();
    await waitFor(() => expect(document.getElementById("identity")).not.toBeNull());
    await waitFor(() => expect(mocks.getSlackMigration).toHaveBeenCalled());
    expect(document.getElementById("integrations")).toBeNull();
  });

  it("goes away by itself once a migration that was in progress finishes", async () => {
    mocks.getSlackMigration
      .mockResolvedValueOnce({
        pending: [{ agentId: "demo", reason: "in_progress" }],
        source: "https://example.invalid/slack",
      })
      .mockResolvedValue({ pending: [], source: "https://example.invalid/slack" });
    renderSettings();
    await waitFor(() => expect(document.getElementById("integrations")).not.toBeNull());
    await waitFor(() => expect(document.getElementById("integrations")).toBeNull(), {
      timeout: 6000,
    });
  }, 10000);

  it("appears with the notice and a nav entry while this agent is pending", async () => {
    mocks.getSlackMigration.mockResolvedValue({
      pending: [{ agentId: "demo", reason: "install_failed", detail: "network down" }],
      source: "https://example.invalid/slack",
    });
    renderSettings();
    await waitFor(() => expect(document.getElementById("integrations")).not.toBeNull());
    expect(screen.getByRole("status").textContent).toContain("network down");
    expect(screen.getAllByRole("button", { name: /Integrations|連携/ }).length).toBeGreaterThan(0);
  });
});
