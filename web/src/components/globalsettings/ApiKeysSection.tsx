import type { EmbeddingModelHook } from "./useEmbeddingModel";
import type { GeminiApiKeyHook } from "./useGeminiApiKey";
import type { XAIApiKeyHook } from "./useXAIApiKey";
import { SectionCard } from "../ui/SectionCard";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Select } from "../ui/Select";
import { Button } from "../ui/Button";
import { useT } from "../../lib/i18n";

interface Props {
  gemini: GeminiApiKeyHook;
  embedding: EmbeddingModelHook;
  xai: XAIApiKeyHook;
}

/** API Keys section — Gemini API key + embedding model selector. */
export function ApiKeysSection({ gemini, embedding, xai }: Props) {
  const t = useT();
  return (
    <SectionCard
      title={t("gs.apiKeys")}
      description={t("gs.apiKeysDesc")}
    >
      <div className="rounded-[10px] border border-hairline bg-raised p-3">
        <div className="flex items-center justify-between gap-2">
          <div className="min-w-0">
            <div className="text-[13px] font-medium text-ink">Gemini API</div>
            <div className="mt-0.5 text-[12px]">
              {gemini.configured ? (
                <span className="text-lamp-run">{t("gs.configured")}</span>
              ) : gemini.hasFallback ? (
                <span className="text-lamp-warn">{t("gs.usingFallback")}</span>
              ) : (
                <span className="text-ink-faint">{t("gs.notConfigured")}</span>
              )}
            </div>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <Button onClick={gemini.toggleEditing}>
              {gemini.editing ? t("common.cancel") : gemini.configured ? t("gs.update") : t("gs.configure")}
            </Button>
            {gemini.configured && (
              <button
                onClick={gemini.remove}
                aria-label={t("gs.removeGeminiKey")}
                className="rounded-md px-1.5 text-ink-faint transition-colors hover:text-lamp-err"
              >
                &times;
              </button>
            )}
          </div>
        </div>

        {gemini.editing && (
          <div className="mt-3 space-y-2 border-t border-hairline pt-3">
            <Input
              mono
              type="password"
              value={gemini.input}
              onChange={(e) => gemini.setInput(e.target.value)}
              placeholder="AIza..."
            />
            <Button
              variant="primary"
              onClick={gemini.save}
              disabled={gemini.saving || !gemini.input.trim()}
              className="w-full"
            >
              {gemini.saving ? t("settings.saving") : t("gs.save")}
            </Button>
          </div>
        )}

        <div className="mt-3 border-t border-hairline pt-3">
          <Field label={t("gs.embeddingModel")}>
            {embedding.loading ? (
              <div className="text-[12px] text-ink-faint">{t("gs.loadingModels")}</div>
            ) : embedding.available.length > 0 ? (
              <Select
                mono
                value={embedding.model}
                onChange={(e) => embedding.change(e.target.value)}
                disabled={embedding.saving}
              >
                {!embedding.available.includes(embedding.model) && embedding.model && (
                  <option value={embedding.model}>{t("gs.modelUnavailable", { model: embedding.model })}</option>
                )}
                {embedding.available.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </Select>
            ) : (
              <div className="text-[12px] text-ink-faint">
                {gemini.configured ? t("gs.loadModelsFailed") : t("gs.configureKeyForModels")}
              </div>
            )}
          </Field>
        </div>
      </div>

      <div className="mt-3 rounded-[10px] border border-hairline bg-raised p-3">
        <div className="flex items-center justify-between gap-2">
          <div className="min-w-0">
            <div className="text-[13px] font-medium text-ink">xAI (Grok) API</div>
            <div className="mt-0.5 text-[12px]">
              {xai.configured ? (
                <span className="text-lamp-run">{t("gs.configured")}</span>
              ) : xai.hasFallback ? (
                <span className="text-lamp-warn">{t("gs.usingFallback")}</span>
              ) : (
                <span className="text-ink-faint">{t("gs.notConfigured")}</span>
              )}
            </div>
            <div className="mt-0.5 text-[11px] text-ink-faint">{t("gs.voiceInputStt")}</div>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <Button onClick={xai.toggleEditing}>
              {xai.editing ? t("common.cancel") : xai.configured ? t("gs.update") : t("gs.configure")}
            </Button>
            {xai.configured && (
              <button
                onClick={xai.remove}
                aria-label={t("gs.removeXaiKey")}
                className="rounded-md px-1.5 text-ink-faint transition-colors hover:text-lamp-err"
              >
                &times;
              </button>
            )}
          </div>
        </div>

        {xai.editing && (
          <div className="mt-3 space-y-2 border-t border-hairline pt-3">
            <Input
              mono
              type="password"
              value={xai.input}
              onChange={(e) => xai.setInput(e.target.value)}
              placeholder="xai-..."
            />
            <Button
              variant="primary"
              onClick={xai.save}
              disabled={xai.saving || !xai.input.trim()}
              className="w-full"
            >
              {xai.saving ? t("settings.saving") : t("gs.save")}
            </Button>
          </div>
        )}
      </div>
    </SectionCard>
  );
}
