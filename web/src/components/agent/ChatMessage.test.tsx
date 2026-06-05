import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AttachmentList } from "./ChatMessage";
import type { AgentMessageAttachment } from "../../lib/agentApi";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function imageAttachment(path: string, name: string): AgentMessageAttachment {
  return {
    path,
    name,
    size: 1234,
    mime: "image/png",
  };
}

describe("AttachmentList image preview", () => {
  it("wraps the preview caption at the rendered media width", async () => {
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({
      x: 0,
      y: 0,
      width: 160,
      height: 120,
      top: 0,
      right: 160,
      bottom: 120,
      left: 0,
      toJSON: () => ({}),
    } as DOMRect);

    const path = "/tmp/generated-images/a-very-long-preview-caption-that-should-wrap-instead-of-being-truncated.png";
    render(<AttachmentList attachments={[imageAttachment(path, "preview.png")]} isUser={false} />);

    fireEvent.click(screen.getByTitle("preview.png"));

    const caption = await screen.findByText(path);
    expect(screen.getByAltText(path)).toHaveClass("mx-auto");
    expect(caption).not.toHaveClass("truncate");
    expect(caption).toHaveClass("wrap-anywhere");
    await waitFor(() => expect(caption).toHaveStyle({ width: "160px" }));
  });

  it("moves between previewable attachments with arrow keys", async () => {
    const first = imageAttachment("/tmp/first.png", "first.png");
    const second = imageAttachment("/tmp/second.png", "second.png");
    render(<AttachmentList attachments={[first, second]} isUser={false} />);

    fireEvent.click(screen.getByTitle(first.name));
    expect(await screen.findByText(first.path)).toBeInTheDocument();

    fireEvent.keyDown(window, { key: "ArrowRight" });
    expect(await screen.findByText(second.path)).toBeInTheDocument();

    fireEvent.keyDown(window, { key: "ArrowLeft" });
    expect(await screen.findByText(first.path)).toBeInTheDocument();

    fireEvent.keyDown(window, { key: "Escape" });
    await waitFor(() => expect(screen.queryByText(first.path)).not.toBeInTheDocument());
  });
});
