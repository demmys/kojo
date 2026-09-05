import { beforeAll, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { UserQuestionCard } from "./UserQuestionCard";
import { setLocale } from "../../lib/i18n";

describe("UserQuestionCard", () => {
  beforeAll(() => setLocale("en"));
  it("answers by Codex ID, preserving identical question texts", async () => {
    const submit = vi.fn().mockResolvedValue(undefined);
    render(<UserQuestionCard pending={{ requestId: "r", questions: [
      { id: "first", question: "Choose?", options: [{ label: "Blue" }] },
      { id: "second", question: "Choose?", options: [{ label: "Green" }] },
    ] }} onSubmit={submit} />);
    await userEvent.click(screen.getByRole("button", { name: "Blue" }));
    await userEvent.click(screen.getByRole("button", { name: "Green" }));
    await userEvent.click(screen.getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(submit).toHaveBeenCalledWith({ first: "Blue", second: "Green" }));
  });
  it("retains Claude question-text keys and free text", async () => {
    const submit = vi.fn().mockResolvedValue(undefined);
    render(<UserQuestionCard pending={{ requestId: "r", questions: [
      { question: "Your answer?" },
    ] }} onSubmit={submit} />);
    await userEvent.type(screen.getByRole("textbox"), "A custom answer");
    await userEvent.click(screen.getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(submit).toHaveBeenCalledWith({ "Your answer?": "A custom answer" }));
  });
});
