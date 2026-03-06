import { useEffect, useState } from "react";
import { useParams, useNavigate } from "react-router";
import { agentApi, type Credential } from "../../lib/agentApi";

export function AgentCredentials() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [credentials, setCredentials] = useState<Credential[]>([]);
  const [label, setLabel] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [adding, setAdding] = useState(false);
  const [copied, setCopied] = useState<string | null>(null);
  const [showForm, setShowForm] = useState(false);

  useEffect(() => {
    if (!id) return;
    agentApi.credentials
      .list(id)
      .then(setCredentials)
      .catch(() => navigate("/"));
  }, [id, navigate]);

  const handleAdd = async () => {
    if (!id || !label.trim() || !username.trim() || !password) return;
    setAdding(true);
    setError("");
    try {
      const cred = await agentApi.credentials.add(
        id,
        label.trim(),
        username.trim(),
        password,
      );
      setCredentials((prev) => [...prev, cred]);
      setLabel("");
      setUsername("");
      setPassword("");
      setShowForm(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setAdding(false);
    }
  };

  const handleDelete = async (credId: string) => {
    if (!id || !confirm("Delete this credential?")) return;
    try {
      await agentApi.credentials.delete(id, credId);
      setCredentials((prev) => prev.filter((c) => c.id !== credId));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleCopy = async (credId: string) => {
    if (!id) return;
    try {
      const pw = await agentApi.credentials.revealPassword(id, credId);
      await navigator.clipboard.writeText(pw);
      setCopied(credId);
      setTimeout(() => setCopied(null), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center justify-between px-4 py-3 border-b border-neutral-800">
        <div className="flex items-center gap-2">
          <button
            onClick={() => navigate(`/agents/${id}/settings`)}
            className="text-neutral-400 hover:text-neutral-200"
          >
            &larr;
          </button>
          <h1 className="text-lg font-bold">Credentials</h1>
        </div>
        <button
          onClick={() => setShowForm((v) => !v)}
          className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-sm"
        >
          {showForm ? "Cancel" : "+ Add"}
        </button>
      </header>

      <main className="p-4 space-y-3 max-w-md mx-auto">
        {/* Add form */}
        {showForm && (
          <div className="p-4 bg-neutral-900 border border-neutral-700 rounded-lg space-y-3">
            <input
              type="text"
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              placeholder="Label (e.g. GitHub)"
              className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
              autoFocus
            />
            <input
              type="text"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="Username / ID"
              className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
            />
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="Password"
              onKeyDown={(e) => {
                if (e.key === "Enter" && !adding) {
                  e.preventDefault();
                  handleAdd();
                }
              }}
              className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
            />
            <button
              onClick={handleAdd}
              disabled={
                adding || !label.trim() || !username.trim() || !password
              }
              className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-sm font-medium disabled:opacity-40"
            >
              {adding ? "Adding..." : "Add"}
            </button>
          </div>
        )}

        {/* Credential list */}
        {credentials.map((cred) => (
          <div
            key={cred.id}
            className="p-3 bg-neutral-900 border border-neutral-800 rounded-lg"
          >
            <div className="text-sm font-medium text-neutral-300">
              {cred.label}
            </div>
            <div className="text-xs text-neutral-500 mt-1 font-mono">
              {cred.username}
            </div>
            <div className="flex items-center justify-between mt-2">
              <span className="text-xs text-neutral-600 tracking-widest select-none">
                ••••••••
              </span>
              <div className="flex gap-1">
                <button
                  onClick={() => handleCopy(cred.id)}
                  className={`text-xs px-2 py-1 rounded ${
                    copied === cred.id
                      ? "text-green-400 bg-green-950"
                      : "text-neutral-500 hover:text-neutral-300 hover:bg-neutral-800"
                  }`}
                >
                  {copied === cred.id ? "Copied" : "Copy"}
                </button>
                <button
                  onClick={() => handleDelete(cred.id)}
                  className="text-xs text-neutral-600 hover:text-red-400 px-2 py-1 rounded hover:bg-neutral-800"
                >
                  Delete
                </button>
              </div>
            </div>
          </div>
        ))}

        {credentials.length === 0 && !showForm && (
          <div className="text-sm text-neutral-600 text-center py-12">
            No credentials registered
          </div>
        )}

        {error && (
          <div className="p-3 bg-red-950 border border-red-800 rounded-lg text-sm text-red-300">
            {error}
          </div>
        )}
      </main>
    </div>
  );
}
