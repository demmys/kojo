import { useEffect, useState } from "react";
import { api, type CustomOpenCodePreset } from "../../../lib/api";
import { Field } from "../../ui/Field";
import { Input } from "../../ui/Input";
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

/** Raw port text of a URL ("" when absent). Kept as typed, not normalised. */
function portOf(u: string): string {
  return /^[a-z]+:\/\/[^/:]*:(\d*)(\/|$)/i.exec(u.trim())?.[1] ?? "";
}

/**
 * baseURL with its port replaced, assembled by hand: the URL API would drop
 * a scheme-default port (":80"), which the server requires to be present.
 * An invalid port is kept too, so the field shows it and the save is
 * rejected instead of silently keeping the old port.
 */
function withPort(u: string, port: string): string {
  return stripPort(u).replace(/^([a-z]+:\/\/[^/:]*)/i, `$1:${port}`);
}

/** URL with its port removed, by text (an invalid port must not break it). */
function stripPort(u: string): string {
  return u.trim().replace(/^([a-z]+:\/\/[^/:]*):\d*(?=\/|$)/i, "$1");
}

/** Same scheme/host/path as the preset, any (even invalid) port. */
function sameExceptPort(u: string, preset: string): boolean {
  return stripPort(u) === stripPort(preset);
}

function matchPreset(presets: CustomOpenCodePreset[], baseURL: string, preferID?: string) {
  const u = baseURL.trim();
  const exact = presets.find((p) => p.baseURL === u);
  if (exact) return exact;
  const local = presets.filter((p) => p.portConfigurable && sameExceptPort(u, p.baseURL));
  return local.find((p) => p.id === preferID) ?? local[0];
}

/**
 * Provider select for the custom-opencode tool. The value stored is the
 * preset's base URL (the agent's customBaseURL); only whitelisted presets
 * can be chosen, and local presets additionally take a port. A value that
 * matches no preset (e.g. a URL left over from a custom-codex tool switch)
 * is replaced with the first preset once the list loads, so the form never
 * submits a URL the server would reject.
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
  baseURL = baseURL ?? "";
  const presets = useCustomOpenCodePresets(true);
  const [selectedID, setSelectedID] = useState<string>();
  const current = matchPreset(presets, baseURL, selectedID);
  useEffect(() => {
    if (presets.length > 0 && !current) setBaseURL(presets[0].baseURL);
  }, [presets, current, setBaseURL]);
  const port = portOf(baseURL);
  const onPort = (v: string) => {
    if (current) setBaseURL(withPort(current.baseURL, v.replace(/\D/g, "").slice(0, 5)));
  };
  return (
    <>
      <Field
        label={t("settings.customOpenCodeProvider")}
        help={
          current
            ? `${baseURL.trim()}${current.keyOptional ? ` — ${t("settings.customOpenCodeKeyOptional")}` : ""}`
            : t("settings.customOpenCodeProviderHelp")
        }
      >
        <Select
          value={current?.id ?? ""}
          onChange={(e) => {
            const p = presets.find((x) => x.id === e.target.value);
            if (!p) return;
            setSelectedID(p.id);
            setBaseURL(p.baseURL);
          }}
          disabled={disabled || presets.length === 0}
          aria-label={t("settings.customOpenCodeProvider")}
        >
          {!current && <option value="">…</option>}
          {presets.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
        </Select>
      </Field>
      {current?.portConfigurable && (
        <Field label={t("settings.customOpenCodePort")} help={t("settings.customOpenCodePortHelp")}>
          <Input
            inputMode="numeric"
            value={port}
            onChange={(e) => onPort(e.target.value)}
            placeholder={portOf(current.baseURL)}
            disabled={disabled}
            mono
            invalid={!/^[1-9]\d*$/.test(port) || Number(port) > 65535}
            aria-label={t("settings.customOpenCodePort")}
          />
        </Field>
      )}
    </>
  );
}
