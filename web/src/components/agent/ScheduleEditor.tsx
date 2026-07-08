import { useEffect, useMemo, useState } from "react";
import { SCHEDULE_PRESETS, TIMEOUT_PRESETS, RESUME_IDLE_PRESETS } from "../../lib/agentApi";
import {
  cronEquivalentToPreset,
  cronFromSimple,
  detectSimpleMode,
  humanizeCron,
  isCronExprSyntaxValid,
  parseCronExpr,
} from "../../lib/cronExpr";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Textarea } from "../ui/Textarea";
import { Toggle } from "../ui/Toggle";
import { t as i18nT, useT, type MessageKey } from "../../lib/i18n";

// Shared chip style for the preset / tab / day-of-week toggles.
function chipClass(selected: boolean): string {
  return `rounded-lg border px-3 py-1.5 text-[13px] transition-colors ${
    selected
      ? "border-copper bg-copper/15 text-copper-bright"
      : "border-hairline bg-raised text-ink-dim hover:border-ink-faint hover:text-ink"
  }`;
}

interface Props {
  // 5-field standard cron expression. "" = scheduling disabled.
  cronExpr: string;
  onCronExprChange: (v: string) => void;
  timeoutMinutes: number;
  onTimeoutChange: (v: number) => void;
  // claude-only: idle window before kojo abandons --resume on an
  // over-token-threshold session. 0 = use server default (5m). Pass `tool`
  // so we hide the control for non-claude backends where it has no effect.
  resumeIdleMinutes?: number;
  onResumeIdleChange?: (v: number) => void;
  tool?: string;
  silentStart: string;
  silentEnd: string;
  onSilentStartChange: (v: string) => void;
  onSilentEndChange: (v: string) => void;
  cronMessage: string;
  onCronMessageChange: (v: string) => void;
  // RFC3339 timestamp of the next scheduled run (silent-hours-adjusted).
  // Empty/undefined when scheduling is off or the agent has no schedule.
  nextCronAt?: string;
  // True when the global cron toggle is in the paused position. We still
  // render nextCronAt (so the user can see what the schedule would do
  // when they un-pause) but suffix "(paused)" to make it obvious the
  // time is not currently firing.
  cronPausedGlobal?: boolean;
  // True when schedule fields have been edited but not yet saved —
  // nextCronAt reflects the saved schedule, so we hide the value and
  // prompt the user to save instead of showing a misleading time.
  scheduleDirty?: boolean;
  // Fires a manual check-in. When omitted the button is hidden.
  onCheckin?: () => void;
  checkingIn?: boolean;
}


// CRON_MESSAGE_MAX_LEN matches the server-side workspaceFileBodyCap (1 MiB)
// divided by 4 — UTF-8 worst-case is 4 bytes per code unit, so capping
// the textarea at ~256 KiB code units keeps the encoded body within the
// MaxBytesReader limit on the /checkin-file PUT regardless of the input
// language. Far larger than realistic check-in bodies (a single line is
// typical) but matches the back-end so the UI never produces a request
// the server will reject.
const CRON_MESSAGE_MAX_LEN = (1 << 20) / 4;

/** Parse "HH:MM" to minutes since midnight. */
function toMinutes(hhmm: string): number {
  const [h, m] = hhmm.split(":").map(Number);
  return h * 60 + (m || 0);
}

/** Build CSS gradient showing the silent window on a 24h bar. */
function timelineGradient(start: string, end: string): string {
  const silent = "rgb(38 43 51)"; // hairline
  const active = "rgb(208 139 85 / 0.4)"; // copper

  if (!start || !end) {
    return active;
  }

  const s = (toMinutes(start) / 1440) * 100;
  const e = (toMinutes(end) / 1440) * 100;

  if (s <= e) {
    return `linear-gradient(to right, ${active} ${s}%, ${silent} ${s}%, ${silent} ${e}%, ${active} ${e}%)`;
  }
  return `linear-gradient(to right, ${silent} ${e}%, ${active} ${e}%, ${active} ${s}%, ${silent} ${s}%)`;
}

const HOUR_MARKS = [0, 3, 6, 9, 12, 15, 18, 21];

