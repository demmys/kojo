import { useState } from "react";
import { agentApi } from "../../lib/agentApi";
import { useT } from "../../lib/i18n";
import { errMsg } from "../../lib/utils";
import { SectionCard } from "../ui/SectionCard";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Button } from "../ui/Button";
import { applyResponseLanguageToAll, type BulkResult } from "./bulkResponseLanguage";

interface Props {
  setError: (msg: string) => void;
}

/**
 * Bulk action: overwrite every active agent's per-agent responseLanguage
 * with one value (empty = auto). Nothing is stored globally — each agent
 * is PATCHed through the normal route (proxied to its holder peer), and
 * agents that could not be updated are listed so the user can retry.
 */
export function ResponseLanguageSection({ setError }: Props) {
  const t = useT();
  const [input, setInput] = useState("");
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<BulkResult | null>(null);

  const apply = async () => {
    const lang = input.trim();
    const label = lang || t("gs.responseLanguageAuto");
    if (!confirm(t("gs.responseLanguageBulkConfirm", { lang: label }))) return;
    setRunning(true);
    setError("");
    setResult(null);
    try {
      const agents = await agentApi.list();
      setResult(
        await applyResponseLanguageToAll(agents, lang, {
          get: (id) => agentApi.get(id),
          update: (id, cfg, etag) => agentApi.update(id, cfg, etag),
        }),
      );
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setRunning(false);
    }
  };

  return (
    <SectionCard title={t("gs.responseLanguage")}>
      <Field help={t("gs.responseLanguageHelp")}>
        <div className="space-y-2">
          <Input
            value={input}
            disabled={running}
            onChange={(e) => setInput(e.target.value)}
            placeholder={t("gs.responseLanguagePlaceholder")}
          />
          <Button variant="primary" onClick={() => void apply()} disabled={running} className="w-full">
            {running ? t("settings.saving") : t("gs.responseLanguageBulkApply")}
          </Button>
          {result && (
            <div className="space-y-1 text-[12px]">
              <div className="text-ink-dim">
                {t("gs.responseLanguageBulkUpdated", { n: String(result.updated) })}
              </div>
              {result.failed.length > 0 && (
                <div className="text-lamp-err">
                  <div>{t("gs.responseLanguageBulkFailed", { n: String(result.failed.length) })}</div>
                  <ul className="list-disc pl-5">
                    {result.failed.map((f) => (
                      <li key={f.id}>
                        {f.name || f.id}: {f.reason}
                      </li>
                    ))}
                  </ul>
                </div>
              )}
            </div>
          )}
        </div>
      </Field>
    </SectionCard>
  );
}
