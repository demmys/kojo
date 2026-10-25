import { useEffect, useState } from "react";
import { api, type CustomOpenCodePreset } from "../../../lib/api";
import { Field } from "../../ui/Field";
import { Select } from "../../ui/Select";
import { useT } from "../../../lib/i18n";

let presetsPromise: Promise<CustomOpenCodePreset[]> | null = null;

/** The server's custom-opencode provider whitelist (cached per page load). */
function loadPresets(): Promise<CustomOpenCodePreset[]> {
  if (!presetsPromise) {
    presetsPromise = api
      .info()
      .then((info) => info.customOpenCodePresets ?? [])
      .catch((err: unknown) => {
        presetsPromise = null;
        throw err;
      });
  }
  return presetsPromise;
}

export function useCustomOpenCodePresets(enabled: boolean): CustomOpenCodePreset[] {
  const [presets, setPresets] = useState<CustomOpenCodePreset[]>([]);
  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    loadPresets()
      .then((p) => {
        if (!cancelled) setPresets(p);
      })
      .catch(console.error);
    return () => {
      cancelled = true;
    };
  }, [enabled]);
  return presets;
}

/**
 * Provider select for the custom-opencode tool. The value is the preset's
 * base URL (stored as the agent's customBaseURL); only whitelisted presets
 * can be chosen. A value that is not a preset (e.g. a URL left over from a
 * custom-codex tool switch) is replaced with the first preset once the list
 * loads, so the form never submits a URL the server would reject.
 */
export function CustomOpenCodeProviderField({
  baseURL,
  setBaseURL,
  disabled,
}: {
  baseURL: string;
  setBaseURL: (v: string) => void;
  disabled?: boolean;
}) {
  const t = useT();
  const presets = useCustomOpenCodePresets(true);
  const current = presets.find((p) => p.baseURL === baseURL.trim());
  useEffect(() => {
    if (presets.length > 0 && !current) setBaseURL(presets[0].baseURL);
  }, [presets, current, setBaseURL]);
  return (
    <Field
      label={t("settings.customOpenCodeProvider")}
      help={
        current
          ? `${current.baseURL}${current.keyOptional ? ` — ${t("settings.customOpenCodeKeyOptional")}` : ""}`
          : t("settings.customOpenCodeProviderHelp")
      }
    >
      <Select
        value={current?.baseURL ?? ""}
        onChange={(e) => setBaseURL(e.target.value)}
        disabled={disabled || presets.length === 0}
        aria-label={t("settings.customOpenCodeProvider")}
      >
        {!current && <option value="">…</option>}
        {presets.map((p) => (
          <option key={p.id} value={p.baseURL}>
            {p.name}
          </option>
        ))}
      </Select>
    </Field>
  );
}