function formatNextCron(
  iso: string | undefined,
  now: number,
): { abs: string; rel: string } | null {
  if (!iso) return null;
  const dt = new Date(iso);
  if (Number.isNaN(dt.getTime())) return null;
  const abs = dt.toLocaleString(undefined, {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    timeZoneName: "short",
  });
  const diffMs = dt.getTime() - now;
  const past = diffMs < 0;
  const mins = Math.max(1, Math.round(Math.abs(diffMs) / 60000));
  let amount: string;
  if (mins < 60) amount = `${mins}m`;
  else if (mins < 60 * 24) {
    const h = Math.floor(mins / 60);
    const m = mins % 60;
    amount = m === 0 ? `${h}h` : `${h}h${m}m`;
  } else {
    const d = Math.floor(mins / (60 * 24));
    const h = Math.floor((mins % (60 * 24)) / 60);
    amount = h === 0 ? `${d}d` : `${d}d${h}h`;
  }
  return { abs, rel: past ? i18nT("sched.relAgo", { amount }) : i18nT("sched.relIn", { amount }) };
}

type TabId = "preset" | "hourly" | "daily" | "weekly" | "advanced";

const TABS: { id: TabId; labelKey: MessageKey }[] = [
  { id: "preset", labelKey: "sched.tabPreset" },
  { id: "hourly", labelKey: "sched.tabHourly" },
  { id: "daily", labelKey: "sched.tabDaily" },
  { id: "weekly", labelKey: "sched.tabWeekly" },
  { id: "advanced", labelKey: "sched.tabAdvanced" },
];

const DOW_KEYS: MessageKey[] = [
  "sched.dowSun",
  "sched.dowMon",
  "sched.dowTue",
  "sched.dowWed",
  "sched.dowThu",
  "sched.dowFri",
  "sched.dowSat",
];

/**
 * Pick which tab to surface when the editor mounts (or the value changes
 * out from under us, e.g. after Save). Falls back to Advanced if the
 * expression doesn't fit any of the simple-mode primitives — that's the
 * tab that can faithfully render anything.
 */
function tabForExpr(expr: string): TabId {
  const detected = detectSimpleMode(expr, SCHEDULE_PRESETS);
  if (!detected) return "advanced";
  switch (detected.mode) {
    case "preset":
      return "preset";
    case "everyN":
      // everyN is folded into the preset chips so users see one row of
      // options instead of two near-duplicates. Match by cadence
      // equivalence, NOT string equality, so an already-expanded preset
      // ("7,37 * * * *") that re-loaded from the server still resolves
      // back to the Preset tab.
      return SCHEDULE_PRESETS.some((p) => cronEquivalentToPreset(expr, p.cron))
        ? "preset"
        : "advanced";
    case "hourly":
      return "hourly";
    case "daily":
      return "daily";
    case "weekly":
      return "weekly";
  }
}

