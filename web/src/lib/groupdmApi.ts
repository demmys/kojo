const BASE = "";

export interface GroupDMInfo {
  id: string;
  name: string;
  members: GroupMember[];
  createdAt: string;
  updatedAt: string;
}

export interface GroupMember {
  agentId: string;
  agentName: string;
}

export interface GroupMessage {
  id: string;
  agentId: string;
  agentName: string;
  content: string;
  timestamp: string;
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(BASE + path);
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  return res.json();
}

async function post<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(BASE + path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  return res.json();
}

async function del<T>(path: string): Promise<T> {
  const res = await fetch(BASE + path, { method: "DELETE" });
  if (!res.ok) throw new Error(`${res.status}: ${await res.text()}`);
  return res.json();
}

export const groupdmApi = {
  list: () =>
    get<{ groups: GroupDMInfo[] }>("/api/v1/groupdms").then((r) => r.groups ?? []),

  get: (id: string) => get<GroupDMInfo>(`/api/v1/groupdms/${id}`),

  create: (name: string, memberIds: string[]) =>
    post<GroupDMInfo>("/api/v1/groupdms", { name, memberIds }),

  delete: (id: string) => del<{ ok: boolean }>(`/api/v1/groupdms/${id}`),

  messages: (id: string, limit = 50, before?: string) => {
    const params = new URLSearchParams({ limit: String(limit) });
    if (before) params.set("before", before);
    return get<{ messages: GroupMessage[]; hasMore: boolean }>(
      `/api/v1/groupdms/${id}/messages?${params}`,
    ).then((r) => ({ messages: r.messages ?? [], hasMore: r.hasMore ?? false }));
  },

  postMessage: (groupId: string, agentId: string, content: string) =>
    post<GroupMessage>(`/api/v1/groupdms/${groupId}/messages`, { agentId, content }),

  forAgent: (agentId: string) =>
    get<{ groups: GroupDMInfo[] }>(`/api/v1/agents/${agentId}/groups`).then(
      (r) => r.groups ?? [],
    ),
};
