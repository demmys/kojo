import { useEffect, useState } from "react";
import { api } from "../../../lib/api";
import { needsCustomURLFor } from "../agentSettingsPayload";

export type CustomModelsStatus = "idle" | "loading" | "success" | "error";
export type CustomModelsErrorCode = "no_models" | "";

function errorMessage(err: unknown): string {
  if (err instanceof Error && err.message) return err.message;
  return String(err);
}

/**
 * For the "custom"/"llama.cpp" backends, debounce-fetch the model list from
 * the operator-supplied base URL. Mirrors the effect that previously lived
 * inline in AgentCreate and AgentSettings: on success it publishes the list
 * and, when non-empty, keeps the current model if still valid or falls back
 * to the first entry; on failure it clears the list. `setModel` is the
 * caller's model setter so the "pick first" reconciliation stays in the
 * parent's state.
 */
export function useCustomModels(
  tool: string,
  baseURL: string,
  setModel: (updater: (prev: string) => string) => void,
  options?: {
    agentId?: string;
    apiKey?: string;
    credentialVersion?: number;
    enabled?: boolean;
  },
) {
  const needsCustomURL = needsCustomURLFor(tool);
  const [customModels, setCustomModels] = useState<string[]>([]);
  const [status, setStatus] = useState<CustomModelsStatus>("idle");
  const [error, setError] = useState("");
  const [errorCode, setErrorCode] = useState<CustomModelsErrorCode>("");

  useEffect(() => {
    if (!needsCustomURL || options?.enabled === false || !baseURL.trim()) {
      setCustomModels([]);
      setStatus("idle");
      setError("");
      setErrorCode("");
      return;
    }
    let cancelled = false;
    setCustomModels([]);
    setStatus("loading");
    setError("");
    setErrorCode("");
    const timer = setTimeout(() => {
      const requestOptions = options
        ? {
            agentId: options.agentId,
            ...(options.apiKey !== undefined ? { apiKey: options.apiKey } : {}),
          }
        : undefined;
      api.customModels(baseURL, requestOptions).then((models) => {
        if (cancelled) return;
        if (models.length === 0) {
          setCustomModels([]);
          setStatus("error");
          setErrorCode("no_models");
          return;
        }
        setCustomModels(models);
        setStatus("success");
        setModel((prev) => (models.includes(prev) ? prev : models[0]));
      }).catch((err: unknown) => {
        if (cancelled) return;
        setCustomModels([]);
        setStatus("error");
        setErrorCode("");
        setError(errorMessage(err));
      });
    }, 300);
    return () => { cancelled = true; clearTimeout(timer); };
  }, [
    needsCustomURL,
    baseURL,
    options?.agentId,
    options?.apiKey,
    options?.credentialVersion,
    options?.enabled,
  ]);

  return { needsCustomURL, customModels, status, error, errorCode };
}
