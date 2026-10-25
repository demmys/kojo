import { useEffect, useState } from "react";
import { groupdmApi, type ExternalTrust, type GroupDMInfo } from "../../lib/groupdmApi";
import { externalAgentApi, isExternalId, type ExternalAgent } from "../../lib/externalAgentApi";
import { errMsg } from "../../lib/utils";
import { Lamp } from "../ui/Lamp";
import { useT } from "../../lib/i18n";

interface Props {
  group: GroupDMInfo;
  onGroupChange: (g: GroupDMInfo) => void;
}

/**
 * Header menu for a group room's external (xa_...) members: the room's
 * trust setting and adding registered external agents. Owner-only server
 * side; the Web UI is always the owner.
 *
 * Trust is re-read on open rather than taken from `group`: most room
 * mutations return the bare GroupDM without `externalTrust`, so the prop
 * can't be relied on to carry it.
 */
export function ExternalMembersMenu({ group, onGroupChange }: Props) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [trust, setTrust] = useState<ExternalTrust | null>(null);
  const [externals, setExternals] = useState<ExternalAgent[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const externalMembers = group.members.filter((m) => isExternalId(m.agentId));
  const memberIds = new Set(group.members.map((m) => m.agentId));
  // POST /members requires a current member as the caller; any kojo member works.
  const caller = group.members.find((m) => !isExternalId(m.agentId));

  useEffect(() => {
    if (!open) return;
    setError("");
    setTrust(null);
    groupdmApi
      .get(group.id)
      .then((g) => setTrust(g.externalTrust === "peer" ? "peer" : "untrusted"))
      .catch((e) => setError(errMsg(e)));
    externalAgentApi
      .list()
      .then((r) => setExternals(r.agents))
      .catch((e) => {
        setError(errMsg(e));
        setExternals([]);
      });
  }, [open, group.id]);

  const addable = (externals ?? []).filter((a) => a.enabled && !memberIds.has(a.id));

  const changeTrust = async (v: ExternalTrust) => {
    if (trust === null || v === trust) return;
    setBusy(true);
    setError("");
    try {
      await groupdmApi.setExternalTrust(group.id, v);
      setTrust(v);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setBusy(false);
    }
  };

  const add = async (a: ExternalAgent) => {
    if (!caller) return;
    setBusy(true);
    setError("");
    try {
      onGroupChange(await groupdmApi.addMember(group.id, a.id, caller.agentId));
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="relative shrink-0">
      <button
        onClick={() => setOpen((v) => !v)}
        className={`cursor-pointer rounded-[10px] px-1.5 py-1 text-base leading-none transition-colors hover:bg-hover hover:text-ink ${
          externalMembers.length > 0 ? "text-copper" : "text-ink-faint"
        }`}
        title={t("xa.trustTitle")}
        aria-label={t("xa.trustTitle")}
      >
        🔌
      </button>
      {open && (
        <>
          <div className="fixed inset-0 z-10" onClick={() => setOpen(false)} />
          <div className="absolute right-0 top-full z-20 mt-1 w-[260px] rounded-[10px] border border-hairline bg-raised p-2 shadow-lg">
            <div className="px-1 pb-1 text-[11px] text-ink-dim">{t("xa.trustTitle")}</div>
            {(
              [
                ["untrusted", t("xa.trustUntrusted"), t("xa.trustUntrustedHint")],
                ["peer", t("xa.trustPeer"), t("xa.trustPeerHint")],
              ] as [ExternalTrust, string, string][]
            ).map(([v, label, hint]) => (
              <button
                key={v}
                disabled={busy || trust === null}
                onClick={() => changeTrust(v)}
                className={`block w-full rounded-[8px] px-2 py-1.5 text-left transition-colors hover:bg-hover disabled:opacity-50 ${
                  trust === v ? "text-ink" : "text-ink-dim"
                }`}
              >
                <div className="text-[13px]">
                  {trust === v ? "● " : "○ "}
                  {label}
                </div>
                <div className="pl-4 text-[10px] text-ink-faint">{hint}</div>
              </button>
            ))}

            {externalMembers.length > 0 && (
              <div className="mt-2 border-t border-hairline pt-2">
                {externalMembers.map((m) => (
                  <div key={m.agentId} className="flex items-center gap-2 px-1 py-0.5 text-[12px] text-ink">
                    <Lamp state={m.status === "online" ? "run" : "off"} />
                    <span className="truncate">{m.agentName}</span>
                  </div>
                ))}
              </div>
            )}

            <div className="mt-2 border-t border-hairline pt-2">
              <div className="px-1 pb-1 text-[11px] text-ink-dim">{t("xa.add")}</div>
              {externals === null ? (
                <div className="px-1 text-[11px] text-ink-faint">{t("gs.loading")}</div>
              ) : !caller ? (
                // The members API needs a kojo member as the inviter.
                <div className="px-1 text-[11px] text-ink-faint">{t("xa.addNoCaller")}</div>
              ) : addable.length === 0 ? (
                <div className="px-1 text-[11px] text-ink-faint">{t("xa.addNone")}</div>
              ) : (
                addable.map((a) => (
                  <button
                    key={a.id}
                    disabled={busy}
                    onClick={() => add(a)}
                    className="flex w-full items-center gap-2 rounded-[8px] px-2 py-1 text-left text-[12px] text-ink transition-colors hover:bg-hover disabled:opacity-50"
                  >
                    <Lamp state={a.online ? "run" : "off"} />
                    <span className="min-w-0 flex-1 truncate">{a.name}</span>
                    <span className="text-ink-faint">＋</span>
                  </button>
                ))
              )}
            </div>
            {error && <p className="mt-2 px-1 text-[11px] text-lamp-err">{error}</p>}
          </div>
        </>
      )}
    </div>
  );
}
