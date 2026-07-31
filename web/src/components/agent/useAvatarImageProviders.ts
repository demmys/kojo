import { useEffect, useMemo, useState } from "react";
import { agentApi, type AvatarProvider } from "../../lib/agentApi";

export interface AvatarImageProviders {
  available: AvatarProvider[];
  selected: AvatarProvider;
  setSelected: (provider: AvatarProvider) => void;
  providerForRequest?: AvatarProvider;
  loaded: boolean;
  error: boolean;
}

/** Loads the effective image providers (stored key or server fallback). */
export function useAvatarImageProviders(): AvatarImageProviders {
  const [available, setAvailable] = useState<AvatarProvider[]>([]);
  const [selected, setSelected] = useState<AvatarProvider>("openai");
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState(false);

  useEffect(() => {
    let active = true;
    Promise.all([
      agentApi.apiKeys.get("gemini").catch(() => null),
      agentApi.apiKeys.get("openai").catch(() => null),
    ]).then(([gemini, openai]) => {
      if (!active) return;
      if (!gemini || !openai) {
        setError(true);
        return;
      }
      const next: AvatarProvider[] = [];
      if (
        gemini.configured || gemini.hasFallback
      ) {
        next.push("gemini");
      }
      if (
        openai.configured || openai.hasFallback
      ) {
        next.push("openai");
      }
      setAvailable(next);
      setSelected((current) =>
        next.includes(current) ? current : next.includes("openai") ? "openai" : next[0] ?? "openai",
      );
      setLoaded(true);
    });
    return () => {
      active = false;
    };
  }, []);

  const providerForRequest = useMemo(
    () => (available.length === 1 ? available[0] : available.length > 1 ? selected : undefined),
    [available, selected],
  );

  return { available, selected, setSelected, providerForRequest, loaded, error };
}
