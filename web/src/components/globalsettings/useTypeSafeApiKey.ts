import { useCallback, useEffect, useState } from "react";
import { agentApi } from "../../lib/agentApi";
import { errMsg } from "../../lib/utils";

/**
 * useTypeSafeApiKey encapsulates the TypeSafe (Jev) API key configured status
 * and the save/remove flows. Jev is the System One model kojo uses for
 * cheap per-turn judgments (auto effort classification); the server owns
 * the key and the browser never sees it.
 *
 * Mirrors useXAIApiKey.
 */
export interface TypeSafeApiKeyHook {
  configured: boolean;
  hasFallback: boolean;
  editing: boolean;
  input: string;
  saving: boolean;
  loaded: boolean;
  setInput: (v: string) => void;
  toggleEditing: () => void;
  save: () => Promise<void>;
  remove: () => Promise<void>;
}

export function useTypeSafeApiKey(
  onError: (msg: string) => void,
  onSuccess: () => void,
): TypeSafeApiKeyHook {
  const [configured, setConfigured] = useState(false);
  const [hasFallback, setHasFallback] = useState(false);
  const [editing, setEditing] = useState(false);
  const [input, setInput] = useState("");
  const [saving, setSaving] = useState(false);
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    agentApi.apiKeys
      .get("typesafe")
      .then((r: { configured: boolean; hasFallback?: boolean }) => {
        setConfigured(r.configured);
        setHasFallback(r.hasFallback ?? false);
      })
      .catch(() => {})
      .finally(() => setLoaded(true));
  }, []);

  const toggleEditing = useCallback(() => {
    setEditing((e) => !e);
    setInput("");
    onError("");
  }, [onError]);

  const save = useCallback(async () => {
    if (!input.trim()) return;
    setSaving(true);
    onError("");
    try {
      await agentApi.apiKeys.set("typesafe", input.trim());
      setConfigured(true);
      setEditing(false);
      setInput("");
      onSuccess();
    } catch (err) {
      onError(errMsg(err));
    } finally {
      setSaving(false);
    }
  }, [input, onError, onSuccess]);

  const remove = useCallback(async () => {
    if (!confirm("Remove TypeSafe API key?")) return;
    try {
      await agentApi.apiKeys.delete("typesafe");
      setConfigured(false);
    } catch (err) {
      onError(errMsg(err));
    }
  }, [onError]);

  return {
    configured,
    hasFallback,
    editing,
    input,
    saving,
    loaded,
    setInput,
    toggleEditing,
    save,
    remove,
  };
}
