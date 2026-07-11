import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ScheduleEditor } from "./ScheduleEditor";

function renderEditor(overrides: Partial<Parameters<typeof ScheduleEditor>[0]> = {}) {
  const props = {
    cronExpr: "*/30 * * * *",
    onCronExprChange: vi.fn(),
    timeoutMinutes: 10,
    onTimeoutChange: vi.fn(),
    silentStart: "",
    silentEnd: "",
    onSilentStartChange: vi.fn(),
    onSilentEndChange: vi.fn(),
    cronMessage: "",
    onCronMessageChange: vi.fn(),
    ...overrides,
  } satisfies Parameters<typeof ScheduleEditor>[0];

  render(<ScheduleEditor {...props} />);
  return props;
}

afterEach(() => cleanup());

describe("ScheduleEditor timeout", () => {
  it("keeps existing preset minute timeout buttons", () => {
    const props = renderEditor();

    fireEvent.click(screen.getByRole("button", { name: "45m" }));

    expect(props.onTimeoutChange).toHaveBeenCalledWith(45);
  });

  it("accepts custom whole-hour timeouts", () => {
    const props = renderEditor();
    const input = screen.getByPlaceholderText("hours");

    fireEvent.change(input, { target: { value: "2" } });

    expect(props.onTimeoutChange).toHaveBeenCalledWith(120);
  });

  it("keeps the 1h preset out of the custom-hours input", () => {
    renderEditor({ timeoutMinutes: 60 });

    expect(screen.getByPlaceholderText("hours")).toHaveValue(null);
  });

  it("keeps decimal custom hours from replacing the saved timeout", () => {
    const props = renderEditor({ timeoutMinutes: 2 * 60 });
    const input = screen.getByPlaceholderText("hours");

    fireEvent.change(input, { target: { value: "1.5" } });
    fireEvent.blur(input);

    expect(input).toHaveValue(2);
    expect(props.onTimeoutChange).not.toHaveBeenCalled();
  });

  it("does not emit custom hours above the duration-safe maximum", () => {
    const props = renderEditor();

    const input = screen.getByPlaceholderText("hours");

    fireEvent.change(input, { target: { value: "2562048" } });
    fireEvent.blur(input);

    expect(props.onTimeoutChange).not.toHaveBeenCalled();
  });

  it("shows a saved custom hour timeout", () => {
    renderEditor({ timeoutMinutes: 3 * 60 });

    expect(screen.getByPlaceholderText("hours")).toHaveValue(3);
  });
});
