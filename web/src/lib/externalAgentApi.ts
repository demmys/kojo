import { get, post, patch, del } from "./httpClient";

/** External AI agent registered by the owner (id "xa_..."). Mirrors the
 * server's externalAgentView. Times are unix millis. */
export interface ExternalAgent {
  id: string;
  name: string;
  description: string;
  paired: boolean;
  hasToken: boolean;
  nodeName?: string;
  enabled: boolean;
  ratePerMin: number;
  online: boolean;
  pairingExpiresAt?: number;
  lastSeenAt?: number;
  createdAt: number;
  updatedAt: number;
}

export interface PairingCode {
  code: string;
  expiresAt: number;
  /** Present only when the external listener is running. */
  pairUrl?: string;
}

export interface ExternalNode {
  stableNodeId: string;
  nodeName: string;
  tailnetIps: string[];
  externalId: string;
  createdAt: number;
  updatedAt: number;
}

export function isExternalId(id: string): boolean {
  return id.startsWith("xa_");
}

export const externalAgentApi = {
  list: () =>
    get<{ externalAgents: ExternalAgent[] | null; listenerUrl?: string }>(
      "/api/v1/external-agents",
    ).then((r) => ({ agents: r.externalAgents ?? [], listenerUrl: r.listenerUrl ?? "" })),

  create: (name: string, description: string) =>
    post<ExternalAgent>("/api/v1/external-agents", { name, description }),

  update: (
    id: string,
    body: Partial<Pick<ExternalAgent, "name" | "description" | "enabled" | "ratePerMin">>,
  ) => patch<ExternalAgent>(`/api/v1/external-agents/${id}`, body),

  delete: (id: string) => del<unknown>(`/api/v1/external-agents/${id}`),

  pairingCode: (id: string) => post<PairingCode>(`/api/v1/external-agents/${id}/pairing-code`),

  revoke: (id: string) => post<ExternalAgent>(`/api/v1/external-agents/${id}/revoke`),

  listNodes: () =>
    get<{ nodes: ExternalNode[] | null }>("/api/v1/external-nodes").then((r) => r.nodes ?? []),

  deleteNode: (stableNodeId: string) =>
    del<unknown>(`/api/v1/external-nodes/${encodeURIComponent(stableNodeId)}`),
};
