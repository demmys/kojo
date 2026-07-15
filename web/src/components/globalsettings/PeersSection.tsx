// Peer registry UI section.
//
// Shows the cluster's known peers, lets the operator register a new
// peer's metadata off-band (deviceId / name / url), edit name+url,
// and delete a retired peer.
//
// Trust model: privileged inter-peer access is gated by tsnet
// identity — a paired peer's NodeKey lives in peer_registry, and
// every inter-peer request is authenticated via Tailscale WhoIs.
// The peer's NodeKey is captured by the join-request flow:
//   - if a registry row already exists (e.g. seeded by manual
//     Register), the NodeKey is back-filled in place and the peer
//     is admitted immediately;
//   - otherwise the request lands in peer_pending and the operator
//     must Approve it from the panel below before the peer can
//     authenticate.
// Manual Register itself is metadata-only; it never captures a
// NodeKey, so the peer always has to complete a join-request to
// reach the privileged surface.
//
// Self-row policy:
// - List: rendered with an "(this device)" badge.
// - Delete: server returns 409, surfaced as an error here too.

import { useCallback, useEffect, useRef, useState } from "react";
import {
  peersApi,
  type PeerInfo,
  type PeerPendingInfo,
} from "../../lib/peerApi";
import { errMsg } from "../../lib/utils";
import { SectionCard } from "../ui/SectionCard";
import { Input } from "../ui/Input";
import { Textarea } from "../ui/Textarea";
import { Button } from "../ui/Button";
import { t as i18nT, useT } from "../../lib/i18n";

interface Props {
  setError: (msg: string) => void;
  flashSuccess: () => void;
}

const STATUS_COLOR: Record<PeerInfo["status"], string> = {
  online: "text-lamp-run",
  offline: "text-ink-faint",
  degraded: "text-lamp-warn",
};

function formatLastSeen(ms?: number): string {
  if (!ms) return i18nT("peers.never");
  const d = new Date(ms);
  if (Number.isNaN(d.getTime())) return i18nT("peers.never");
  const diff = Date.now() - ms;
  if (diff < 60_000) return i18nT("peers.justNow");
  if (diff < 3_600_000) return i18nT("peers.minAgo", { n: Math.floor(diff / 60_000) });
  if (diff < 86_400_000) return i18nT("peers.hourAgo", { n: Math.floor(diff / 3_600_000) });
  return d.toLocaleString();
}

// REFRESH_INTERVAL_MS is how often the section polls the server for
// status / last_seen drift while the settings page is open. Matches
// the backend OfflineSweeper cadence so a peer flipping to offline
// shows up within one tick of the server detecting it. Polling is
// scoped to this component (cleared on unmount) so it never runs
// when the user is elsewhere in the UI.
const REFRESH_INTERVAL_MS = 30_000;

