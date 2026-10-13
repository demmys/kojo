import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router";
import {
  getSlackMigration,
  retrySlackMigration,
  type SlackMigrationPending,
  type SlackMigrationStatus,
} from "../../lib/slackMigrationApi";
import { errMsg } from "../../lib/utils";
import { Button } from "../ui/Button";
import { useT } from "../../lib/i18n";

export interface SlackMigrationState {
  /** Set while this agent's built-in Slack bot has not been moved yet. */
  pending?: SlackMigrationPending;
  /** Where the Slack package is published. */
  source: string;
  retrying: boolean;
  /** Why the retry request itself failed (not why the migration did). */
  retryError: string;
  retry: () => void;
}

/**
 * Loads whether this agent still has a built-in Slack bot waiting to be
 * moved into the Slack extension package. A failed load reads as "nothing
 * pending": the notice is an aid, and an agent that never used Slack must
 * not show an error about it.
 */
export function useSlackMigration(agentId: string | undefined): SlackMigrationState {
  const [status, setStatus] = useState<SlackMigrationStatus | null>(null);
  const [retrying, setRetrying] = useState(false);
  const [retryError, setRetryError] = useState("");

  useEffect(() => {
    let cancelled = false;
    setStatus(null);
    setRetryError("");
    if (!agentId) return;
    getSlackMigration()
      .then((s) => {
        if (!cancelled) setStatus(s);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [agentId]);

  const retry = useCallback(() => {
    setRetrying(true);
    setRetryError("");
    retrySlackMigration()
      .then(setStatus)
      .catch((e) => setRetryError(errMsg(e)))
      .finally(() => setRetrying(false));
  }, []);

  return {
    pending: status?.pending.find((p) => p.agentId === agentId),
    source: status?.source ?? "",
    retrying,
    retryError,
    retry,
  };
}

/**
 * Tells the Owner that this agent's Slack bot — set up when Slack was built
 * into kojo — has not been moved into the Slack extension package yet, why,
 * and what to do. Renders nothing once the move is done: from then on Slack
 * is configured under Settings → Extensions like any other package.
 */
export function SlackMigrationNotice({ state }: { state: SlackMigrationState }) {
  const t = useT();
  const { pending, source, retrying, retryError, retry } = state;
  if (!pending) return null;
  return (
    <div
      role="status"
      className="space-y-3 rounded-[10px] border border-lamp-warn/40 bg-lamp-warn/5 px-3 py-3 text-[13px]"
    >
      <div className="font-medium text-lamp-warn">{t("slackMig.title")}</div>
      <p className="text-ink-dim">{t("slackMig.body")}</p>
      <div>
        <p className="text-ink">{t(`slackMig.reason.${pending.reason}`)}</p>
        {pending.detail && (
          <p className="mt-1 break-words font-mono text-[11px] text-ink-faint">
            {pending.detail}
          </p>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <Button variant="primary" onClick={retry} disabled={retrying}>
          {retrying ? t("slackMig.retrying") : t("slackMig.retry")}
        </Button>
        <Link
          to="/settings"
          className="rounded-[10px] border border-hairline bg-raised px-3 py-1.5 text-sm text-ink transition-colors hover:bg-hover"
        >
          {t("slackMig.openExtensions")}
        </Link>
      </div>
      {retryError && (
        <p className="break-words text-lamp-err">
          {t("slackMig.retryFailed", { error: retryError })}
        </p>
      )}
      <div className="text-ink-dim">
        <div>{t("slackMig.manualTitle")}</div>
        <ol className="mt-1 list-decimal space-y-1 pl-5">
          <li>
            {t("slackMig.manualInstall")}
            {source && (
              <code className="mt-1 block select-all break-all font-mono text-[12px] text-ink">
                {source}
              </code>
            )}
          </li>
          <li>{t("slackMig.manualRetry")}</li>
          <li>{t("slackMig.manualBind")}</li>
        </ol>
      </div>
    </div>
  );
}
