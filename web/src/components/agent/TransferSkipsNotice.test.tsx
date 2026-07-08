import { beforeAll, describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
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

    await user.click(screen.getByRole("button"));
    expect(screen.getByText(/big\.jsonl/)).toBeInTheDocument();
    expect(screen.getByText(/oversized, 34\.0 MiB/)).toBeInTheDocument();
    expect(screen.getByText(/main\.json/)).toBeInTheDocument();
    expect(screen.getByText(/unreadable_ref/)).toBeInTheDocument();
  });
});
