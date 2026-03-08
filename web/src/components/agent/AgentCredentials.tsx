import { useEffect, useState, useRef, useCallback } from "react";
import { useParams, useNavigate } from "react-router";
import { agentApi, type Credential, type OTPEntry } from "../../lib/agentApi";

function TOTPDisplay({ agentId, credId }: { agentId: string; credId: string }) {
  const [code, setCode] = useState<string | null>(null);
  const [remaining, setRemaining] = useState(0);
  const [loading, setLoading] = useState(false);
  const timerRef = useRef<ReturnType<typeof setInterval>>(undefined);
  const [copied, setCopied] = useState(false);

  const fetchCode = useCallback(async () => {
    try {
      const r = await agentApi.credentials.getTOTPCode(agentId, credId);
      setCode(r.code);
      setRemaining(r.remaining);
    } catch {
      setCode(null);
    }
  }, [agentId, credId]);

  const handleReveal = async () => {
    setLoading(true);
    await fetchCode();
    setLoading(false);
  };

  useEffect(() => {
    if (code === null) return;
    timerRef.current = setInterval(() => {
      setRemaining((prev) => {
        if (prev <= 1) {
          fetchCode();
          return prev; // Will be updated by fetchCode
        }
        return prev - 1;
      });
    }, 1000);
    return () => clearInterval(timerRef.current);
  }, [code, fetchCode]);

  const handleCopy = async () => {
    if (!code) return;
    // Refetch to get the freshest code
    const r = await agentApi.credentials.getTOTPCode(agentId, credId);
    await navigator.clipboard.writeText(r.code);
    setCode(r.code);
    setRemaining(r.remaining);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  if (code === null) {
    return (
      <button
        onClick={handleReveal}
        disabled={loading}
        className="text-xs px-2 py-1 rounded text-blue-400 hover:bg-neutral-800"
      >
        {loading ? "..." : "TOTP"}
      </button>
    );
  }

  return (
    <div className="flex items-center gap-2">
      <span className="font-mono text-sm text-blue-300 tracking-widest">{code}</span>
      <span className="text-xs text-neutral-500 w-5 text-right">{remaining}s</span>
      <button
        onClick={handleCopy}
        className={`text-xs px-1.5 py-0.5 rounded ${
          copied ? "text-green-400 bg-green-950" : "text-neutral-500 hover:text-neutral-300 hover:bg-neutral-800"
        }`}
      >
        {copied ? "OK" : "Copy"}
      </button>
    </div>
  );
}

function QRImportModal({
  agentId,
  onImport,
  onClose,
}: {
  agentId: string;
  onImport: (entries: OTPEntry[]) => void;
  onClose: () => void;
}) {
  const [entries, setEntries] = useState<OTPEntry[]>([]);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [mode, setMode] = useState<"upload" | "uri">("upload");
  const [uri, setUri] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);

  const handleFile = async (file: File) => {
    setLoading(true);
    setError("");
    try {
      const result = await agentApi.credentials.parseQR(agentId, file);
      setEntries(result);
      setSelected(new Set(result.map((_, i) => i)));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  };

  const handleURI = async () => {
    if (!uri.trim()) return;
    setLoading(true);
    setError("");
    try {
      const result = await agentApi.credentials.parseOTPURI(agentId, uri.trim());
      setEntries(result);
      setSelected(new Set(result.map((_, i) => i)));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  };

  const toggleEntry = (i: number) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(i)) next.delete(i);
      else next.add(i);
      return next;
    });
  };

  const handleImport = () => {
    onImport(entries.filter((_, i) => selected.has(i)));
  };

  return (
    <div className="fixed inset-0 bg-black/60 flex items-center justify-center z-50 p-4">
      <div className="bg-neutral-900 border border-neutral-700 rounded-lg w-full max-w-md max-h-[80vh] flex flex-col">
        <div className="flex items-center justify-between px-4 py-3 border-b border-neutral-800">
          <h2 className="text-sm font-bold">Import from QR / URI</h2>
          <button onClick={onClose} className="text-neutral-500 hover:text-neutral-300">
            &times;
          </button>
        </div>

        <div className="p-4 space-y-3 overflow-y-auto flex-1">
          {entries.length === 0 ? (
            <>
              <div className="flex gap-2">
                <button
                  onClick={() => setMode("upload")}
                  className={`flex-1 py-1.5 text-xs rounded ${
                    mode === "upload" ? "bg-neutral-700 text-neutral-200" : "text-neutral-500 hover:bg-neutral-800"
                  }`}
                >
                  QR Image
                </button>
                <button
                  onClick={() => setMode("uri")}
                  className={`flex-1 py-1.5 text-xs rounded ${
                    mode === "uri" ? "bg-neutral-700 text-neutral-200" : "text-neutral-500 hover:bg-neutral-800"
                  }`}
                >
                  URI Text
                </button>
              </div>

              {mode === "upload" ? (
                <div>
                  <input
                    ref={fileRef}
                    type="file"
                    accept="image/*"
                    className="hidden"
                    onChange={(e) => {
                      const f = e.target.files?.[0];
                      if (f) handleFile(f);
                    }}
                  />
                  <button
                    onClick={() => fileRef.current?.click()}
                    disabled={loading}
                    className="w-full py-8 border-2 border-dashed border-neutral-700 rounded-lg text-neutral-500 hover:border-neutral-500 hover:text-neutral-300 text-sm"
                  >
                    {loading ? "Decoding..." : "Tap to select QR image"}
                  </button>
                </div>
              ) : (
                <div className="space-y-2">
                  <textarea
                    value={uri}
                    onChange={(e) => setUri(e.target.value)}
                    placeholder="otpauth://totp/... or otpauth-migration://..."
                    rows={3}
                    className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500 resize-none"
                  />
                  <button
                    onClick={handleURI}
                    disabled={loading || !uri.trim()}
                    className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-sm font-medium disabled:opacity-40"
                  >
                    {loading ? "Parsing..." : "Parse"}
                  </button>
                </div>
              )}
            </>
          ) : (
            <>
              <div className="text-xs text-neutral-500">
                {entries.length} entries found &mdash; select to import
              </div>
              {entries.map((entry, i) => (
                <label
                  key={i}
                  className={`flex items-start gap-3 p-3 rounded-lg border cursor-pointer ${
                    selected.has(i) ? "border-blue-700 bg-blue-950/30" : "border-neutral-800 bg-neutral-950"
                  }`}
                >
                  <input
                    type="checkbox"
                    checked={selected.has(i)}
                    onChange={() => toggleEntry(i)}
                    className="mt-0.5"
                  />
                  <div className="min-w-0 flex-1">
                    <div className="text-sm font-medium text-neutral-300 truncate">
                      {entry.issuer || entry.label || "Unknown"}
                    </div>
                    <div className="text-xs text-neutral-500 font-mono truncate">{entry.username}</div>
                  </div>
                </label>
              ))}
              <button
                onClick={handleImport}
                disabled={selected.size === 0}
                className="w-full py-2 bg-blue-700 hover:bg-blue-600 rounded text-sm font-medium disabled:opacity-40"
              >
                Import {selected.size} credential{selected.size !== 1 ? "s" : ""}
              </button>
            </>
          )}

          {error && (
            <div className="p-2 bg-red-950 border border-red-800 rounded text-xs text-red-300">{error}</div>
          )}
        </div>
      </div>
    </div>
  );
}

