# Interactive questions

Claude (`AskUserQuestion`) and Codex (`item/tool/requestUserInput`) can ask
questions while a Kojo turn is running. This is separate from steering: a
question answer resolves the original tool request, never a new chat turn.

## Slack setup and use

- Enable **Interactivity & Shortcuts → Interactivity** in the Slack app settings.
  Socket Mode carries the interactions; a public Request URL is not needed.
- Update both the Hub and runtime peers to use interactive questions remotely.
  Older peers ignore the opt-in header and retain ordinary, non-interactive chat.
- When a question appears in the original Slack thread, choose **回答する**.
  The modal offers single/multiple selection and free text. Free text overrides
  a single selection, or is appended to multiple selections.
- Only the Slack user who initiated that turn can answer its form. This identity
  is retained for the turn's post-handoff continuation. Other users' ordinary
  thread replies are not interpreted as question answers.
- Closing the modal does not deny the question. Reopen it from the card, or use
  `!stop` to cancel the running turn.
- Answered, expired and stopped questions lose their buttons. A daemon restart
  invalidates old cards even if Slack still displays a button.
- On a transport error, Kojo does not automatically replay the answer. If the
  turn is still active, the user can explicitly retry the same question ID.
  The holder accepts each request at most once; late answers cannot target a
  new turn. A broken CLI answer pipe terminates that CLI rather than leaving a
  consumed question blocked indefinitely. If the original answer was accepted but its acknowledgement was
  lost, a retry is reported as already answered/expired.
- Secret-input prompts are not supported. Do not enter passwords or tokens in
  these forms: ordinary answers are recorded in conversation history.

## Implementation notes

`OneShotOpts.InteractiveQuestions` is a per-response-surface capability. It is
only enabled by adapters that can display and answer questions. Other one-shot
callers keep their previous non-interactive behavior.

The shared `UserQuestion` contract has an optional `id`: Codex answers use that
ID, while Claude answers retain the original question-text key. Codex JSON-RPC
IDs remain opaque (both strings and numbers are echoed exactly); the UI sees a
fresh prompt UUID, preventing collisions after a CLI restart. Codex server-side
resolution notifications invalidate prompts. Nonblocking Codex questions remain
answerable while work continues, without marking the agent as blocked. Kojo only
adds a fallback timer for unwatched, automated blocking turns.

External answers share the existing holder-fenced, no-automatic-replay input
transport (`external-chat/steer`), with an exclusive `question` envelope instead
of `content`. The receiver verifies the agent, session, originating Hub and
pending request before invoking the captured backend answer function. Answers
are not downgraded to steering or FIFO messages on errors. Question lifecycle
events use reliable delivery rather than lossy text-delta forwarding.

Slack acknowledges interactions before opening a modal or contacting a peer.
Network work is bounded independently from the Socket Mode event loop. Accepted
answers also join the turn's ordered history so a handoff can retain the Q&A.

References:
- [Slack Socket Mode interactions](https://docs.slack.dev/apis/events-api/using-socket-mode/)
- [Codex App Server](https://learn.chatgpt.com/docs/app-server)
