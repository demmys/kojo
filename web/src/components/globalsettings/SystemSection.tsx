import { useState } from "react";
import { api } from "../../lib/api";
import { errMsg } from "../../lib/utils";
import { SectionCard } from "../ui/SectionCard";
import { Button } from "../ui/Button";
import { useT } from "../../lib/i18n";

interface Props {
  setError: (msg: string) => void;
}

/**
 * SystemSection exposes daemon lifecycle controls to the Owner:
 * "Rebuild & Restart" runs `make build` server-side (swapping the
 * running binary in place) and, on success, triggers a graceful
 * restart so the new build takes effect. "Restart" skips the build.
 *
 * Both are destructive to in-flight state (the restart drains active
 * turns then re-execs the process), so each is guarded by a confirm.
 */
export function SystemSection({ setError }: Props) {
  const t = useT();
  const [busy, setBusy] = useState<"rebuild" | "restart" | null>(null);
  const [status, setStatus] = useState("");
  const [output, setOutput] = useState("");

  const doRestart = async () => {
    setStatus(t("gs.restarting"));
    await api.system.restart();
    setStatus(t("gs.restartRequested"));
  };

  const handleRestart = async () => {
    if (!confirm(t("gs.restartConfirm"))) {
      return;
    }
    setError("");
    setOutput("");
    setBusy("restart");
    try {
      await doRestart();
    } catch (e) {
      setError(errMsg(e));
      setStatus("");
    } finally {
      setBusy(null);
    }
  };

  const handleRebuild = async () => {
    if (
      !confirm(
        t("gs.rebuildConfirm"),
      )
    ) {
      return;
    }
    setError("");
    setOutput("");
    setBusy("rebuild");
    setStatus(t("gs.building"));
    try {
      const res = await api.system.rebuild();
      if (res.output) setOutput(res.output);
      await doRestart();
    } catch (e) {
      setError(errMsg(e));
      setStatus("");
    } finally {
      setBusy(null);
    }
  };

  return (
    <SectionCard
      title={t("gs.system")}
      description={t("gs.systemDesc")}
      danger
    >
      <div className="flex flex-wrap gap-2">
        <Button variant="primary" onClick={handleRebuild} disabled={busy !== null}>
          {busy === "rebuild" ? t("gs.rebuilding") : t("gs.rebuildRestart")}
        </Button>
        <Button variant="danger" onClick={handleRestart} disabled={busy !== null}>
          {busy === "restart" ? t("gs.restarting") : t("gs.restart")}
        </Button>
      </div>
      {status && <p className="mt-3 text-[12px] text-ink-dim">{status}</p>}
      {output && (
        <pre className="mt-3 max-h-64 overflow-auto rounded-[8px] border border-hairline bg-raised p-3 font-mono text-[11px] text-ink-faint whitespace-pre-wrap">
          {output}
        </pre>
      )}
    </SectionCard>
  );
}
