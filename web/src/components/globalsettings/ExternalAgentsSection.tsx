import { useCallback, useEffect, useState } from "react";
import {
  externalAgentApi,
  type ExternalAgent,
  type ExternalNode,
  type PairingCode,
} from "../../lib/externalAgentApi";
import { errMsg } from "../../lib/utils";
import { SectionCard } from "../ui/SectionCard";
import { Button } from "../ui/Button";
import { Input } from "../ui/Input";
import { Lamp } from "../ui/Lamp";
import { Toggle } from "../ui/Toggle";
import { useT } from "../../lib/i18n";

interface Props {
  setError: (msg: string) => void;
  flashSuccess: () => void;
}

// Online state only changes when an external client connects/disconnects;
// a slow poll is enough for a settings screen.
const POLL_MS = 10_000;

function pairCommand(p: PairingCode): string {
  const url = p.pairUrl ?? "http://<kojo-host>:<external-port>/v1/pair";
  return `curl -s ${url} -d '{"code":"${p.code}"}'`;
}

function fmtTime(ms?: number): string {
  if (!ms) return "";
  return new Date(ms).toLocaleString();
}

/**
 * ExternalAgentsSection manages external AI agents (xa_...): AIs running on
 * other tailnet nodes that join group DMs through the dedicated external
 * listener. Registration issues a one-time pairing code; the node that
 * redeems it is bound to the agent and locked out of this Owner UI.
 */
