import type { AgentInfo, AgentUpdateParams } from "../../lib/agentApi";
import { PreconditionFailedError } from "../../lib/httpClient";
import { errMsg } from "../../lib/utils";

export interface BulkFailure {
  id: string;
  name: string;
  reason: string;
}

export interface BulkResult {
  updated: number;
  failed: BulkFailure[];
}

export interface BulkApi {
  /** Fresh read for the row etag (GET /agents/{id}; proxied to the holder). */
  get: (id: string) => Promise<Pick<AgentInfo, "etag">>;
  update: (id: string, cfg: AgentUpdateParams, etag?: string) => Promise<unknown>;
}

/**
 * Turn an agentApi error ("502: {\"error\":{\"code\":...,\"message\":...}}")
 * into a short human-readable reason. Falls back to the raw message.
 */
export function bulkFailureReason(e: unknown): string {
  const msg = errMsg(e);
  const m = /^(\d{3}): (.*)$/s.exec(msg);
  if (!m) return msg;
  try {
    const body = JSON.parse(m[2]);
    const err = body?.error ?? body;
    const code = typeof err?.code === "string" ? err.code : "";
    const text = typeof err?.message === "string" ? err.message : "";
    if (code || text) return [code, text].filter(Boolean).join(": ");
  } catch {
    // not JSON
  }
  return msg;
}

/**
 * Apply one response language to every given (non-archived) agent via
 * the normal per-agent PATCH, so remote-held agents go through the same
 * holder proxy as a regular settings save. Sequential: one failure (e.g.
 * holder offline) never aborts the rest; each is reported for retry.
 * No global value is stored anywhere.
 *
 * Each PATCH carries If-Match from a fresh GET so the bulk action also
 * works when the server requires If-Match (KOJO_REQUIRE_IF_MATCH); a 412
 * from a concurrent edit is retried once with a re-read etag.
 */
export async function applyResponseLanguageToAll(
  agents: Pick<AgentInfo, "id" | "name" | "archived">[],
  language: string,
  api: BulkApi,
): Promise<BulkResult> {
  const responseLanguage = language.trim();
  const result: BulkResult = { updated: 0, failed: [] };
  for (const a of agents) {
    if (a.archived) continue;
    try {
      const patch = async () => {
        const { etag } = await api.get(a.id);
        await api.update(a.id, { responseLanguage }, etag || undefined);
      };
      try {
        await patch();
      } catch (e) {
        if (!(e instanceof PreconditionFailedError)) throw e;
        await patch();
      }
      result.updated++;
    } catch (e) {
      result.failed.push({ id: a.id, name: a.name, reason: bulkFailureReason(e) });
    }
  }
  return result;
}
