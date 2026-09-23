import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { GoalControls } from "./GoalControls";
import { get } from "../lib/httpClient";
vi.mock("../lib/httpClient", () => ({ get: vi.fn() }));
afterEach(() => { cleanup(); vi.clearAllMocks(); });
const props = { agentId: "ag_1", sessionKey: "groupdm:g1", enabled: false, onToggle: vi.fn(), running: false, onCommand: vi.fn(), budget: "", onBudget: vi.fn() };
const openPopover = () => fireEvent.click(screen.getByRole("button", { name: /Goal mode/ }));
describe("GoalControls", () => {
 it("never loads the main conversation goal for a new draft thread", () => {
  render(<GoalControls {...props} sessionKey={null} />);
  expect(get).not.toHaveBeenCalled();
  openPopover();
  expect(screen.getByRole("switch")).toBeEnabled();
 });
 it("keeps the popover closed until the button is pressed", () => {
  vi.mocked(get).mockResolvedValue(null);
  render(<GoalControls {...props} />);
  expect(screen.queryByRole("dialog")).toBeNull();
  openPopover();
  const dialog = screen.getByRole("dialog");
  expect(dialog).toHaveFocus();
  fireEvent.keyDown(dialog, { key: "Escape" });
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(screen.getByRole("button", { name: /Goal mode/ })).toHaveFocus();
 });
 it("closes on an outside click and when focus leaves", () => {
  vi.mocked(get).mockResolvedValue(null);
  render(<><GoalControls {...props} /><button>elsewhere</button></>);
  openPopover();
  fireEvent.mouseDown(document.body);
  expect(screen.queryByRole("dialog")).toBeNull();
  openPopover();
  fireEvent.focusOut(screen.getByRole("dialog"), { relatedTarget: screen.getByRole("switch") });
  expect(screen.getByRole("dialog")).toBeInTheDocument();
  fireEvent.focusOut(screen.getByRole("switch"), { relatedTarget: screen.getByText("elsewhere") });
  expect(screen.queryByRole("dialog")).toBeNull();
 });
 it("loads the thread goal and offers pause while running", async () => {
  vi.mocked(get).mockResolvedValue({ desiredPaused: false, state: { objective: "Fix the bug", status: "active", tokensUsed: 123, tokenBudget: 1000, timeUsedSeconds: 4 } });
  render(<GoalControls {...props} running />);
  openPopover();
  await screen.findByText("Fix the bug");
  expect(get).toHaveBeenCalledWith("/api/v1/agents/ag_1/goal?sessionKey=groupdm%3Ag1");
  fireEvent.click(screen.getByText("Pause"));
  expect(props.onCommand).toHaveBeenCalledWith("!goal pause");
  expect(screen.queryByText("Resume")).toBeNull();
  expect(screen.getByRole("switch")).toBeDisabled();
 });
 it("retains paused status and offers explicit resume", async () => {
  vi.mocked(get).mockResolvedValue({ desiredPaused: true, state: { objective: "Retained", status: "active", tokensUsed: 456, tokenBudget: null, timeUsedSeconds: 5 } });
  render(<GoalControls {...props} />);
  openPopover();
  await screen.findByText("Retained");
  expect(screen.getByText(/paused · 456/)).toBeInTheDocument();
  fireEvent.click(screen.getByText("Resume"));
  expect(props.onCommand).toHaveBeenCalledWith("!goal resume");
 });
 it("does not start work on a status load", async () => {
  vi.mocked(get).mockResolvedValue(null);
  render(<GoalControls {...props} enabled />);
  openPopover();
  await screen.findByText(/Native token budgets/);
  expect(props.onCommand).not.toHaveBeenCalled();
  fireEvent.change(screen.getByRole("spinbutton"), { target: { value: "20000" } });
  expect(props.onBudget).toHaveBeenCalledWith("20000");
 });
});
