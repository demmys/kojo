import { describe, expect, it } from "vitest";
import type { AgentMessage, ChatEvent } from "../../lib/agentApi";
import {
  appendSystemErrorIfNew,
  appendUniqueMessage,
  applyDoneMessage,
  applySubagentEvent,
  applyToolResult,
  dropEchoedPending,
  matchToolForResult,
  newToolFromEvent,
  toToolUse,
  type StreamingTool,
} from "./chatEventReducer";

const msg = (id: string, role: AgentMessage["role"], content = ""): AgentMessage => ({
  id,
  role,
  content,
  timestamp: "2026-05-16T12:00:00Z",
});

const tool = (id: string, name: string, output: string | null = null): StreamingTool => ({
  id,
  name,
  input: "",
  output,
});

describe("matchToolForResult", () => {
  it("matches by id when toolUseId is present", () => {
    const m = matchToolForResult("u1", "Bash");
    expect(m(tool("u1", "Different"))).toBe(true);
    expect(m(tool("u2", "Bash"))).toBe(false);
  });

  it("matches by name + null output when toolUseId is empty", () => {
    const m = matchToolForResult("", "Bash");
    expect(m(tool("", "Bash"))).toBe(true);
    expect(m(tool("", "Bash", "already filled"))).toBe(false);
    expect(m(tool("", "Edit"))).toBe(false);
  });
});

describe("applyToolResult", () => {
  it("fills the most recent matching tool_use by id", () => {
    const prev: StreamingTool[] = [tool("u1", "Bash"), tool("u1", "Bash")];
    const after = applyToolResult(prev, {
      type: "tool_result",
      toolUseId: "u1",
      toolOutput: "done",
    });
    // The tail entry is the one that gets filled — Codex's two-Bash
    // workflow assigns the more recent invocation the inbound result.
    expect(after[0].output).toBeNull();
    expect(after[1].output).toBe("done");
  });

  it("fills by name + null-output when no id is supplied", () => {
    const prev: StreamingTool[] = [tool("", "Bash", "first"), tool("", "Bash")];
    const after = applyToolResult(prev, {
      type: "tool_result",
      toolName: "Bash",
      toolOutput: "second",
    });
    expect(after[0].output).toBe("first");
    expect(after[1].output).toBe("second");
  });

  it("is a no-op for orphan tool_result with no match", () => {
    const prev: StreamingTool[] = [tool("u1", "Bash", "done")];
    const after = applyToolResult(prev, {
      type: "tool_result",
      toolUseId: "u-orphan",
      toolOutput: "lost",
    });
    expect(after).toEqual(prev);
  });

  it("returns a copy for non-tool_result events (never mutates)", () => {
    const prev: StreamingTool[] = [tool("u1", "Bash")];
    const after = applyToolResult(prev, { type: "text", delta: "x" } as ChatEvent);
    expect(after).not.toBe(prev);
    expect(after).toEqual(prev);
  });

  it("treats undefined toolOutput as empty string", () => {
    const prev: StreamingTool[] = [tool("u1", "Bash")];
    const after = applyToolResult(prev, {
      type: "tool_result",
      toolUseId: "u1",
    });
    expect(after[0].output).toBe("");
  });
});

describe("newToolFromEvent", () => {
  it("builds a streaming tool entry from a tool_use event", () => {
    const built = newToolFromEvent({
      type: "tool_use",
      toolUseId: "u1",
      toolName: "Bash",
      toolInput: '{"cmd":"ls"}',
    });
    expect(built).toEqual({ id: "u1", name: "Bash", input: '{"cmd":"ls"}', output: null });
  });

  it("returns null when toolName is missing", () => {
    expect(newToolFromEvent({ type: "tool_use", toolUseId: "u1" })).toBeNull();
  });

  it("returns null for non-tool_use events", () => {
    expect(newToolFromEvent({ type: "text", delta: "x" } as ChatEvent)).toBeNull();
  });

  it("defaults id and input to empty strings when absent", () => {
    const built = newToolFromEvent({ type: "tool_use", toolName: "Bash" });
    expect(built).toEqual({ id: "", name: "Bash", input: "", output: null });
  });
});

describe("appendUniqueMessage", () => {
  it("appends when the id is new", () => {
    const prev = [msg("a", "user"), msg("b", "assistant")];
    const after = appendUniqueMessage(prev, msg("c", "assistant"));
    expect(after.map((m) => m.id)).toEqual(["a", "b", "c"]);
  });

  it("returns the SAME array reference when the id already exists (React fast-path)", () => {
    const prev = [msg("a", "user"), msg("b", "assistant")];
    const after = appendUniqueMessage(prev, msg("a", "user", "different content"));
    expect(after.map((m) => m.id)).toEqual(["a", "b"]);
    expect(after[0].content).toBe(""); // original kept, NOT replaced
    expect(after).toBe(prev);
  });
});