export function ScheduleEditor({
  cronExpr,
  onCronExprChange,
  timeoutMinutes,
  onTimeoutChange,
  resumeIdleMinutes,
  onResumeIdleChange,
  tool,
  silentStart,
  silentEnd,
  onSilentStartChange,
  onSilentEndChange,
  cronMessage,
  onCronMessageChange,
  nextCronAt,
  cronPausedGlobal,
  scheduleDirty,
  onCheckin,
  checkingIn,
}: Props) {
  const t = useT();
  const showResumeIdle =
    onResumeIdleChange !== undefined && (tool === undefined || tool === "claude");

  // Live-tick the relative "in 12m" / "2h ago" label. Skip while dirty —
  // the value is hidden behind a "save to update" notice in that case.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!nextCronAt || scheduleDirty) return;
    const id = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(id);
  }, [nextCronAt, scheduleDirty]);

  // Active tab. Re-sync when cronExpr changes externally (e.g. after Save
  // re-issues a fresh agent record) but otherwise keep whatever the user
  // last clicked so editing doesn't fight the auto-detect.
  const [tab, setTab] = useState<TabId>(() => tabForExpr(cronExpr));
  const [advancedDraft, setAdvancedDraft] = useState(cronExpr);
  useEffect(() => {
    setTab(tabForExpr(cronExpr));
    setAdvancedDraft(cronExpr);
  }, [cronExpr]);

  const detected = useMemo(
    () => detectSimpleMode(cronExpr, SCHEDULE_PRESETS),
    [cronExpr],
  );

  // Parsed cronExpr for the tab editors — these read the structure once
  // and re-emit a freshly-built expression on each input change.
  const parsed = parseCronExpr(cronExpr);

  // ---- Silent hours toggle (unchanged from the old editor) ----
  const [silentEnabled, setSilentEnabled] = useState(silentStart !== "" && silentEnd !== "");
  useEffect(() => {
    setSilentEnabled(silentStart !== "" && silentEnd !== "");
  }, [silentStart, silentEnd]);

  const toggleSilentHours = () => {
    if (silentEnabled) {
      setSilentEnabled(false);
      onSilentStartChange("");
      onSilentEndChange("");
    } else {
      setSilentEnabled(true);
      onSilentStartChange(silentStart || "01:00");
      onSilentEndChange(silentEnd || "07:00");
    }
  };
  const localStart = silentStart || "01:00";
  const localEnd = silentEnd || "07:00";

  const enabled = cronExpr !== "";

  return (
    <div className="space-y-4">
      {/* Mode tabs */}
      <div>
        <label className="mb-2 block text-[12px] font-medium text-ink-dim">{t("settings.sec.schedule")}</label>
        <div className="mb-3 flex gap-1 rounded-lg border border-hairline bg-raised p-1">
          {TABS.map((tb) => (
            <button
              key={tb.id}
              type="button"
              onClick={() => setTab(tb.id)}
              className={`flex-1 rounded-md px-2 py-1 text-[12px] transition-colors ${
                tab === tb.id
                  ? "bg-copper/15 text-copper-bright"
                  : "text-ink-dim hover:text-ink"
              }`}
            >
              {t(tb.labelKey)}
            </button>
          ))}
        </div>

        {tab === "preset" && (
          <div className="flex flex-wrap gap-1.5">
            {SCHEDULE_PRESETS.map((opt) => {
              // cronEquivalentToPreset (NOT ===) so a Save round-trip
              // that expanded "@preset:30" into "7,37 * * * *" still
              // highlights the original "30m" chip.
              const selected = cronEquivalentToPreset(cronExpr, opt.cron);
              return (
                <button
                  key={opt.label}
                  type="button"
                  onClick={() => onCronExprChange(opt.cron)}
                  className={chipClass(selected)}
                >
                  {opt.label === "Off"
                    ? t("sched.presetOff")
                    : opt.label === "Daily 09:00"
                      ? t("sched.presetDaily9")
                      : opt.label}
                </button>
              );
            })}
          </div>
        )}

        {tab === "hourly" && (
          <HourlyEditor
            initialMinute={
              detected?.mode === "hourly"
                ? detected.hourlyMinute ?? 0
                : parsed && /^\d+$/.test(parsed.minute)
                  ? parseInt(parsed.minute, 10)
                  : 0
            }
            onChange={(m) =>
              onCronExprChange(cronFromSimple("hourly", { minute: m }))
            }
          />
        )}

        {tab === "daily" && (
          <DailyEditor
            hh={detected?.mode === "daily" ? detected.hh ?? 9 : 9}
            mm={detected?.mode === "daily" ? detected.mm ?? 0 : 0}
            onChange={(hh, mm) =>
              onCronExprChange(cronFromSimple("daily", { hh, mm }))
            }
          />
        )}

        {tab === "weekly" && (
          <WeeklyEditor
            hh={detected?.mode === "weekly" ? detected.hh ?? 9 : 9}
            mm={detected?.mode === "weekly" ? detected.mm ?? 0 : 0}
            dows={detected?.mode === "weekly" ? detected.dows ?? [1] : [1]}
            onChange={(hh, mm, dows) =>
              onCronExprChange(cronFromSimple("weekly", { hh, mm, dows }))
            }
          />
        )}

        {tab === "advanced" && (
          <AdvancedEditor
            value={advancedDraft}
            onLocalChange={setAdvancedDraft}
            onCommit={(v) => onCronExprChange(v)}
          />
        )}

        {/* Live human-readable preview — shown across all tabs. */}
        <p className="mt-2 text-[11px] text-ink-faint">
          {humanizeCron(cronExpr)}
        </p>
      </div>

      {/* Timeout */}
      {(enabled || onCheckin) && (
        <Field label={t("sched.timeout")} help={t("sched.timeoutHelp")}>
          <div className="flex flex-wrap gap-1.5">
            {TIMEOUT_PRESETS.map((opt) => (
              <button
                key={opt.value}
                type="button"
                onClick={() => onTimeoutChange(opt.value)}
                className={chipClass(timeoutMinutes === opt.value)}
              >
                {opt.label}
              </button>
            ))}
          </div>
        </Field>
      )}

      {/* Resume Idle (claude only) */}
      {showResumeIdle && (
        <Field
          label={
            <>
              {t("sched.resumeWindow")}
              <span className="ml-2 text-ink-faint">{t("sched.resumeWindowSub")}</span>
            </>
          }
          help={t("sched.resumeWindowHelp")}
        >
          <div className="flex flex-wrap gap-1.5">
            {RESUME_IDLE_PRESETS.map((opt) => (
              <button
                key={opt.value}
                type="button"
                onClick={() => onResumeIdleChange?.(opt.value)}
                className={chipClass((resumeIdleMinutes ?? 0) === opt.value)}
              >
                {opt.value === 0 ? t("sched.resumeDefault") : opt.label}
              </button>
            ))}
          </div>
        </Field>
      )}

      {/* Silent Hours */}
      {enabled && (
        <div>
          <div className="mb-2 flex items-center justify-between">
            <label className="text-[12px] font-medium text-ink-dim">{t("sched.silentHours")}</label>
            <Toggle checked={silentEnabled} onChange={toggleSilentHours} aria-label={t("sched.silentHours")} />
          </div>

          {silentEnabled && (
            <div className="space-y-3">
              <div className="flex items-center gap-3">
                <Field label={t("sched.from")} className="flex-1">
                  <Input
                    type="time"
                    value={localStart}
                    onChange={(e) => onSilentStartChange(e.target.value)}
                  />
                </Field>
                <span className="mt-6 text-ink-faint">—</span>
                <Field label={t("sched.to")} className="flex-1">
                  <Input
                    type="time"
                    value={localEnd}
                    onChange={(e) => onSilentEndChange(e.target.value)}
                  />
                </Field>
              </div>

              <div>
                <div
                  className="h-3 overflow-hidden rounded-full border border-hairline"
                  style={{ background: timelineGradient(localStart, localEnd) }}
                />
                <div className="mt-1 flex justify-between px-0.5">
                  {HOUR_MARKS.map((h) => (
                    <span key={h} className="text-[9px] tabular-nums text-ink-faint">
                      {h}
                    </span>
                  ))}
                  <span className="text-[9px] tabular-nums text-ink-faint">24</span>
                </div>
              </div>

              <p className="text-[11px] text-ink-faint">
                {toMinutes(localStart) <= toMinutes(localEnd)
                  ? t("sched.silentRange", { start: localStart, end: localEnd })
                  : t("sched.silentRangeOvernight", { start: localStart, end: localEnd })}
              </p>
            </div>
          )}

          {!silentEnabled && (
            <p className="text-[11px] text-ink-faint">
              {t("sched.runs247")}
            </p>
          )}
        </div>
      )}

      {/* Next check-in / manual check-in trigger.
          Both pieces are only meaningful for a persisted agent: nextCronAt
          is server-computed against the saved schedule, and onCheckin fires
          a run against the saved record. AgentCreate passes neither, so
          gating on `onCheckin` (which AgentSettings always provides) keeps
          the whole block hidden in create mode — even though `enabled` is
          true there via the default cron preset. */}
      {onCheckin && (
        <div className="space-y-2 rounded-[10px] border border-hairline bg-raised p-3">
          {enabled && (() => {
            const next = scheduleDirty ? null : formatNextCron(nextCronAt, now);
            return (
              <div className="flex items-center justify-between gap-3">
                <span className="text-[12px] text-ink-dim">
                  {t("sched.nextCheckin")}
                  {cronPausedGlobal && (
                    <span className="ml-1.5 text-lamp-warn">{t("sched.pausedSuffix")}</span>
                  )}
                </span>
                <span className="text-[12px] tabular-nums text-ink">
                  {scheduleDirty ? (
                    <span className="text-ink-faint">{t("sched.saveToUpdate")}</span>
                  ) : next ? (
                    <>
                      {next.abs}
                      <span className="ml-1.5 text-ink-dim">({next.rel})</span>
                    </>
                  ) : (
                    <span className="text-ink-faint">—</span>
                  )}
                </span>
              </div>
            );
          })()}

          {/* Outer guard above already requires onCheckin, so render
              unconditionally here. */}
          <button
            type="button"
            onClick={onCheckin}
            disabled={checkingIn}
            className="w-full rounded-lg border border-copper/50 bg-copper/10 px-3 py-2 text-[13px] text-copper-bright transition-colors hover:bg-copper/20 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {checkingIn ? t("sched.checkingIn") : t("sched.checkinNow")}
          </button>
        </div>
      )}

      {/* Custom Check-in Message */}
      <Field
        label={t("sched.checkinMessage")}
        help={
          <>
            {t("sched.checkinMessageHelpPre")}
            <code className="text-ink-dim">{"{date}"}</code>
            {t("sched.checkinMessageHelpPost")}
          </>
        }
      >
        <Textarea
          value={cronMessage}
          onChange={(e) => onCronMessageChange(e.target.value)}
          rows={3}
          maxLength={CRON_MESSAGE_MAX_LEN}
          placeholder={t("sched.checkinMessageHint")}
        />
      </Field>
    </div>
  );
}

