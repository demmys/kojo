import { useEffect, useState } from "react";
import { api } from "../../../lib/api";
import { needsCustomURLFor } from "../agentSettingsPayload";

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
) {
  const needsCustomURL = needsCustomURLFor(tool);
  const [customModels, setCustomModels] = useState<string[]>([]);

  useEffect(() => {
    if (!needsCustomURL) return;
    let cancelled = false;
    const timer = setTimeout(() => {
      api.customModels(baseURL).then((models) => {
        if (cancelled) return;
        setCustomModels(models);
        if (models.length > 0) {
          setModel((prev) => (models.includes(prev) ? prev : models[0]));
        }
      }).catch(() => { if (!cancelled) setCustomModels([]); });
    }, 300);
    return () => { cancelled = true; clearTimeout(timer); };
  }, [needsCustomURL, baseURL]);

  return { needsCustomURL, customModels };
}
