import { useState } from "react";
import { api } from "../../lib/api";
import { errMsg } from "../../lib/utils";
import { SectionCard } from "../ui/SectionCard";
import { Button } from "../ui/Button";

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
  const [busy, setBusy] = useState<"rebuild" | "restart" | null>(null);
  const [status, setStatus] = useState("");
  const [output, setOutput] = useState("");

  const doRestart = async () => {
    setStatus("Restarting...");
    await api.system.restart();
    setStatus("Restart requested. The server is re-execing; reload in a moment.");
  };

  const handleRestart = async () => {
    if (!confirm("Restart the server now? In-flight agent turns will drain first.")) {
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
        "Rebuild the server (`make build`) and restart? This can take several minutes.",
      )
    ) {
      return;
    }
    setError("");
    setOutput("");
    setBusy("rebuild");
    setStatus("Building... (this can take several minutes)");
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
      title="System"
      description="Rebuild the server from source or restart the running daemon. Restart drains in-flight agent turns before re-execing, so it may take a moment."
      danger
    >
      <div className="flex flex-wrap gap-2">
        <Button variant="primary" onClick={handleRebuild} disabled={busy !== null}>
          {busy === "rebuild" ? "Rebuilding..." : "Rebuild & Restart"}
        </Button>
        <Button variant="danger" onClick={handleRestart} disabled={busy !== null}>
          {busy === "restart" ? "Restarting..." : "Restart"}
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
