import { api } from "./api";

export type RegisteredNode = {
  address: string;
  label: string;
  master: boolean;
  agent?: boolean;
  online?: boolean;
  lastSeen?: string;
};
export type NodeRegistry = {
  nodes: RegisteredNode[];
  port: number;
  masterWebUrl: string;
};
export type NodeChange = {
  address: string;
  label: string;
  advertisedLocalRoots?: string[];
};
export type AgentInvitation = {
  id: string;
  expiresAt: string;
  joinUrl: string;
  dockerCommand?: string;
  traditionalCommand?: string;
  windowsCommand?: string;
  nativeCommand?: string;
  nativeAgent?: { available: boolean };
};
export async function createAgentInvitation(change: Partial<NodeChange>) {
  const response = await api<AgentInvitation & { data?: AgentInvitation }>(
    "/api/master/agents/invitations",
    { method: "POST", body: change },
  );
  return response.data ?? response;
}
export function saveNode(originalAddress: string | null, change: NodeChange) {
  return api<{ data: NodeRegistry }>(
    `/api/master/nodes${originalAddress ? `/${encodeURIComponent(originalAddress)}` : ""}`,
    { method: originalAddress ? "PUT" : "POST", body: change },
  );
}
export function removeNode(address: string) {
  return api<{ data: NodeRegistry }>(
    `/api/master/nodes/${encodeURIComponent(address)}`,
    { method: "DELETE" },
  );
}