describe("appendSystemErrorIfNew", () => {
  const nowMs = () => 1700000000000;
  const ts = () => "2026-05-16T12:00:00Z";

  it("appends a synthesized system entry when the tail is not the same error", () => {
    const prev = [msg("a", "user", "hi")];
    const after = appendSystemErrorIfNew(prev, "⚠️ Error: oops", nowMs, ts);
    expect(after.length).toBe(2);
    expect(after[1]).toMatchObject({
      role: "system",
      content: "⚠️ Error: oops",
      timestamp: "2026-05-16T12:00:00Z",
    });
    expect(after[1].id).toBe("error_1700000000000");
  });

  it("returns the SAME array reference when the trailing entry is an identical system error", () => {
    const prev: AgentMessage[] = [
      msg("a", "user", "hi"),
      msg("error_old", "system", "⚠️ Error: oops"),
    ];
    const after = appendSystemErrorIfNew(prev, "⚠️ Error: oops", nowMs, ts);
    expect(after).toBe(prev); // React state setter fast-paths on identity
  });

  it("does NOT dedupe when the tail is a non-system entry with matching content", () => {
    const prev = [
      msg("a", "user", "⚠️ Error: oops"), // happens to match content but role differs
    ];
    const after = appendSystemErrorIfNew(prev, "⚠️ Error: oops", nowMs, ts);
    expect(after.length).toBe(2);
  });
});

describe("dropEchoedPending", () => {
  it("drops a pending_ row echoed back by the fetched page", () => {
    const local = [msg("pending_1", "user", "hello")];
    const fetched = [msg("srv1", "user", "hello")];
    expect(dropEchoedPending(local, fetched)).toEqual([]);
  });

  it("keeps a pending_ row with no echo in the fetched page", () => {
    const local = [msg("pending_1", "user", "still in flight")];
    const fetched = [msg("srv1", "user", "something else")];
    expect(dropEchoedPending(local, fetched)).toEqual(local);
  });

  it("requires role to match, not just content", () => {
    const local = [msg("pending_1", "user", "hello")];
    const fetched = [msg("srv1", "assistant", "hello")];
    expect(dropEchoedPending(local, fetched)).toEqual(local);
  });

  it("leaves error_/aborted_ synthetics alone even on content match", () => {
    const local = [
      msg("error_1", "system", "boom"),
      msg("aborted_1", "assistant", "partial"),
    ];
    const fetched = [
      msg("srv1", "system", "boom"),
      msg("srv2", "assistant", "partial"),
    ];
    expect(dropEchoedPending(local, fetched)).toEqual(local);
  });

  it("drops only the echoed rows from a mixed list", () => {
    const local = [
      msg("pending_1", "user", "echoed"),
      msg("pending_2", "user", "not yet"),
    ];
    const fetched = [msg("srv1", "user", "echoed")];
    expect(dropEchoedPending(local, fetched)).toEqual([msg("pending_2", "user", "not yet")]);
  });

  it("does not let an old identical row eat a fresh in-flight send", () => {
    // pending sent at 2026-05-16T12:00:00Z; fetched row persisted an
    // hour earlier — same text, but it is NOT this send's echo.
    const sentMs = new Date("2026-05-16T12:00:00Z").getTime();
    const local = [msg(`pending_${sentMs}`, "user", "ok")];
    const old = { ...msg("srv1", "user", "ok"), timestamp: "2026-05-16T11:00:00Z" };
    expect(dropEchoedPending(local, [old])).toEqual(local);
  });

  it("drops against a row within the clock-skew slack window", () => {
    // Server clock 30 s behind the client: still recognized as the echo.
    const sentMs = new Date("2026-05-16T12:00:00Z").getTime();
    const local = [msg(`pending_${sentMs}`, "user", "ok")];
    const echoed = { ...msg("srv1", "user", "ok"), timestamp: "2026-05-16T11:59:30Z" };
    expect(dropEchoedPending(local, [echoed])).toEqual([]);
  });

  it("keeps a pending row whose attachments differ from the fetched row", () => {
    const att = { path: "/up/a.png", name: "a.png", size: 10, mime: "image/png" };
    const local = [{ ...msg("pending_1", "user", "see file"), attachments: [att] }];
    const fetched = [msg("srv1", "user", "see file")]; // no attachments
    expect(dropEchoedPending(local, fetched)).toEqual(local);
  });

  it("matches attachments by name+size, ignoring server-relocated paths", () => {
    const local = [{
      ...msg("pending_1", "user", ""),
      attachments: [{ path: "/local/a.png", name: "a.png", size: 10, mime: "image/png" }],
    }];
    const fetched = [{
      ...msg("srv1", "user", ""),
      attachments: [{ path: "/srv/data/a.png", name: "a.png", size: 10, mime: "image/png" }],
    }];
    expect(dropEchoedPending(local, fetched)).toEqual([]);
  });

  it("consumes each fetched row at most once for identical pendings", () => {
    const local = [
      msg("pending_1", "user", "ok"),
      msg("pending_2", "user", "ok"),
    ];
    const fetched = [msg("srv1", "user", "ok")];
    expect(dropEchoedPending(local, fetched)).toEqual([msg("pending_2", "user", "ok")]);
  });
});