// ---- Sub-editors ----

function HourlyEditor({
  initialMinute,
  onChange,
}: {
  initialMinute: number;
  onChange: (m: number) => void;
}) {
  const t = useT();
  const [m, setM] = useState(initialMinute);
  useEffect(() => setM(initialMinute), [initialMinute]);
  return (
    <div className="flex items-center gap-3">
      <label className="text-[13px] text-ink-dim">{t("sched.everyHourAt")}</label>
      <Input
        type="number"
        min={0}
        max={59}
        value={m}
        onChange={(e) => {
          const v = clampInt(e.target.value, 0, 59);
          setM(v);
          onChange(v);
        }}
        className="w-20"
      />
      <span className="text-[13px] text-ink-dim">{t("sched.minuteUnit")}</span>
    </div>
  );
}

function DailyEditor({
  hh,
  mm,
  onChange,
}: {
  hh: number;
  mm: number;
  onChange: (hh: number, mm: number) => void;
}) {
  const t = useT();
  const [h, setH] = useState(hh);
  const [m, setM] = useState(mm);
  useEffect(() => {
    setH(hh);
    setM(mm);
  }, [hh, mm]);
  const value = `${pad(h)}:${pad(m)}`;
  return (
    <div className="flex items-center gap-3">
      <label className="text-[13px] text-ink-dim">{t("sched.everyDayAt")}</label>
      <Input
        type="time"
        value={value}
        onChange={(e) => {
          const [nh, nm] = e.target.value.split(":").map(Number);
          if (Number.isFinite(nh) && Number.isFinite(nm)) {
            setH(nh);
            setM(nm);
            onChange(nh, nm);
          }
        }}
        className="w-auto"
      />
    </div>
  );
}

