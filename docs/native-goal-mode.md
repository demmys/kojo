# Native Codex goal mode

Kojo exposes the Codex app-server native thread goal. Only `tool: codex` is
supported; custom Codex providers and other backends are not emulated.
Tested with Codex 0.153.4. An older CLI without the goal API fails explicitly.

## Start and control

- Main WebUI chat and single-agent thread rooms: enable **Goal · Codex native**
  before sending. An optional token budget is available in the composer.
- Slack: `!goal <objective>` (or `!goal start <objective>`).
- Initial budget: `!goal --tokens 20000 <objective>`.
- Commands: `!goal status`, `!goal pause`, `!goal resume`, `!goal clear`,
  `!goal budget 40000`.
- WebUI shows native status, tokens used/budget and elapsed time, with controls.
  Status polling only reads a checkpoint; it never starts a CLI.
- `!stop` / WebUI stop cancels the running turn and fences automatic recovery.
  Pause retains the objective and accounting. Clear removes the goal; it does
  **not** mean the objective was achieved.
- Goal scope is one conversation, not the agent. Unfinished goals must be
  cleared before starting a different objective in that conversation.
- Goal controls do not submit or erase an unrelated composer draft/attachments.

Native token budgets are continuation limits, not hard per-request billing
ceilings: an in-flight turn can exceed its budget. No budget is inferred when
none was requested. `blocked`, `usageLimited`, `budgetLimited` and `complete`
are native terminal states for the current run; explicit resume is needed to
continue an unfinished goal. Completion is reported by the native goal tool,
not detected by matching words in the final answer.

## Execution and recovery

`thread/goal/set` with active status starts native work on an idle thread.
Kojo therefore does **not** also call `turn/start`, and does not implement a
second model-turn continuation loop. It retains the app-server through native
continuations, drains final output and queries final native accounting before
releasing the response adapter.

The thread pointer stores goal binding, original setup context, generation,
response-surface origin and pause intent. Pointer writes are atomic, mode 0600.
The CLI remains authoritative for status and accounting in `goals_1.sqlite`.

On daemon restart, active goals resume through their original main WebUI,
thread-room or Slack response surface. Explicitly stopped goals stay stopped.
Transport disconnects and unfinished CLI failures schedule bounded recovery;
terminal native states do not restart automatically. Repeated admission errors
or repeated CLI failures pause the goal for explicit operator recovery.
Original native conversation state is resumed, never replaced with a fresh
thread after a resume failure. This is not an exactly-once guarantee for tool
side effects: after an interrupted external operation, the agent must inspect
its actual outcome before repeating it.

Remote runs have per-run nonces. User stop is recorded durably on the origin
before requesting a holder-side stop fence. Recovery checks that tombstone at
both admission and actual dispatch, so a lost stop RPC does not authorize a
later restart. Daemon shutdown omits that user-stop tombstone.

## Device migration

Operator-initiated WebUI migration drains the source before snapshotting.
Transfers include the goal binding, native goal database row (including used
budget/time), continuation deferral, thread pointer and native rollout. A
compatible, initialized `goals_1.sqlite` must exist on the target. Old targets
without native-goal transfer support are rejected rather than silently losing
state. Staging has compensating rollback.

**Initial safety restriction:** an active goal cannot migrate itself from a
`kojo-switch-device` tool call. The legacy self-call protocol snapshots the
native rollout before its own tool result and final accounting, and cannot
safely hand that live goal to a second runner. Use the WebUI device switch, or
pause the goal, move the agent, then resume. Supporting autonomous mid-goal
self-migration requires a post-turn native-tail handoff protocol.

## Verification

- Unit tests cover native multi-turn continuation, final-output drain, pause
  fences, failed-command deduplication, guarded arrival, run-specific stop
  identity, wire round-trip and native database rollback/accounting.
- HTTP tests cover access denial and a failed holder-stop RPC whose origin
  tombstone still prevents recovery dispatch.
- Web tests cover conversation scoping, pause/resume and draft-thread isolation.
- Opt-in authenticated smoke tests:
  `KOJO_TEST_CODEX_GOAL=1 go test ./internal/agent -run TestCodexGoalLive -v`.
  They use disposable arithmetic goals, including process-boundary pause/resume
  and user-stop / daemon-shutdown / remote-disconnect cancellation variants.
- Live physical peer migration, browser clicking and Slack delivery still need
  deployment verification; unit/in-process HTTP tests do not substitute for
  those end-to-end checks.