describe("applyDoneMessage", () => {
  const done = (id: string, content = "ok"): ChatEvent => ({
    type: "done",
    message: msg(id, "assistant", content),
  });

  it("appends with dedupe when there is no abort marker", () => {
    const prev = [msg("a", "user")];
    const after = applyDoneMessage(prev, done("b"), null);
    expect(after.map((m) => m.id)).toEqual(["a", "b"]);
  });

  it("upgrades the abort marker in place when the server delivers a new id", () => {
    const prev = [msg("abort-1", "assistant", "<aborted>")];
    const after = applyDoneMessage(prev, done("real-1", "full reply"), "abort-1");
    expect(after.length).toBe(1);
    expect(after[0].id).toBe("real-1");
    expect(after[0].content).toBe("full reply");
  });

  it("drops the abort marker when the server's id is already in the transcript", () => {
    const prev = [
      msg("real-1", "assistant", "earlier completion"),
      msg("abort-1", "assistant", "<aborted>"),
    ];
    const after = applyDoneMessage(prev, done("real-1", "now arriving"), "abort-1");
    expect(after.map((m) => m.id)).toEqual(["real-1"]);
    expect(after[0].content).toBe("earlier completion"); // existing entry NOT clobbered
  });

  it("returns the SAME array reference when event.message is absent", () => {
    const prev = [msg("a", "user")];
    const after = applyDoneMessage(prev, { type: "done" }, null);
    expect(after).toBe(prev);
  });

  it("returns the SAME array reference for non-done events", () => {
    const prev = [msg("a", "user")];
    const after = applyDoneMessage(prev, { type: "text", delta: "x" } as ChatEvent, null);
    expect(after).toBe(prev);
  });
});

describe("applySubagentEvent", () => {
  it("nests a subagent tool_use under the matching top-level Task tool", () => {
    const prev: StreamingTool[] = [tool("task1", "Task")];
    const after = applySubagentEvent(prev, {
      type: "tool_use",
      toolUseId: "sub1",
      toolName: "Bash",
      toolInput: "ls /tmp",
      parentToolUseId: "task1",
    });
    expect(after[0].children).toEqual([{ id: "sub1", name: "Bash", input: "ls /tmp", output: null }]);
    // Top-level list untouched otherwise.
    expect(after[0].id).toBe("task1");
  });

  it("fills subagent tool output by matching id within children", () => {
    const withChild: StreamingTool[] = [
      { ...tool("task1", "Task"), children: [tool("sub1", "Bash")] },
    ];
    const after = applySubagentEvent(withChild, {
      type: "tool_result",
      toolUseId: "sub1",
      toolName: "Bash",
      toolOutput: "file1\nfile2",
      parentToolUseId: "task1",
    });
    expect(after[0].children?.[0].output).toBe("file1\nfile2");
  });

  it("accumulates subagent narrative text into a trailing text-bubble child", () => {
    let cur: StreamingTool[] = [tool("task1", "Task")];
    cur = applySubagentEvent(cur, { type: "text", delta: "Listing ", parentToolUseId: "task1" });
    cur = applySubagentEvent(cur, { type: "text", delta: "/tmp", parentToolUseId: "task1" });
    expect(cur[0].children).toEqual([{ id: "", name: "", input: "", output: null, text: "Listing /tmp" }]);
  });

  it("is a no-op copy when the parent tool hasn't been seen yet", () => {
    const prev: StreamingTool[] = [];
    const after = applySubagentEvent(prev, {
      type: "tool_use",
      toolUseId: "sub1",
      toolName: "Bash",
      parentToolUseId: "task1",
    });
    expect(after).toEqual([]);
    expect(after).not.toBe(prev);
  });

  it("is a no-op copy when parentToolUseId is absent", () => {
    const prev: StreamingTool[] = [tool("task1", "Task")];
    const after = applySubagentEvent(prev, { type: "text", delta: "x" });
    expect(after).toEqual(prev);
    expect(after).not.toBe(prev);
  });
});

describe("toToolUse", () => {
  it("normalizes a null output to empty string and recurses into children", () => {
    const st: StreamingTool = {
      ...tool("task1", "Task"),
      children: [tool("sub1", "Bash", "done")],
    };
    expect(toToolUse(st)).toEqual({
      id: "task1",
      name: "Task",
      input: "",
      output: "",
      text: undefined,
      children: [{ id: "sub1", name: "Bash", input: "", output: "done", text: undefined, children: undefined }],
    });
  });
});
