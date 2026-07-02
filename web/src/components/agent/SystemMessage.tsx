import { useState } from "react";
import type { AgentMessage } from "../../lib/agentApi";
import { formatTime } from "../../lib/utils";

// Match the legacy "[Group DM: <name>] New message from <sender> at <timestamp>."
// header. Older sessions still have these rendered in their transcripts.
const GROUP_DM_LEGACY_RE = /^\[Group DM: (.+)\] New message from (.+?)(?:\s+at\s+\S+)?\.?\n/;

// Match the current "[Group DM: <name>] N new message(s) from <sender>."
// header. The "from <sender>" suffix is server-emitted into the trusted
// header so we never have to parse the untrusted message block to find
// the latest sender. The suffix is optional to stay compatible with any
// transcripts captured before the suffix landed.
//
// The sender capture uses `.*?` (not `.+?`) so a header whose sender field
// resolved to an empty string — e.g. " from ." emitted when the recipient's
// view of the sender's display name was blank (hard-deleted agent, or a
// member loaded before the agents-JOIN ran) — still matches and renders
// as a pill. Without this the regex falls through and the entire 10KB
// notification body is shown raw with no close/expand toggle.
const GROUP_DM_BATCH_RE = /^\[Group DM: (.+?)\] (\d+) new message\(s\)(?: from (.*?))?\./;

/**
 * Compact pill for group-DM notifications. The full notification body now
 * runs ~10KB (inline message bodies + reply curl + truncation footer), so
 * we render a collapsed pill by default and let the user click to inspect
 * the raw payload. New batch format and legacy single-message format share
 * this widget.
 */
function GroupDMNotificationPill({
  message,
  groupName,
  sender,
  count,
}: {
  message: AgentMessage;
  groupName: string;
  sender?: string;
  count?: number;
}) {
  const [expanded, setExpanded] = useState(false);
  // Treat an empty-string sender the same as "missing" — the server emits
  // " from ." when the resolved display name is blank, which leaves `sender`
  // as "" after the regex capture rather than undefined.
  const senderLabel = sender && sender.trim() !== "" ? sender : "?";
  return (
    <div className="flex justify-center my-1.5">
      <div className="flex flex-col items-center max-w-[90%] w-full">
        <button
          type="button"
          onClick={() => setExpanded((v) => !v)}
          className="flex items-center gap-1.5 px-3 py-1 rounded-full bg-neutral-900/60 border border-neutral-800 hover:border-neutral-700 text-[11px] text-neutral-500 transition-colors cursor-pointer"
          title={expanded ? "Hide notification body" : "Show notification body"}
        >
          <svg className="w-3 h-3 shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M20.25 8.511c.884.284 1.5 1.128 1.5 2.097v4.286c0 1.136-.847 2.1-1.98 2.193-.34.027-.68.052-1.02.072v3.091l-3-3c-1.354 0-2.694-.055-4.02-.163a2.115 2.115 0 01-.825-.242m9.345-8.334a2.126 2.126 0 00-.476-.095 48.64 48.64 0 00-8.048 0c-1.131.094-1.976 1.057-1.976 2.192v4.286c0 .837.46 1.58 1.155 1.951m9.345-8.334V6.637c0-1.621-1.152-3.026-2.76-3.235A48.455 48.455 0 0011.25 3c-2.115 0-4.198.137-6.24.402-1.608.209-2.76 1.614-2.76 3.235v6.226c0 1.621 1.152 3.026 2.76 3.235.577.075 1.157.14 1.74.194V21l4.155-4.155" />
          </svg>
          <span>
            <span className="text-neutral-400">{senderLabel}</span> &rarr; {groupName}
          </span>
          {count != null && count > 1 && (
            <span className="text-neutral-600">{count} msgs</span>
          )}
          <span className="text-neutral-600">{formatTime(message.timestamp)}</span>
          <svg
            className={`w-3 h-3 transition-transform ${expanded ? "rotate-90" : ""}`}
            fill="none"
            viewBox="0 0 24 24"
            stroke="currentColor"
            strokeWidth={2}
          >
            <path strokeLinecap="round" strokeLinejoin="round" d="M9 5l7 7-7 7" />
          </svg>
        </button>
        {expanded && (
          <pre className="mt-1.5 w-full min-w-0 px-3 py-2 rounded bg-neutral-900/80 border border-neutral-800 text-[10px] leading-relaxed text-neutral-400 whitespace-pre-wrap wrap-anywhere overflow-x-auto">
            {message.content}
          </pre>
        )}
      </div>
    </div>
  );
}

