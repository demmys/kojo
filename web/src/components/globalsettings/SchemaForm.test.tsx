import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { SchemaForm } from "./SchemaForm";
import type { JSONSchema } from "../../lib/extensionsApi";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

const schema: JSONSchema = {
  type: "object",
  required: ["channel"],
  properties: {
    channel: { type: "string", title: "Channel" },
    notes: { type: "string", title: "Notes", multiline: true },
    token: { type: "string", title: "Token", format: "password" },
    retries: { type: "integer", title: "Retries" },
    verbose: { type: "boolean", title: "Verbose" },
    mode: { type: "string", title: "Mode", enum: ["fast", "safe"] },
    weird: { type: "object" as never, title: "Weird" },
  },
};

describe("SchemaForm", () => {
  it("renders one labelled control per property", () => {
    render(<SchemaForm schema={schema} value={{}} onChange={() => {}} />);
    // Required properties are marked so the operator can tell them apart.
    expect(screen.getByLabelText("Channel *")).toBeTruthy();
    expect(screen.getByLabelText("Notes")).toBeTruthy();
    expect(screen.getByLabelText("Retries")).toBeTruthy();
    expect(screen.getByLabelText("Mode")).toBeTruthy();
  });

  it("masks password-format strings", () => {
    render(<SchemaForm schema={schema} value={{}} onChange={() => {}} />);
    expect(screen.getByLabelText("Token").getAttribute("type")).toBe("password");
  });

  it("never shows a stored secret, and removes it only as an explicit null", () => {
    const onChange = vi.fn();
    const { rerender } = render(
      <SchemaForm schema={schema} value={{}} onChange={onChange} secretsSet={["token"]} />,
    );
    const input = screen.getByLabelText("Token") as HTMLInputElement;
    expect(input.value).toBe("");
    expect(input.getAttribute("placeholder")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /削除|Remove/ }));
    expect(onChange).toHaveBeenLastCalledWith({ token: null });
    rerender(
      <SchemaForm schema={schema} value={{ token: null }} onChange={onChange} secretsSet={["token"]} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /取り消す|Undo/ }));
    expect(onChange).toHaveBeenLastCalledWith({ token: "" });
  });

  it("offers no remove button for a secret that is not stored", () => {
    render(<SchemaForm schema={schema} value={{}} onChange={() => {}} />);
    expect(screen.queryByRole("button", { name: /削除|Remove/ })).toBeNull();
  });

  it("emits the edited key without dropping the others", () => {
    const onChange = vi.fn();
    render(
      <SchemaForm schema={schema} value={{ retries: 3 }} onChange={onChange} />,
    );
    fireEvent.change(screen.getByLabelText("Channel *"), { target: { value: "#ops" } });
    expect(onChange).toHaveBeenCalledWith({ retries: 3, channel: "#ops" });
  });

  it("clears a number field to undefined instead of coercing to 0", () => {
    const onChange = vi.fn();
    render(<SchemaForm schema={schema} value={{ retries: 3 }} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText("Retries"), { target: { value: "" } });
    expect(onChange).toHaveBeenCalledWith({ retries: undefined });
  });

  it("parses numeric input as a number, not a string", () => {
    const onChange = vi.fn();
    render(<SchemaForm schema={schema} value={{}} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText("Retries"), { target: { value: "5" } });
    expect(onChange).toHaveBeenCalledWith({ retries: 5 });
  });

  it("toggles booleans", () => {
    const onChange = vi.fn();
    render(<SchemaForm schema={schema} value={{ verbose: false }} onChange={onChange} />);
    fireEvent.click(screen.getByRole("switch"));
    expect(onChange).toHaveBeenCalledWith({ verbose: true });
  });

  it("renders enums as a select with a blank unset option", () => {
    const onChange = vi.fn();
    render(<SchemaForm schema={schema} value={{}} onChange={onChange} />);
    const select = screen.getByLabelText("Mode") as HTMLSelectElement;
    expect(select.tagName).toBe("SELECT");
    expect(select.options.length).toBe(3);
    fireEvent.change(select, { target: { value: "safe" } });
    expect(onChange).toHaveBeenCalledWith({ mode: "safe" });
  });

  it("shows an unsupported placeholder rather than silently skipping a field", () => {
    render(<SchemaForm schema={schema} value={{}} onChange={() => {}} />);
    const control = screen.getByLabelText("Weird") as HTMLInputElement;
    expect(control.disabled).toBe(true);
    expect(control.placeholder).toContain("unsupported");
  });

  it("renders only the description when the schema has no properties", () => {
    render(
      <SchemaForm
        schema={{ type: "object", description: "nothing to configure" }}
        value={{}}
        onChange={() => {}}
      />,
    );
    expect(screen.getByText("nothing to configure")).toBeTruthy();
  });

  it("shows the schema default for an unset property without writing it", () => {
    const onChange = vi.fn();
    const withDefaults: JSONSchema = {
      type: "object",
      properties: {
        limit: { type: "integer", title: "Limit", default: 20 },
        greet: { type: "boolean", title: "Greet", default: true },
      },
    };
    const { rerender } = render(<SchemaForm schema={withDefaults} value={{}} onChange={onChange} />);
    expect((screen.getByLabelText("Limit") as HTMLInputElement).value).toBe("20");
    expect(onChange).not.toHaveBeenCalled();
    // An explicit value wins over the default, including false.
    rerender(<SchemaForm schema={withDefaults} value={{ limit: 5, greet: false }} onChange={onChange} />);
    expect((screen.getByLabelText("Limit") as HTMLInputElement).value).toBe("5");
  });
});
