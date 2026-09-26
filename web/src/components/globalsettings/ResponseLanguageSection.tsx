import { useEffect, useState } from "react";
import { agentApi } from "../../lib/agentApi";
import { useT } from "../../lib/i18n";
import { errMsg } from "../../lib/utils";
import { SectionCard } from "../ui/SectionCard";
import { Field } from "../ui/Field";
import { Input } from "../ui/Input";
import { Button } from "../ui/Button";

interface Props {
  setError: (msg: string) => void;
  flashSuccess: () => void;
}

// Keep in sync with agent.ResponseLanguageMaxRunes on the server.
const MAX_CHARS = 64;

/**
 * Server-backed, kojo-wide agent response language. Free text so any
 * language (or variant, e.g. "関西弁の日本語") can be set; empty = auto.
 * Distinct from the client-side UI locale ("gs.language") card.
 */
export function ResponseLanguageSection({ setError, flashSuccess }: Props) {
  const t = useT();
  const [saved, setSaved] = useState<string | null>(null);
  const [input, setInput] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    let cancelled = false;
    agentApi.responseLanguage
      .get()
      .then((v) => {
        if (!cancelled) {
          setSaved(v);
          setInput(v);
        }
      })
      .catch(() => {
        if (!cancelled) setSaved("");
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const save = async () => {
    setSaving(true);
    setError("");
    try {
      const res = await agentApi.responseLanguage.set(input);
      setSaved(res.language);
      setInput(res.language);
      flashSuccess();
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setSaving(false);
    }
  };

  const dirty = saved !== null && input.trim() !== saved;

  return (
    <SectionCard title={t("gs.responseLanguage")}>
      <Field help={t("gs.responseLanguageHelp")}>
        <div className="space-y-2">
          <Input
            value={input}
            maxLength={MAX_CHARS}
            disabled={saved === null || saving}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.nativeEvent.isComposing && dirty && !saving) void save();
            }}
            placeholder={t("gs.responseLanguagePlaceholder")}
          />
          <Button
            variant="primary"
            onClick={() => void save()}
            disabled={!dirty || saving}
            className="w-full"
          >
            {saving ? t("settings.saving") : t("gs.save")}
          </Button>
        </div>
      </Field>
    </SectionCard>
  );
}