export function ExternalAgentsSection({ setError, flashSuccess }: Props) {
  const t = useT();
  const [agents, setAgents] = useState<ExternalAgent[] | null>(null);
  const [listenerUrl, setListenerUrl] = useState("");
  const [nodes, setNodes] = useState<ExternalNode[]>([]);
  const [codes, setCodes] = useState<Record<string, PairingCode>>({});
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [creating, setCreating] = useState(false);
  const [copied, setCopied] = useState("");

  const reload = useCallback(() => {
    externalAgentApi
      .list()
      .then((r) => {
        setAgents(r.agents);
        setListenerUrl(r.listenerUrl);
      })
      .catch((e) => {
        setError(errMsg(e));
        setAgents((prev) => prev ?? []);
      });
    externalAgentApi
      .listNodes()
      .then(setNodes)
      .catch(() => {});
  }, [setError]);

  useEffect(() => {
    reload();
    const h = setInterval(reload, POLL_MS);
    return () => clearInterval(h);
  }, [reload]);

  const run = async (id: string, fn: () => Promise<void>) => {
    setBusy((p) => ({ ...p, [id]: true }));
    try {
      await fn();
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setBusy((p) => ({ ...p, [id]: false }));
    }
  };

  const replace = (a: ExternalAgent) =>
    setAgents((prev) => (prev ?? []).map((x) => (x.id === a.id ? a : x)));

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim()) return;
    setCreating(true);
    try {
      const a = await externalAgentApi.create(name.trim(), description.trim());
      setAgents((prev) => [...(prev ?? []), a]);
      setName("");
      setDescription("");
      // Registering is pointless without a code; issue one right away.
      const code = await externalAgentApi.pairingCode(a.id);
      setCodes((p) => ({ ...p, [a.id]: code }));
      flashSuccess();
    } catch (err) {
      setError(errMsg(err));
    } finally {
      setCreating(false);
    }
  };

  const copy = async (key: string, text: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(key);
      setTimeout(() => setCopied((c) => (c === key ? "" : c)), 1500);
    } catch {
      /* clipboard unavailable (non-secure context) — text stays selectable */
    }
  };

  return (
    <SectionCard title={t("xa.title")} description={t("xa.desc")}>
      {!listenerUrl && agents !== null && (
        <p className="mb-3 rounded-[10px] border border-lamp-warn/40 bg-lamp-warn/5 p-2 text-[12px] text-ink-dim">
          {t("xa.noListener")}
        </p>
      )}

      {agents === null && (
        <p className="py-4 text-center text-[12px] text-ink-faint">{t("gs.loading")}</p>
      )}
      {agents !== null && agents.length === 0 && (
        <p className="py-4 text-center text-[12px] text-ink-faint">{t("xa.none")}</p>
      )}

      <div className="space-y-2">
        {(agents ?? []).map((a) => {
          const code = codes[a.id];
          const codeLive = code && code.expiresAt > Date.now();
          const b = !!busy[a.id];
          return (
            <div key={a.id} className="rounded-[10px] border border-hairline bg-raised p-3">
              <div className="flex items-center gap-3">
                <Lamp state={!a.enabled ? "off" : a.online ? "run" : "off"} />
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <span className="truncate text-[13px] font-medium text-ink">{a.name}</span>
                    <span className="shrink-0 font-mono text-[10px] text-ink-faint">{a.id}</span>
                  </div>
                  <div className="truncate text-[11px] text-ink-faint">
                    {!a.enabled
                      ? t("xa.disabled")
                      : a.online
                        ? t("xa.online")
                        : t("xa.offline")}
                    {a.nodeName ? ` · ${a.nodeName}` : a.paired ? "" : ` · ${t("xa.unpaired")}`}
                    {a.paired && !a.hasToken ? ` · ${t("xa.revoked")}` : ""}
                    {a.lastSeenAt ? ` · ${t("xa.lastSeen", { time: fmtTime(a.lastSeenAt) })}` : ""}
                  </div>
                  {a.description && (
                    <div className="mt-0.5 truncate text-[11px] text-ink-dim">{a.description}</div>
                  )}
                </div>
                <Toggle
                  checked={a.enabled}
                  disabled={b}
                  aria-label={t("xa.enabled")}
                  onChange={(v) =>
                    run(a.id, async () => replace(await externalAgentApi.update(a.id, { enabled: v })))
                  }
                />
              </div>

              {codeLive && (
                <div className="mt-3 space-y-1.5 rounded-[10px] border border-copper/40 bg-copper/5 p-2">
                  <div className="flex items-center justify-between gap-2">
                    <span className="font-mono text-[15px] font-semibold tracking-wider text-ink">{code.code}</span>
                    <span className="text-[10px] text-ink-faint">
                      {t("xa.codeExpires", { time: new Date(code.expiresAt).toLocaleTimeString() })}
                    </span>
                  </div>
                  <div className="flex items-start gap-2">
                    <code className="min-w-0 flex-1 select-all break-all font-mono text-[11px] text-ink-dim">
                      {pairCommand(code)}
                    </code>
                    <Button className="shrink-0 !px-2 !py-0.5 text-[11px]" onClick={() => copy(a.id, pairCommand(code))}>
                      {copied === a.id ? t("xa.copied") : t("xa.copy")}
                    </Button>
                  </div>
                  <p className="text-[10px] text-ink-faint">{t("xa.codeHelp")}</p>
                </div>
              )}

              <div className="mt-2 flex flex-wrap justify-end gap-2">
                <Button
                  disabled={b}
                  onClick={() =>
                    run(a.id, async () => {
                      if (a.paired && !confirm(t("xa.repairConfirm", { name: a.name }))) return;
                      const c = await externalAgentApi.pairingCode(a.id);
                      setCodes((p) => ({ ...p, [a.id]: c }));
                    })
                  }
                >
                  {a.paired ? t("xa.repair") : t("xa.issueCode")}
                </Button>
                {a.hasToken && (
                  <Button
                    disabled={b}
                    onClick={() =>
                      run(a.id, async () => {
                        if (!confirm(t("xa.revokeConfirm", { name: a.name }))) return;
                        replace(await externalAgentApi.revoke(a.id));
                        flashSuccess();
                      })
                    }
                  >
                    {t("xa.revoke")}
                  </Button>
                )}
                <Button
                  variant="danger"
                  disabled={b}
                  onClick={() =>
                    run(a.id, async () => {
                      if (!confirm(t("xa.deleteConfirm", { name: a.name }))) return;
                      await externalAgentApi.delete(a.id);
                      setAgents((prev) => (prev ?? []).filter((x) => x.id !== a.id));
                      flashSuccess();
                    })
                  }
                >
                  {t("gdm.delete")}
                </Button>
              </div>
            </div>
          );
        })}
      </div>

      <form onSubmit={handleCreate} className="mt-4 space-y-2 border-t border-hairline pt-3">
        <div className="text-xs text-ink-dim">{t("xa.register")}</div>
        <Input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder={t("xa.namePlaceholder")}
          maxLength={64}
          disabled={creating}
          aria-label={t("xa.name")}
        />
        <Input
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          placeholder={t("xa.descPlaceholder")}
          disabled={creating}
          aria-label={t("xa.description")}
        />
        <div className="flex justify-end">
          <Button type="submit" variant="primary" disabled={creating || !name.trim()}>
            {creating ? "..." : t("xa.registerButton")}
          </Button>
        </div>
      </form>

      {nodes.length > 0 && (
        <div className="mt-4 border-t border-hairline pt-3">
          <div className="text-xs text-ink-dim">{t("xa.nodes")}</div>
          <p className="mb-2 text-[11px] text-ink-faint">{t("xa.nodesHelp")}</p>
          <div className="space-y-1.5">
            {nodes.map((n) => {
              return (
                <div
                  key={n.stableNodeId}
                  className="flex items-center gap-2 rounded-[10px] border border-hairline bg-app p-2"
                >
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-[12px] text-ink">{n.nodeName || n.stableNodeId}</div>
                    <div className="truncate font-mono text-[10px] text-ink-faint">
                      {n.tailnetIps.join(", ")}
                    </div>
                  </div>
                  <Button
                    className="shrink-0 !px-2 !py-0.5 text-[11px]"
                    disabled={!!busy[n.stableNodeId]}
                    onClick={() =>
                      run(n.stableNodeId, async () => {
                        if (!confirm(t("xa.nodeRestoreConfirm", { name: n.nodeName || n.stableNodeId }))) return;
                        await externalAgentApi.deleteNode(n.stableNodeId);
                        setNodes((prev) => prev.filter((x) => x.stableNodeId !== n.stableNodeId));
                        flashSuccess();
                      })
                    }
                  >
                    {t("xa.nodeRestore")}
                  </Button>
                </div>
              );
            })}
          </div>
        </div>
      )}
    </SectionCard>
  );
}
