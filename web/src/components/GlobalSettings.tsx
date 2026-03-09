import { useEffect, useState } from "react";
import { useNavigate } from "react-router";
import { agentApi, type OAuthClientInfo } from "../lib/agentApi";

export function GlobalSettings() {
  const navigate = useNavigate();
  const [clients, setClients] = useState<OAuthClientInfo[]>([]);
  const [editProvider, setEditProvider] = useState<string | null>(null);
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [success, setSuccess] = useState(false);

  useEffect(() => {
    agentApi.oauthClients.list().then(setClients).catch(() => {});
  }, []);

  const handleSave = async (provider: string) => {
    if (!clientId.trim() || !clientSecret.trim()) return;
    setSaving(true);
    setError("");
    try {
      await agentApi.oauthClients.set(provider, clientId.trim(), clientSecret.trim());
      setClients((prev) =>
        prev.map((c) => (c.provider === provider ? { ...c, configured: true } : c)),
      );
      setEditProvider(null);
      setClientId("");
      setClientSecret("");
      setSuccess(true);
      setTimeout(() => setSuccess(false), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleRemove = async (provider: string) => {
    if (!confirm(`Remove OAuth credentials for ${provider}?`)) return;
    try {
      await agentApi.oauthClients.delete(provider);
      setClients((prev) =>
        prev.map((c) => (c.provider === provider ? { ...c, configured: false } : c)),
      );
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const providerLabels: Record<string, string> = {
    gmail: "Google (Gmail)",
  };

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center gap-2 px-4 py-3 border-b border-neutral-800">
        <button
          onClick={() => navigate("/")}
          className="text-neutral-400 hover:text-neutral-200"
        >
          &larr;
        </button>
        <h1 className="text-lg font-bold">Settings</h1>
      </header>

      <main className="p-4 space-y-5 max-w-md mx-auto">
        {/* OAuth Clients */}
        <div>
          <h2 className="text-xs font-semibold text-neutral-500 uppercase tracking-wider mb-3">
            OAuth Clients
          </h2>
          <p className="text-xs text-neutral-600 mb-3">
            Configure OAuth2 credentials for notification sources. These are shared across all agents.
          </p>

          {clients.map((client) => (
            <div
              key={client.provider}
              className="p-3 bg-neutral-900 border border-neutral-800 rounded-lg mb-2"
            >
              <div className="flex items-center justify-between">
                <div>
                  <div className="text-sm font-medium">
                    {providerLabels[client.provider] ?? client.provider}
                  </div>
                  <div className="text-xs text-neutral-500 mt-0.5">
                    {client.configured ? (
                      <span className="text-emerald-500">Configured</span>
                    ) : (
                      <span className="text-neutral-600">Not configured</span>
                    )}
                  </div>
                </div>
                <div className="flex gap-2">
                  <button
                    onClick={() => {
                      setEditProvider(
                        editProvider === client.provider ? null : client.provider,
                      );
                      setClientId("");
                      setClientSecret("");
                      setError("");
                    }}
                    className="px-2 py-1 bg-neutral-800 hover:bg-neutral-700 rounded text-xs"
                  >
                    {editProvider === client.provider ? "Cancel" : client.configured ? "Update" : "Configure"}
                  </button>
                  {client.configured && (
                    <button
                      onClick={() => handleRemove(client.provider)}
                      className="text-neutral-600 hover:text-red-400 text-sm"
                    >
                      &times;
                    </button>
                  )}
                </div>
              </div>

              {editProvider === client.provider && (
                <div className="mt-3 space-y-2 border-t border-neutral-800 pt-3">
                  <input
                    type="text"
                    value={clientId}
                    onChange={(e) => setClientId(e.target.value)}
                    placeholder="Client ID"
                    className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
                  />
                  <input
                    type="password"
                    value={clientSecret}
                    onChange={(e) => setClientSecret(e.target.value)}
                    placeholder="Client Secret"
                    className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
                  />
                  <button
                    onClick={() => handleSave(client.provider)}
                    disabled={saving || !clientId.trim() || !clientSecret.trim()}
                    className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-xs font-medium disabled:opacity-40"
                  >
                    {saving ? "Saving..." : "Save"}
                  </button>
                </div>
              )}
            </div>
          ))}
        </div>

        {error && (
          <div className="p-3 bg-red-950 border border-red-800 rounded-lg text-sm text-red-300">
            {error}
          </div>
        )}
        {success && (
          <div className="p-3 bg-green-950 border border-green-800 rounded-lg text-sm text-green-300">
            Saved
          </div>
        )}
      </main>
    </div>
  );
}
