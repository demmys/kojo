import { beforeAll, describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { SlackMigrationNotice, type SlackMigrationState } from "./SlackMigrationNotice";
import { setLocale } from "../../lib/i18n";

const SOURCE = "https://github.com/loppo-llc/kojo-ext-slack";

function renderNotice(over: Partial<SlackMigrationState>) {
  const state: SlackMigrationState = {
    source: SOURCE,
    retrying: false,
    retryError: "",
    retry: vi.fn(),
    ...over,
  };
  const view = render(
    <MemoryRouter>
      <SlackMigrationNotice state={state} />
    </MemoryRouter>,
  );
  return { state, ...view };
}

describe("SlackMigrationNotice", () => {
  beforeAll(() => {
    setLocale("en");
  });

  it("renders nothing when the agent has nothing left to move", () => {
    const { container } = renderNotice({});
    expect(container).toBeEmptyDOMElement();
  });

  it("explains why the move is pending and how to finish it by hand", () => {
    renderNotice({
      pending: {
        agentId: "ag_1",
        reason: "install_failed",
        detail: "git fetch: could not resolve host github.com",
      },
    });
    expect(screen.getByText("Slack has moved to an extension package")).toBeInTheDocument();
    expect(screen.getByText(/The Slack package could not be installed/)).toBeInTheDocument();
    expect(screen.getByText(/could not resolve host/)).toBeInTheDocument();
    // The manual route: where to install from, and where to do it.
    expect(screen.getByText(SOURCE)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open Extensions settings" })).toHaveAttribute(
      "href",
      "/settings",
    );
  });

  it("has a specific explanation for a package from another source", () => {
    renderNotice({ pending: { agentId: "ag_1", reason: "untrusted_source" } });
    expect(screen.getByText(/the tokens were not handed to it/)).toBeInTheDocument();
  });

  it("retries on request", async () => {
    const user = userEvent.setup();
    const { state } = renderNotice({ pending: { agentId: "ag_1", reason: "install_failed" } });
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(state.retry).toHaveBeenCalledOnce();
  });

  it("disables the button while a retry runs and reports a retry that could not be sent", () => {
    renderNotice({
      pending: { agentId: "ag_1", reason: "install_failed" },
      retrying: true,
      retryError: "offline",
    });
    expect(screen.getByRole("button", { name: "Retrying..." })).toBeDisabled();
    expect(screen.getByText("The retry could not be run: offline")).toBeInTheDocument();
  });
});
