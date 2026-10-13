// Migration of the former built-in Slack bot — client for
// /api/v1/slack-migration.
//
// Slack is an extension package now. kojo moves an agent's old Slack
// settings, tokens and thread history into the package by itself at
// startup; an agent appears here only while that has not finished.

import { get, post } from "./httpClient";

/** Why an agent is still waiting. Mirrors the server's reason codes. */
export type SlackMigrationReason =
  | "in_progress"
  | "extensions_unavailable"
  | "install_failed"
  | "review_required"
  | "untrusted_source"
  | "incompatible_package"
  | "tokens_unreadable"
  | "bind_failed"
  | "history_copy_failed"
  | "cleanup_failed";

export interface SlackMigrationPending {
  agentId: string;
  reason: SlackMigrationReason;
  /** The underlying error text, when there is one. */
  detail?: string;
}

export interface SlackMigrationStatus {
  pending: SlackMigrationPending[];
  /** Where the Slack package is published, for a manual install. */
  source: string;
}

async function normalize(
  req: Promise<Partial<SlackMigrationStatus>>,
): Promise<SlackMigrationStatus> {
  const res = await req;
  return { pending: res.pending ?? [], source: res.source ?? "" };
}

export function getSlackMigration(): Promise<SlackMigrationStatus> {
  return normalize(get<Partial<SlackMigrationStatus>>("/api/v1/slack-migration"));
}

/** Runs the migration again and resolves with what is still pending. */
export function retrySlackMigration(): Promise<SlackMigrationStatus> {
  return normalize(
    post<Partial<SlackMigrationStatus>>("/api/v1/slack-migration/retry", {}),
  );
}