function WeeklyEditor({
  hh,
  mm,
  dows,
  onChange,
}: {
  hh: number;
  mm: number;
  dows: number[];
  onChange: (hh: number, mm: number, dows: number[]) => void;
}) {
  const t = useT();
  const [h, setH] = useState(hh);
  const [m, setM] = useState(mm);
  const [d, setD] = useState<number[]>(dows);
  useEffect(() => {
    setH(hh);
    setM(mm);
    setD(dows);
  }, [hh, mm, dows]);

  const toggleDow = (n: number) => {
    const next = d.includes(n) ? d.filter((x) => x !== n) : [...d, n].sort((a, b) => a - b);
    setD(next);
    onChange(h, m, next);
  };

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap gap-1.5">
        {DOW_KEYS.map((key, i) => {
          const selected = d.includes(i);
          return (
            <button
              key={i}
              type="button"
              onClick={() => toggleDow(i)}
              className={chipClass(selected)}
            >
              {t(key)}
            </button>
          );
        })}
      </div>
      <div className="flex items-center gap-3">
        <Input
          type="time"
          value={`${pad(h)}:${pad(m)}`}
          onChange={(e) => {
            const [nh, nm] = e.target.value.split(":").map(Number);
            if (Number.isFinite(nh) && Number.isFinite(nm)) {
              setH(nh);
              setM(nm);
              onChange(nh, nm, d);
            }
          }}
          className="w-auto"
        />
      </div>
      {d.length === 0 && (
        <p className="text-[12px] text-lamp-warn">
          {t("sched.weeklyNoDow")}
        </p>
      )}
    </div>
  );
}

function AdvancedEditor({
  value,
  onLocalChange,
  onCommit,
}: {
  value: string;
  onLocalChange: (v: string) => void;
  onCommit: (v: string) => void;
}) {
  const t = useT();
  const valid = isCronExprSyntaxValid(value);
  return (
    <Field
      help={t("sched.advancedHelp")}
      error={!valid ? t("sched.advancedInvalid") : undefined}
    >
      <Input
        mono
        invalid={!valid}
        value={value}
        onChange={(e) => {
          onLocalChange(e.target.value);
        }}
        onBlur={() => {
          if (valid) onCommit(value.trim());
        }}
        onKeyDown={(e) => {
          if (e.key === "Enter") {
            e.preventDefault();
            if (valid) onCommit(value.trim());
          }
        }}
        placeholder="*/15 * * * *"
      />
    </Field>
  );
}

function pad(n: number): string {
  return n.toString().padStart(2, "0");
}

function clampInt(s: string, lo: number, hi: number): number {
  const n = parseInt(s, 10);
  if (Number.isNaN(n)) return lo;
  return Math.max(lo, Math.min(hi, n));
}
