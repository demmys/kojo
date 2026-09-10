import { useCallback, useEffect, useState } from "react";
import { agentApi } from "../../lib/agentApi";
import { errMsg } from "../../lib/utils";

export interface OpenAIApiKeyHook {
  configured: boolean;
  hasFallback: boolean;
  editing: boolean;
  input: string;
  saving: boolean;
  loaded: boolean;
  loadError: boolean;
  setInput: (value: string) => void;
  toggleEditing: () => void;
  save: () => Promise<void>;
  remove: () => Promise<void>;
}

export function useOpenAIApiKey(
  onError: (message: string) => void,
  onSuccess: () => void,
): OpenAIApiKeyHook {
  const [configured, setConfigured] = useState(false);
  const [hasFallback, setHasFallback] = useState(false);
  const [editing, setEditing] = useState(false);
  const [input, setInput] = useState("");
  const [saving, setSaving] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [loadError, setLoadError] = useState(false);

  useEffect(() => {
    let active = true;
    agentApi.apiKeys
      .get("openai")
      .then((result) => {
        if (!active) return;
        setConfigured(result.configured);
        setHasFallback(result.hasFallback ?? false);
        setLoadError(false);
      })
      .catch(() => {
        if (active) setLoadError(true);
      })
      .finally(() => {
        if (active) setLoaded(true);
      });
    return () => {
      active = false;
    };
  }, []);

  const toggleEditing = useCallback(() => {
    setEditing((current) => !current);
    setInput("");
    onError("");
  }, [onError]);

  const save = useCallback(async () => {
    if (!input.trim()) return;
    setSaving(true);
    onError("");
    try {
      await agentApi.apiKeys.set("openai", input.trim());
      setLoadError(false);
      setConfigured(true);
      setEditing(false);
      setInput("");
      onSuccess();
    } catch (error) {
      onError(errMsg(error));
    } finally {
      setSaving(false);
    }
  }, [input, onError, onSuccess]);

  const remove = useCallback(async () => {
    if (!confirm("Remove OpenAI API key?")) return;
    try {
      await agentApi.apiKeys.delete("openai");
      setLoadError(false);
      setConfigured(false);
    } catch (error) {
      onError(errMsg(error));
    }
  }, [onError]);

  return {
    configured,
    hasFallback,
    editing,
    input,
    saving,
    loaded,
    loadError,
    setInput,
    toggleEditing,
    save,
    remove,
  };
}
