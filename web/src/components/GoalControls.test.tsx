import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { GoalControls } from "./GoalControls";
import { get } from "../lib/httpClient";
vi.mock("../lib/httpClient", () => ({ get: vi.fn() }));
afterEach(() => { cleanup(); vi.clearAllMocks(); });
const props = { agentId: "ag_1", sessionKey: "groupdm:g1", enabled: false, onToggle: vi.fn(), running: false, onCommand: vi.fn(), budget: "", onBudget: vi.fn() };
describe("GoalControls", () => {
 it("never loads the main conversation goal for a new draft thread", () => {
  render(<GoalControls {...props} sessionKey={null} />);
  expect(get).not.toHaveBeenCalled();
  expect(screen.getByRole("checkbox")).toBeEnabled();
 });
 it("loads the scoped snapshot and exposes pause while running", async () => {
  vi.mocked(get).mockResolvedValue({ desiredPaused: false, state: { objective: "Fix the bug", status: "active", tokensUsed: 123, tokenBudget: 1000, timeUsedSeconds: 4 } });
  render(<GoalControls {...props} running />);
  await screen.findByText("Fix the bug");
  expect(get).toHaveBeenCalledWith("/api/v1/agents/ag_1/goal?sessionKey=groupdm%3Ag1");
  fireEvent.click(screen.getByText("Pause"));
  expect(props.onCommand).toHaveBeenCalledWith("!goal pause");
  expect(screen.queryByText("Resume")).toBeNull();
  expect(screen.getByRole("checkbox")).toBeDisabled();
 });
 it("retains paused status and offers explicit resume", async () => {
  vi.mocked(get).mockResolvedValue({ desiredPaused: true, state: { objective: "Retained", status: "active", tokensUsed: 456, tokenBudget: null, timeUsedSeconds: 5 } });
  render(<GoalControls {...props} />);
  await screen.findByText("Retained");
  expect(screen.getByText(/paused · 456/)).toBeInTheDocument();
  fireEvent.click(screen.getByText("Resume"));
  expect(props.onCommand).toHaveBeenCalledWith("!goal resume");
 });
 it("does not start work on a status load", async () => {
  vi.mocked(get).mockResolvedValue(null);
  render(<GoalControls {...props} enabled />);
  await screen.findByText(/Native token budgets/);
  expect(props.onCommand).not.toHaveBeenCalled();
  fireEvent.change(screen.getByRole("spinbutton"), { target: { value: "20000" } });
  expect(props.onBudget).toHaveBeenCalledWith("20000");
 });
});