/** System / error messages -- centered, distinct styling */
export function SystemMessage({ message }: { message: AgentMessage }) {
  const isError = message.content.startsWith("⚠️ Error:");

  // New batch-format group DM notification — collapse by default.
  if (!isError) {
    const batch = GROUP_DM_BATCH_RE.exec(message.content);
    if (batch) {
      const [, groupName, countStr, sender] = batch;
      return (
        <GroupDMNotificationPill
          message={message}
          groupName={groupName}
          sender={sender}
          count={parseInt(countStr, 10)}
        />
      );
    }
    const legacy = GROUP_DM_LEGACY_RE.exec(message.content);
    if (legacy) {
      const [, groupName, sender] = legacy;
      return <GroupDMNotificationPill message={message} groupName={groupName} sender={sender} />;
    }
  }

  const content = isError
    ? message.content.replace(/^⚠️ Error:\s*/, "")
    : message.content;

  // Cron / manual check-in prompts open with "[system message]" and tend
  // to span several lines (timestamp meta header + the --- Instructions ---
  // block injected from checkin.md). Collapsing them by default keeps the
  // chat scroll from being dominated by repeated check-in pills; the user
  // can expand on demand.
  if (!isError && content.startsWith("[system message]")) {
    return <CollapsibleSystemPill message={message} content={content} />;
  }

  return (
    <div className="flex justify-center my-2">
      <div
        className={`max-w-[90%] px-4 py-2.5 rounded-lg text-xs leading-relaxed ${
          isError
            ? "bg-red-950/50 border border-red-900/50 text-red-300"
            : "bg-neutral-900/60 border border-neutral-800 text-neutral-400"
        }`}
      >
        <div className="flex items-start gap-2">
          {isError ? (
            <svg
              className="w-4 h-4 text-red-400 shrink-0 mt-0.5"
              fill="none"
              viewBox="0 0 24 24"
              stroke="currentColor"
              strokeWidth={2}
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                d="M12 9v3.75m-9.303 3.376c-.866 1.5.217 3.374 1.948 3.374h14.71c1.73 0 2.813-1.874 1.948-3.374L13.949 3.378c-.866-1.5-3.032-1.5-3.898 0L2.697 16.126zM12 15.75h.007v.008H12v-.008z"
              />
            </svg>
          ) : (
            <svg
              className="w-4 h-4 text-neutral-500 shrink-0 mt-0.5"
              fill="none"
              viewBox="0 0 24 24"
              stroke="currentColor"
              strokeWidth={2}
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                d="M11.25 11.25l.041-.02a.75.75 0 011.063.852l-.708 2.836a.75.75 0 001.063.853l.041-.021M21 12a9 9 0 11-18 0 9 9 0 0118 0zm-9-3.75h.008v.008H12V8.25z"
              />
            </svg>
          )}
          <span className="whitespace-pre-wrap wrap-anywhere">{content}</span>
        </div>
        <div className="text-[10px] text-neutral-600 mt-1.5 text-right">
          {formatTime(message.timestamp)}
        </div>
      </div>
    </div>
  );
}

// CollapsibleSystemPill renders a multi-line "[system message]" prompt as
// a one-line pill by default with an expand chevron. The first line is
// the cron / check-in meta header (timestamp + timeout); the body below
// the "--- Instructions ---" separator is the per-agent check-in content,
// which can be a long checklist. Expand on click; clicking again collapses.
function CollapsibleSystemPill({
  message,
  content,
}: {
  message: AgentMessage;
  content: string;
}) {
  const [expanded, setExpanded] = useState(false);
  const firstLineEnd = content.indexOf("\n");
  const firstLine = firstLineEnd < 0 ? content : content.slice(0, firstLineEnd);
  // "More content available" guard: a single-line system message has no
  // body to reveal, and an unusually long first line still wraps in the
  // pill, so showing the chevron would be misleading. 80 chars is a
  // soft heuristic — past that the pill almost certainly has a body.
  const hasMore = firstLineEnd >= 0 || firstLine.length > 80;
  return (
    <div className="flex justify-center my-2">
      <div className="max-w-[90%] bg-neutral-900/60 border border-neutral-800 text-neutral-400 rounded-lg text-xs leading-relaxed">
        <button
          type="button"
          onClick={() => hasMore && setExpanded((v) => !v)}
          className={`w-full px-4 py-2.5 flex items-start gap-2 text-left ${
            hasMore ? "cursor-pointer hover:bg-neutral-900/80" : "cursor-default"
          }`}
        >
          <svg
            className="w-4 h-4 text-neutral-500 shrink-0 mt-0.5"
            fill="none"
            viewBox="0 0 24 24"
            stroke="currentColor"
            strokeWidth={2}
          >
            <path
              strokeLinecap="round"
              strokeLinejoin="round"
              d="M11.25 11.25l.041-.02a.75.75 0 011.063.852l-.708 2.836a.75.75 0 001.063.853l.041-.021M21 12a9 9 0 11-18 0 9 9 0 0118 0zm-9-3.75h.008v.008H12V8.25z"
            />
          </svg>
          <span
            className={`flex-1 ${expanded ? "whitespace-pre-wrap wrap-anywhere" : "truncate"}`}
          >
            {expanded ? content : firstLine}
          </span>
          {hasMore && (
            <svg
              className={`w-3.5 h-3.5 text-neutral-500 shrink-0 mt-1 transition-transform ${
                expanded ? "rotate-180" : ""
              }`}
              fill="none"
              viewBox="0 0 24 24"
              stroke="currentColor"
              strokeWidth={2}
            >
              <path strokeLinecap="round" strokeLinejoin="round" d="M19 9l-7 7-7-7" />
            </svg>
          )}
        </button>
        <div className="text-[10px] text-neutral-600 px-4 pb-1.5 text-right">
          {formatTime(message.timestamp)}
        </div>
      </div>
    </div>
  );
}
