import { useEffect, useState } from "react";
import { agentApi, type AgentInfo } from "../../lib/agentApi";
import { AgentAvatar } from "../agent/AgentAvatar";
import { errMsg } from "../../lib/utils";
import { SectionCard } from "../ui/SectionCard";
import { Button } from "../ui/Button";
import { useT } from "../../lib/i18n";

interface Props {
  setError: (msg: string) => void;
  flashSuccess: () => void;
}

/**
 * ArchivedAgentsSection lists agents that have been archived (data retained
 * but runtime stopped) and lets the user restore or permanently delete each.
 *
 * Hidden behind global Settings on purpose: archive is a "soft delete" that
 * normal users shouldn't have to think about during day-to-day use, but power
 * users still need a place to recover or finally clear out archived agents.
 */
export function ArchivedAgentsSection({ setError, flashSuccess }: Props) {
  const t = useT();
  const [agents, setAgents] = useState<AgentInfo[] | null>(null);
  const [busy, setBusy] = useState<Record<string, "unarchive" | "delete" | undefined>>({});

  const reload = () => {
    agentApi
      .listArchived()
      .then(setAgents)
      .catch((e) => {
        setError(errMsg(e));
        setAgents([]);
      });
  };

  useEffect(reload, []); // eslint-disable-line react-hooks/exhaustive-deps

  const setAgentBusy = (id: string, op: "unarchive" | "delete" | undefined) =>
    setBusy((prev) => ({ ...prev, [id]: op }));

  const handleUnarchive = async (a: AgentInfo) => {
    setAgentBusy(a.id, "unarchive");
    try {
      await agentApi.unarchive(a.id);
      setAgents((prev) => (prev ?? []).filter((x) => x.id !== a.id));
      flashSuccess();
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setAgentBusy(a.id, undefined);
    }
  };

  const handleDelete = async (a: AgentInfo) => {
    if (
      !confirm(
        t("gs.deleteArchivedConfirm", { name: a.name }),
      )
    ) {
      return;
    }
    setAgentBusy(a.id, "delete");
    try {
      await agentApi.delete(a.id);
      setAgents((prev) => (prev ?? []).filter((x) => x.id !== a.id));
      flashSuccess();
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setAgentBusy(a.id, undefined);
    }
  };

  return (
    <SectionCard
      title={t("gs.archivedAgents")}
      description={t("gs.archivedAgentsDesc")}
    >
      {agents === null && (
        <p className="py-4 text-center text-[12px] text-ink-faint">{t("gs.loading")}</p>
      )}
      {agents !== null && agents.length === 0 && (
        <p className="py-4 text-center text-[12px] text-ink-faint">
          {t("gs.noArchivedAgents")}
        </p>
      )}

      <div className="space-y-2">
        {(agents ?? []).map((a) => {
          const op = busy[a.id];
          return (
            <div
              key={a.id}
              className="flex items-center gap-3 rounded-[10px] border border-hairline bg-raised p-3"
            >
              <AgentAvatar
                agentId={a.id}
                name={a.name}
                size="sm"
                cacheBust={a.avatarHash}
              />
              <div className="min-w-0 flex-1">
                <div className="truncate text-[13px] font-medium text-ink">{a.name}</div>
                <div className="truncate font-mono text-[10px] text-ink-faint">
                  {a.tool}
                  {a.archivedAt ? ` · ${t("gs.archivedOn", { date: a.archivedAt.slice(0, 10) })}` : ""}
                </div>
              </div>
              <div className="flex shrink-0 gap-2">
                <Button onClick={() => handleUnarchive(a)} disabled={op !== undefined}>
                  {op === "unarchive" ? "..." : t("gs.restore")}
                </Button>
                <Button
                  variant="danger"
                  onClick={() => handleDelete(a)}
                  disabled={op !== undefined}
                >
                  {op === "delete" ? "..." : t("gdm.delete")}
                </Button>
              </div>
            </div>
          );
        })}
      </div>
    </SectionCard>
  );
}
