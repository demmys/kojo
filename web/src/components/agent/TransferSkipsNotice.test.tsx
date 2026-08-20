import { beforeAll, describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { TransferSkipsNotice } from "./TransferSkipsNotice";
import { setLocale } from "../../lib/i18n";

describe("TransferSkipsNotice", () => {
  beforeAll(() => {
    setLocale("en");
  });

  it("renders nothing for a clean transfer", () => {
    const { container: empty } = render(<TransferSkipsNotice skips={[]} />);
    expect(empty).toBeEmptyDOMElement();
    const { container: absent } = render(<TransferSkipsNotice />);
    expect(absent).toBeEmptyDOMElement();
  });

  it("shows a summary and expands to per-file detail", async () => {
    const user = userEvent.setup();
    render(
      <TransferSkipsNotice
        skips={[
          { path: "big.jsonl", reason: "oversized", sizeBytes: 34 * 1024 * 1024 },
          { path: "main.json", reason: "unreadable_ref" },
        ]}
      />,
    );
    expect(screen.getByText(/Files skipped during transfer: 2/)).toBeInTheDocument();
    // Detail hidden until expanded.
    expect(screen.queryByText(/big\.jsonl/)).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /Files skipped during transfer/ }));
    expect(screen.getByText(/big\.jsonl/)).toBeInTheDocument();
    expect(screen.getByText(/oversized, 34\.0 MiB/)).toBeInTheDocument();
    expect(screen.getByText(/main\.json/)).toBeInTheDocument();
    expect(screen.getByText(/unreadable_ref/)).toBeInTheDocument();
  });

  it("dismisses the notice without opening the agent row", async () => {
    const user = userEvent.setup();
    const onDismiss = vi.fn().mockResolvedValue(undefined);
    render(
      <TransferSkipsNotice
        skips={[{ path: "old.jsonl", reason: "capacity" }]}
        onDismiss={onDismiss}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    await waitFor(() => expect(onDismiss).toHaveBeenCalledOnce());
  });

  it("keeps the notice and reports a failed dismiss", async () => {
    const user = userEvent.setup();
    render(
      <TransferSkipsNotice
        skips={[{ path: "old.jsonl", reason: "capacity" }]}
        onDismiss={() => Promise.reject(new Error("offline"))}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(await screen.findByText("Could not dismiss the warning")).toBeInTheDocument();
    expect(screen.getByText(/Files skipped during transfer: 1/)).toBeInTheDocument();
  });
});
