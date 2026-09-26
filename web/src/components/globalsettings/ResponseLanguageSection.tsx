import { useEffect, useState } from "react";
import { agentApi } from "../../lib/agentApi";
import { useT } from "../../lib/i18n";
import { errMsg } from "../../lib/utils";
import { SectionCard } from "../ui/SectionCard";
import { Field } from "../ui/Field";
import { Select } from "../ui/Select";

interface Props {
  setError: (msg: string) => void;
  flashSuccess: () => void;
}

// Language names are shown in their own language so they read the same
// regardless of UI locale. Keep values in sync with the server allowlist
// (internal/agent/response_language.go).
const LANGUAGES: { tag: string; name: string }[] = [
  { tag: "ja", name: "日本語" },
  { tag: "en", name: "English" },
  { tag: "zh", name: "中文" },
  { tag: "ko", name: "한국어" },
];

/**
 * Server-backed, kojo-wide agent response language. Distinct from the
 * client-side UI locale ("gs.language") card.
 */
export function ResponseLanguageSection({ setError, flashSuccess }: Props) {
  const t = useT();
  const [value, setValue] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    let cancelled = false;
    agentApi.responseLanguage
      .get()
      .then((v) => {
        if (!cancelled) setValue(v);
      })
      .catch(() => {
        if (!cancelled) setValue("");
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const change = async (next: string) => {
    const prev = value;
    setValue(next);
    setSaving(true);
    try {
      await agentApi.responseLanguage.set(next);
      flashSuccess();
    } catch (e) {
      setValue(prev);
      setError(errMsg(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <SectionCard title={t("gs.responseLanguage")}>
      <Field help={t("gs.responseLanguageHelp")}>
        <Select
          value={value ?? ""}
          disabled={value === null || saving}
          onChange={(e) => void change(e.target.value)}
        >
          <option value="">{t("gs.responseLanguageAuto")}</option>
          {LANGUAGES.map((l) => (
            <option key={l.tag} value={l.tag}>
              {l.name}
            </option>
          ))}
        </Select>
      </Field>
    </SectionCard>
  );
}
