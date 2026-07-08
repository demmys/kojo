import { useEffect, useState } from "react";
import { agentApi, type SlackBotStatus } from "../../lib/agentApi";
import { errMsg } from "../../lib/utils";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Toggle } from "../ui/Toggle";
import { Button } from "../ui/Button";
import { Banner } from "../ui/Banner";
import { useT } from "../../lib/i18n";

function CheckRow({
  checked,
  onChange,
  children,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  children: React.ReactNode;
}) {
  return (
    <label className="flex cursor-pointer items-center gap-2 py-1">
      <input
        type="checkbox"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
        className="h-4 w-4 rounded border-hairline bg-raised accent-[color:var(--color-copper)]"
      />
      <span className="text-[13px] text-ink-dim">{children}</span>
    </label>
  );
}

export function SlackBotSettings({ agentId }: { agentId: string }) {
  const t = useT();
  const [status, setStatus] = useState<SlackBotStatus | null>(null);
  const [enabled, setEnabled] = useState(false);
  const [appToken, setAppToken] = useState("");
  const [botToken, setBotToken] = useState("");
  const [threadReplies, setThreadReplies] = useState(true);
  const [respondDM, setRespondDM] = useState(true);
  const [respondMention, setRespondMention] = useState(true);
  const [respondThread, setRespondThread] = useState(true);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [error, setError] = useState("");
  const [testResult, setTestResult] = useState("");

  useEffect(() => {
    agentApi.slackBot.get(agentId).then((s) => {
      setStatus(s);
      setEnabled(s.enabled);
      setThreadReplies(s.threadReplies);
      setRespondDM(s.respondDM);
      setRespondMention(s.respondMention);
      setRespondThread(s.respondThread);
    }).catch(() => {});
  }, [agentId]);

  const handleSave = async () => {
    setSaving(true);
    setError("");
    try {
      await agentApi.slackBot.set(agentId, {
        enabled,
        ...(appToken ? { appToken } : {}),
        ...(botToken ? { botToken } : {}),
        threadReplies,
        respondDM,
        respondMention,
        respondThread,
      });
      setAppToken("");
      setBotToken("");
      const s = await agentApi.slackBot.get(agentId);
      setStatus(s);
    } catch (err) {
      setError(errMsg(err));
    } finally {
      setSaving(false);
    }
  };

  const handleTest = async () => {
    setTesting(true);
    setTestResult("");
    setError("");
    try {
      const res = await agentApi.slackBot.test(agentId, {
        ...(appToken ? { appToken } : {}),
        ...(botToken ? { botToken } : {}),
      });
      setTestResult(t("slack.connectedResult", { team: res.team, bot: res.botUser }));
    } catch (err) {
      setError(errMsg(err));
    } finally {
      setTesting(false);
    }
  };

  const handleDelete = async () => {
    if (!confirm(t("slack.removeConfirm"))) return;
    setSaving(true);
    setError("");
    try {
      await agentApi.slackBot.delete(agentId);
      const s = await agentApi.slackBot.get(agentId);
      setStatus(s);
      setEnabled(false);
      setAppToken("");
      setBotToken("");
      setTestResult("");
    } catch (err) {
      setError(errMsg(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div>
      {/* Header with toggle */}
      <div className="flex items-center justify-between">
        <h3 className="text-[13px] font-semibold text-ink">Slack Bot</h3>
        <div className="flex items-center gap-2">
          {status?.connected && (
            <span className="text-[12px] text-lamp-run">{t("slack.connected")}</span>
          )}
          <Toggle checked={enabled} onChange={setEnabled} aria-label={t("slack.enableAria")} />
        </div>
      </div>

      {/* Collapsible settings */}
      {enabled && (
        <div className="mt-4 space-y-3">
          <Field
            label={
              <>
                {t("slack.appToken")}
                {status?.hasAppToken && !appToken && (
                  <span className="ml-1 text-lamp-run">{t("slack.configured")}</span>
                )}
              </>
            }
          >
            <Input
              mono
              type="password"
              value={appToken}
              onChange={(e) => setAppToken(e.target.value)}
              placeholder={status?.hasAppToken ? "••••••••" : "xapp-..."}
            />
          </Field>

          <Field
            label={
              <>
                {t("slack.botToken")}
                {status?.hasBotToken && !botToken && (
                  <span className="ml-1 text-lamp-run">{t("slack.configured")}</span>
                )}
              </>
            }
          >
            <Input
              mono
              type="password"
              value={botToken}
              onChange={(e) => setBotToken(e.target.value)}
              placeholder={status?.hasBotToken ? "••••••••" : "xoxb-..."}
            />
          </Field>

          <CheckRow checked={threadReplies} onChange={setThreadReplies}>
            {t("slack.threadReplies")}
          </CheckRow>

          {/* Reaction patterns */}
          <div>
            <div className="mb-1 text-[12px] font-medium text-ink-dim">{t("slack.respondTo")}</div>
            <div className="ml-1">
              <CheckRow checked={respondDM} onChange={setRespondDM}>
                {t("slack.respondDM")}
              </CheckRow>
              <CheckRow checked={respondMention} onChange={setRespondMention}>
                {t("slack.respondMention")}
              </CheckRow>
              <CheckRow checked={respondThread} onChange={setRespondThread}>
                {t("slack.respondThread")}
              </CheckRow>
            </div>
          </div>

          {error && <Banner tone="error">{error}</Banner>}
          {testResult && <Banner tone="success">{testResult}</Banner>}

          <div className="flex gap-2">
            <Button onClick={handleSave} disabled={saving} className="flex-1">
              {saving ? t("settings.saving") : t("gs.save")}
            </Button>
            <Button
              onClick={handleTest}
              disabled={
                testing ||
                (!status?.hasAppToken && !appToken) ||
                (!status?.hasBotToken && !botToken)
              }
            >
              {testing ? t("slack.testing") : t("slack.testConnection")}
            </Button>
          </div>

          {(status?.hasAppToken || status?.hasBotToken) && (
            <Button variant="danger" onClick={handleDelete} disabled={saving} className="w-full">
              {t("slack.removeBot")}
            </Button>
          )}

          <p className="text-[12px] text-ink-faint">
            {t("slack.setupHelp")}
          </p>
        </div>
      )}
    </div>
  );
}
