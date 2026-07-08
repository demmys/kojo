import type { RateLimitInfo } from "../../lib/agentApi";
import { useT } from "../../lib/i18n";

// RateLimitBadge renders a compact usage-window indicator in the chat header.
//
// Visibility / styling rules (per spec):
//   - hidden when status is "allowed" and utilization < 0.5
//   - shows the percentage once utilization >= 0.5
//   - amber (lamp-warn) when status is "allowed_warning" or utilization >= 0.8
//   - red (lamp-err) when status is "rejected"
//   - default (copper) for the plain 50–80% band
//
// The tooltip carries the rate-limit window type and the reset time in the
// viewer's local timezone.
export function RateLimitBadge({ info }: { info: RateLimitInfo | null }) {
  const t = useT();
  if (!info) return null;

  const util = info.utilization ?? 0;
  const rejected = info.status === "rejected";
  const warning = info.status === "allowed_warning";

  // Hide the low-usage steady state entirely.
  if (info.status === "allowed" && util < 0.5) return null;

  const tint = rejected ? "lamp-err" : warning || util >= 0.8 ? "lamp-warn" : "copper";
  const cls = {
    "lamp-err": "border-lamp-err/50 bg-lamp-err/10 text-lamp-err",
    "lamp-warn": "border-lamp-warn/50 bg-lamp-warn/10 text-lamp-warn",
    copper: "border-copper/50 bg-copper/10 text-copper",
  }[tint];

  const pct = Math.round(util * 100);
  const statusLabel =
    info.status === "rejected"
      ? t("rate.statusRejected")
      : info.status === "allowed_warning"
        ? t("rate.statusWarning")
        : info.status === "allowed"
          ? t("rate.statusAllowed")
          : info.status;
  const windowLabel =
    info.rateLimitType === "seven_day"
      ? t("rate.window7d")
      : info.rateLimitType === "five_hour"
        ? t("rate.window5h")
        : info.rateLimitType ?? "";
  const resetLabel = info.resetsAt ? new Date(info.resetsAt * 1000).toLocaleString() : "";

  const tooltip = [
    t("rate.status", { status: statusLabel }),
    windowLabel ? t("rate.window", { window: windowLabel }) : "",
    t("rate.utilization", { pct }),
    resetLabel ? t("rate.resets", { time: resetLabel }) : "",
  ]
    .filter(Boolean)
    .join("\n");

  return (
    <span
      title={tooltip}
      className={`shrink-0 rounded-[10px] border px-2 py-1 font-mono text-[11px] ${cls}`}
    >
      {rejected ? t("rate.limitPct", { pct }) : `${pct}%`}
    </span>
  );
}
