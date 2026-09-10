import { fireEvent, render, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useFileUpload } from "./useFileUpload";

const uploadMock = vi.fn();
vi.mock("../lib/api", () => ({
  api: { upload: (...args: unknown[]) => uploadMock(...args) },
}));

function Harness({ peerId }: { peerId?: string }) {
  const { fileInputRef, handleFileSelect } = useFileUpload(peerId);
  return <input ref={fileInputRef} type="file" onChange={handleFileSelect} />;
}

afterEach(() => {
  uploadMock.mockReset();
});

describe("useFileUpload", () => {
  it("uploads directly to the current remote holder", async () => {
    uploadMock.mockResolvedValue({ path: "/peer/upload/a.txt", name: "a.txt", size: 1, mime: "text/plain" });
    const { container, rerender } = render(<Harness peerId="peer-a" />);
    const input = container.querySelector("input")!;

    fireEvent.change(input, { target: { files: [new File(["a"], "a.txt", { type: "text/plain" })] } });
    await waitFor(() => expect(uploadMock).toHaveBeenCalledWith(expect.any(File), "peer-a"));

    rerender(<Harness peerId="peer-b" />);
    fireEvent.change(input, { target: { files: [new File(["b"], "b.txt", { type: "text/plain" })] } });
    await waitFor(() => expect(uploadMock).toHaveBeenLastCalledWith(expect.any(File), "peer-b"));
  });
});