export function AgentCredentials() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [credentials, setCredentials] = useState<Credential[]>([]);
  const [label, setLabel] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [totpSecret, setTotpSecret] = useState("");
  const [error, setError] = useState("");
  const [adding, setAdding] = useState(false);
  const [copied, setCopied] = useState<string | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [showQRImport, setShowQRImport] = useState(false);

  useEffect(() => {
    if (!id) return;
    agentApi.credentials
      .list(id)
      .then(setCredentials)
      .catch(() => navigate("/"));
  }, [id, navigate]);

  const handleAdd = async () => {
    if (!id || !label.trim() || !username.trim() || (!password && !totpSecret.trim())) return;
    setAdding(true);
    setError("");
    try {
      const cred = await agentApi.credentials.add(
        id,
        label.trim(),
        username.trim(),
        password,
        totpSecret.trim() ? { secret: totpSecret.trim() } : undefined,
      );
      setCredentials((prev) => [...prev, cred]);
      setLabel("");
      setUsername("");
      setPassword("");
      setTotpSecret("");
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

  const handleQRImport = async (entries: OTPEntry[]) => {
    if (!id) return;
    setShowQRImport(false);
    setError("");
    const newCreds: Credential[] = [];
    for (const entry of entries) {
      try {
        const cred = await agentApi.credentials.add(
          id,
          entry.issuer || entry.label || "Unknown",
          entry.username,
          "",
          {
            secret: entry.totpSecret,
            algorithm: entry.algorithm || undefined,
            digits: entry.digits || undefined,
            period: entry.period || undefined,
          },
        );
        newCreds.push(cred);
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err));
        break;
      }
    }
    if (newCreds.length > 0) {
      setCredentials((prev) => [...prev, ...newCreds]);
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
        <div className="flex gap-2">
          <button
            onClick={() => setShowQRImport(true)}
            className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-sm"
          >
            QR Import
          </button>
          <button
            onClick={() => setShowForm((v) => !v)}
            className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-sm"
          >
            {showForm ? "Cancel" : "+ Add"}
          </button>
        </div>
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
              className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
            />
            <input
              type="password"
              value={totpSecret}
              onChange={(e) => setTotpSecret(e.target.value)}
              placeholder="TOTP Secret (optional)"
              className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
            />
            <button
              onClick={handleAdd}
              disabled={
                adding || !label.trim() || !username.trim() || (!password && !totpSecret.trim())
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
                  {copied === cred.id ? "Copied" : "Copy PW"}
                </button>
                <button
                  onClick={() => handleDelete(cred.id)}
                  className="text-xs text-neutral-600 hover:text-red-400 px-2 py-1 rounded hover:bg-neutral-800"
                >
                  Delete
                </button>
              </div>
            </div>
            {cred.totpSecret && id && (
              <div className="mt-2 pt-2 border-t border-neutral-800">
                <TOTPDisplay agentId={id} credId={cred.id} />
              </div>
            )}
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

      {showQRImport && id && (
        <QRImportModal
          agentId={id}
          onImport={handleQRImport}
          onClose={() => setShowQRImport(false)}
        />
      )}
    </div>
  );
}