export function PeersSection({ setError, flashSuccess }: Props) {
  const t = useT();
  const [items, setItems] = useState<PeerInfo[]>([]);
  const [pending, setPending] = useState<PeerPendingInfo[]>([]);
  const [selfId, setSelfId] = useState<string>("");
  const [loading, setLoading] = useState(true);
  // unavailable=true means the server returned 404 / 503 for the
  // registry endpoint — typically because peerIdentity didn't load
  // (KEK missing, fresh install before identity bootstrap, etc).
  // The section degrades to a soft "not available" notice instead
  // of bubbling the network error up to the page-level banner.
  const [unavailable, setUnavailable] = useState(false);
  const [showAdd, setShowAdd] = useState(false);
  // Form state for add: a single textarea matching the --peer-add
  // pipe-separated spec the daemon prints at startup.
  const [pairingSpec, setPairingSpec] = useState("");
  const [parseError, setParseError] = useState("");
  // Inline edit form per row. Only the human-friendly Name and the
  // dial URL are editable here — device_id is immutable.
  const [editFor, setEditFor] = useState<string>("");
  const [editName, setEditName] = useState("");
  const [editURL, setEditURL] = useState("");
  const [busy, setBusy] = useState(false);
  // requestSeq is monotonically incremented before every list() call;
  // a response is only allowed to update state when its captured seq
  // is still the latest. Without this, a slow background poll racing
  // a fast post-mutation refresh could overwrite the fresh result
  // with a stale snapshot (e.g. after register/delete the item count
  // would briefly revert until the next tick). Mounted is also
  // tracked so a response that arrives after unmount doesn't write
  // through to React (no warning, but no cycles wasted either).
  const requestSeq = useRef(0);
  const mounted = useRef(true);

  // refresh is wrapped so background ticks (silent=true) don't flash
  // the loading spinner on every poll and don't bubble transient
  // errors to the page-level banner. Initial mount + post-mutation
  // refreshes pass silent=false so the user sees the spinner once.
  const refresh = useCallback(
    async (silent = false) => {
      // Early-return *before* any setState so a mutation handler that
      // races unmount can't write through the unmounted component.
      // setLoading(true) above this line would have leaked.
      if (!mounted.current) return;
      if (!silent) setLoading(true);
      const myseq = ++requestSeq.current;
      try {
        const [resp, pendResp] = await Promise.all([
          peersApi.list(),
          // pending API: swallow 404/503 (route not registered /
          // registry not initialized — same soft states the main
          // list handles via setUnavailable). Surface anything
          // else as an error banner so the Approve / Reject UI
          // doesn't silently disappear on a real failure.
          peersApi.pending().catch((e: unknown) => {
            const msg = errMsg(e);
            if (!/^404:|^503:/.test(msg) && !silent) {
              setError(t("peers.loadPendingFailed", { msg }));
            }
            return { items: [] };
          }),
        ]);
        if (!mounted.current) return;
        if (myseq === requestSeq.current) {
          setItems(resp.items ?? []);
          setSelfId(resp.selfDeviceId ?? "");
          setPending(pendResp.items ?? []);
          setUnavailable(false);
        }
      } catch (e) {
        if (!mounted.current) return;
        if (myseq !== requestSeq.current) return;
        const msg = errMsg(e);
        // Detect "registry not registered on this server". The
        // server returns 404 (route not registered) when peerIdentity
        // is nil and 503 (registry not initialized) when the
        // registrar hasn't seeded the self-row yet. Both are soft
        // states that the UI should render as "not available", not
        // as red error banners.
        if (/^404:|^503:/.test(msg)) {
          setUnavailable(true);
        } else if (!silent) {
          setError(t("peers.loadFailed", { msg }));
        }
      } finally {
        // Clear loading regardless of seq when this *was* the
        // non-silent fetch the user is waiting on. Without this, a
        // silent poll that becomes "latest" while the user-initiated
        // load is still in-flight would prevent the user-initiated
        // load from ever clearing its spinner — the seq check below
        // would fail when our reply finally lands. silent ticks never
        // touched loading, so they have nothing to clear.
        if (mounted.current && !silent) {
          setLoading(false);
        }
      }
    },
    [setError],
  );

  useEffect(() => {
    mounted.current = true;
    void refresh();
    const handle = window.setInterval(() => {
      void refresh(true);
    }, REFRESH_INTERVAL_MS);
    return () => {
      mounted.current = false;
      window.clearInterval(handle);
    };
  }, [refresh]);

  const resetAddForm = () => {
    setPairingSpec("");
    setParseError("");
    setShowAdd(false);
  };

  // parsePairingSpec splits the pipe-separated spec the `--peer-add`
  // flag accepts: `device_id | name | url`. Same shape the daemon
  // prints on startup so the operator pastes it verbatim. Strips a
  // surrounding pair of single or double quotes so a copy that
  // included the shell-escape delimiters still parses.
  const parsePairingSpec = (raw: string) => {
    let s = raw.trim();
    if ((s.startsWith("'") && s.endsWith("'")) || (s.startsWith('"') && s.endsWith('"'))) {
      s = s.slice(1, -1).trim();
    }
    const parts = s.split("|");
    if (parts.length !== 3) {
      throw new Error(i18nT("peers.parseFieldCount", { count: parts.length }));
    }
    const [deviceId, name, url] = parts.map((p) => p.trim());
    if (!deviceId || !name || !url) {
      throw new Error(i18nT("peers.parseFieldEmpty"));
    }
    return { deviceId, name, url };
  };

  const submitAdd = async () => {
    setParseError("");
    let parsed: { deviceId: string; name: string; url: string };
    try {
      parsed = parsePairingSpec(pairingSpec);
    } catch (e) {
      setParseError(errMsg(e));
      return;
    }
    setBusy(true);
    try {
      await peersApi.register(parsed);
      resetAddForm();
      flashSuccess();
      await refresh();
    } catch (e) {
      setError(t("peers.registerFailed", { msg: errMsg(e) }));
    } finally {
      setBusy(false);
    }
  };

  const openEdit = (p: PeerInfo) => {
    setEditFor(editFor === p.deviceId ? "" : p.deviceId);
    setEditName(p.name);
    setEditURL(p.url ?? "");
  };

  const submitEdit = async (p: PeerInfo) => {
    const name = editName.trim();
    const url = editURL.trim();
    if (!name || !url) {
      setError(t("peers.editBothRequired"));
      return;
    }
    setBusy(true);
    try {
      // Narrow PATCH: only name + url reach the server. last_seen
      // and status are server-owned, so a stale browser tab can't
      // roll back a refresh that landed in another window or
      // another surface.
      await peersApi.updateMetadata(p.deviceId, { name, url });
      setEditFor("");
      flashSuccess();
      await refresh();
    } catch (e) {
      setError(t("peers.editFailed", { msg: errMsg(e) }));
    } finally {
      setBusy(false);
    }
  };

  const approvePending = async (p: PeerPendingInfo) => {
    setBusy(true);
    try {
      await peersApi.approvePending(p.deviceId);
      flashSuccess();
      await refresh();
    } catch (e) {
      setError(t("peers.approveFailed", { msg: errMsg(e) }));
    } finally {
      setBusy(false);
    }
  };

  const rejectPending = async (p: PeerPendingInfo) => {
    if (!window.confirm(t("peers.rejectConfirm", { name: p.name }))) return;
    setBusy(true);
    try {
      await peersApi.rejectPending(p.deviceId);
      flashSuccess();
      await refresh();
    } catch (e) {
      setError(t("peers.rejectFailed", { msg: errMsg(e) }));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: string, peerName: string) => {
    if (!window.confirm(t("peers.removeConfirm", { name: peerName }))) return;
    setBusy(true);
    try {
      await peersApi.remove(id);
      flashSuccess();
      await refresh();
    } catch (e) {
      setError(t("peers.deleteFailed", { msg: errMsg(e) }));
    } finally {
      setBusy(false);
    }
  };

  if (unavailable) {
    return (
      <SectionCard title={t("peers.title")}>
        <div className="rounded-[10px] border border-hairline bg-raised p-3 text-[12px] text-ink-dim">
          {t("peers.unavailable")}
        </div>
      </SectionCard>
    );
  }

  return (
    <SectionCard
      title={t("peers.title")}
      description={t("peers.desc")}
      action={
        <Button onClick={() => setShowAdd((v) => !v)}>
          {showAdd ? t("common.cancel") : t("peers.register")}
        </Button>
      }
    >
      {showAdd && (
        <div className="mb-2 space-y-2 rounded-[10px] border border-hairline bg-raised p-3">
          <p className="text-[11px] leading-snug text-ink-dim">
            {t("peers.pairingHelpPre")}
            (<code className="font-mono">kojo --peer-add</code>{t("peers.pairingHelpArg")})
            {t("peers.pairingHelpFormat")} <code className="font-mono">deviceId | name | url</code>.
            {t("peers.pairingHelpPost")}
          </p>
          <Textarea
            mono
            value={pairingSpec}
            onChange={(e) => {
              setPairingSpec(e.target.value);
              if (parseError) setParseError("");
            }}
            placeholder="00000000-0000-4000-8000-000000000000|laptop|http://100.64.0.5:8080"
            rows={3}
          />
          {parseError && (
            <div className="text-[12px] text-lamp-err">{t("peers.parsePrefix")}{parseError}</div>
          )}
          <Button
            variant="primary"
            onClick={submitAdd}
            disabled={busy || !pairingSpec.trim()}
            className="w-full"
          >
            {busy ? t("peers.registering") : t("peers.registerPeer")}
          </Button>
        </div>
      )}

      {pending.length > 0 && (
        <div className="mb-3">
          <h3 className="mb-2 text-[11px] font-semibold uppercase tracking-wider text-lamp-warn">
            {t("peers.pendingTitle")}
          </h3>
          <p className="mb-2 text-[11px] leading-snug text-ink-dim">
            {t("peers.pendingHelpPre")}
            <code className="font-mono">kojo --peer</code>
            {t("peers.pendingHelpPost")}
          </p>
          {pending.map((p) => (
            <div
              key={p.deviceId}
              className="mb-2 rounded-[10px] border border-lamp-warn/40 bg-lamp-warn/5 p-3"
            >
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0 flex-1">
                  <div className="truncate text-[13px] font-medium text-ink">{p.name}</div>
                  <div className="mt-0.5 truncate font-mono text-[11px] text-ink-faint">
                    {p.deviceId}
                  </div>
                  <div className="truncate font-mono text-[11px] text-ink-faint">
                    {p.url}
                  </div>
                  <div className="mt-1 text-[12px] text-ink-dim">
                    {t("peers.seen", { when: formatLastSeen(p.lastSeen) })}
                  </div>
                </div>
                <div className="flex shrink-0 flex-col gap-1">
                  <Button variant="primary" onClick={() => approvePending(p)} disabled={busy}>
                    {busy ? "..." : t("peers.approve")}
                  </Button>
                  <Button variant="danger" onClick={() => rejectPending(p)} disabled={busy}>
                    {t("peers.reject")}
                  </Button>
                </div>
              </div>
            </div>
          ))}
        </div>
      )}

      {loading ? (
        <div className="text-[12px] text-ink-faint">{t("gs.loading")}</div>
      ) : items.length === 0 ? (
        <div className="text-[12px] text-ink-faint">{t("peers.none")}</div>
      ) : (
        items.map((p) => {
          const isSelf = p.isSelf || p.deviceId === selfId;
          return (
            <div
              key={p.deviceId}
              className="mb-2 rounded-[10px] border border-hairline bg-raised p-3"
            >
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2 text-[13px] font-medium text-ink">
                    <span className="truncate">{p.name}</span>
                    {isSelf && (
                      <span className="rounded bg-hover px-1.5 py-0.5 text-[10px] text-ink-dim">
                        {t("peers.thisDevice")}
                      </span>
                    )}
                  </div>
                  <div className="mt-0.5 truncate font-mono text-[11px] text-ink-faint">
                    {p.deviceId}
                  </div>
                  {p.url && (
                    <div className="truncate font-mono text-[11px] text-ink-faint">
                      {p.url}
                    </div>
                  )}
                  <div className="mt-1 flex items-center gap-3 text-[12px]">
                    <span className={STATUS_COLOR[p.status] ?? "text-ink-faint"}>
                      {p.status}
                    </span>
                    {p.version && (
                      <span
                        className="min-w-0 truncate font-mono text-ink-faint"
                        title={t("peers.versionTitle")}
                      >
                        {p.version}
                      </span>
                    )}
                    <span className="text-ink-faint">
                      {t("peers.seen", { when: formatLastSeen(p.lastSeen) })}
                    </span>
                  </div>
                </div>
                {!isSelf && (
                  <div className="flex shrink-0 flex-col gap-1">
                    <Button
                      onClick={() => openEdit(p)}
                      title={t("peers.editTitle")}
                    >
                      {editFor === p.deviceId ? t("common.cancel") : t("peers.edit")}
                    </Button>
                    <Button
                      variant="danger"
                      onClick={() => remove(p.deviceId, p.name)}
                      title={t("peers.removeTitle")}
                    >
                      {t("gdm.delete")}
                    </Button>
                  </div>
                )}
              </div>

              {!isSelf && editFor === p.deviceId && (
                <div className="mt-3 space-y-2 border-t border-hairline pt-3">
                  <div className="text-[11px] text-ink-dim">
                    {t("peers.displayNameHelp")}
                  </div>
                  <Input
                    value={editName}
                    onChange={(e) => setEditName(e.target.value)}
                    placeholder="laptop"
                  />
                  <div className="text-[11px] text-ink-dim">
                    {t("peers.dialUrlHelp")}
                  </div>
                  <Input
                    mono
                    value={editURL}
                    onChange={(e) => setEditURL(e.target.value)}
                    placeholder="http://100.64.0.5:8080"
                  />
                  <Button
                    variant="primary"
                    onClick={() => submitEdit(p)}
                    disabled={busy || !editName.trim() || !editURL.trim()}
                    className="w-full"
                  >
                    {busy ? t("settings.saving") : t("settings.saveChanges")}
                  </Button>
                </div>
              )}
            </div>
          );
        })
      )}
    </SectionCard>
  );
}
