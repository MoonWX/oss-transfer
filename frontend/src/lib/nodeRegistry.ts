import { api } from "./api";
import { t } from "./i18n";

export type NodeDisplay = {
  displayName?: string;
  displayAddress?: string;
  displayLabel?: string;
  identityOnly?: boolean;
  label?: string;
  hostname?: string;
  connectionAddress?: string;
  localIps?: string[];
};
export type NodeLabels = (
  identity: string | undefined,
  display?: NodeDisplay,
) => string;

function ipAddress(value?: string): string {
  if (!value) return "";
  const v4 = value.match(/^(\d{1,3}(?:\.\d{1,3}){3})(?::\d+)?$/)?.[1];
  if (v4 && v4.split(".").every((part) => Number(part) <= 255)) return v4;
  const v6 = value.match(/^\[([^\]]+)\](?::\d+)?$/)?.[1] ?? value;
  if (!v6.includes(":")) return "";
  try {
    return new URL(`http://[${v6}]`).hostname.slice(1, -1);
  } catch {
    return "";
  }
}

function hostAddress(value?: string): string {
  const ip = ipAddress(value);
  if (!ip) return "";
  if (!ip.includes(":"))
    return ip.startsWith("127.") || ip === "0.0.0.0" ? "" : ip;
  const [left, right] = ip.split("::");
  const first = left ? left.split(":") : [];
  const last = right ? right.split(":") : [];
  const words =
    right === undefined
      ? first
      : [...first, ...Array(8 - first.length - last.length).fill("0"), ...last];
  const values = words.map((word) => parseInt(word, 16));
  if (values.slice(0, 7).every((word) => word === 0) && values[7] <= 1)
    return "";
  if (
    values.slice(0, 5).every((word) => word === 0) &&
    values[5] === 0xffff &&
    (values[6] >>> 8 === 127 || (values[6] === 0 && values[7] === 0))
  )
    return "";
  return ip;
}

export function displayNode(
  identity: string | undefined,
  node: NodeDisplay = {},
): string {
  if (node.identityOnly && identity)
    return t("节点标识：{value1}", { value1: identity });
  if (node.displayLabel) return node.displayLabel;
  const name =
    node.displayName?.trim() || node.label?.trim() || node.hostname?.trim();
  const candidates = [
    node.displayAddress,
    node.connectionAddress,
    ...(node.localIps ?? []),
    identity,
  ];
  const address =
    candidates.map(hostAddress).find(Boolean) ||
    candidates.map(ipAddress).find(Boolean);
  if (name && address && name !== address) return `${name} (${address})`;
  return (
    name ||
    address ||
    (identity ? t("节点标识：{value1}", { value1: identity }) : "—")
  );
}

function hasNodeDisplay(node?: NodeDisplay): boolean {
  return (
    !!node &&
    !node.identityOnly &&
    !!(
      node.displayName ||
      node.label ||
      node.hostname ||
      node.displayAddress ||
      node.displayLabel ||
      hostAddress(node.connectionAddress) ||
      node.localIps?.some(hostAddress)
    )
  );
}

// Registry names take precedence; a worker can fill in an unreported address.
export function mergeNodeDisplay(
  preferred?: NodeDisplay,
  fallback?: NodeDisplay,
): NodeDisplay {
  const fields = Object.fromEntries(
    Object.entries(preferred ?? {}).filter(
      ([, value]) => value != null && value !== "",
    ),
  );
  if (preferred?.identityOnly && hasNodeDisplay(fallback)) return fallback!;
  const merged = { ...fallback, ...fields };
  const name =
    preferred?.displayName || preferred?.label || preferred?.hostname;
  if (name) {
    merged.displayName = name;
    if (!preferred?.displayLabel) delete merged.displayLabel;
    merged.identityOnly = false;
  }
  return merged;
}

// A current directory entry wins over snapshots attached to older tasks.
export function resolveNodeDisplay(
  identity: string | undefined,
  current?: NodeDisplay,
  snapshot?: NodeDisplay,
): string {
  return displayNode(
    identity,
    hasNodeDisplay(current) ? current : (snapshot ?? current),
  );
}

export function taskNodeLabels(
  task: Record<string, any>,
  resolve: NodeLabels = displayNode,
): string {
  const assignedNode = task.executionNodeIp || task.workerNode;
  const isChild =
    task.childTaskId ||
    task.shardId ||
    (task.parentTaskId != null && task.parentTaskId !== task.taskIndex);
  if (isChild && assignedNode) {
    const metadata = task.executionNodes?.find(
      (node: { address?: string; id?: string }) =>
        node.address === assignedNode || node.id === assignedNode,
    );
    return resolve(
      assignedNode,
      metadata ?? {
        displayName: task.executionNodeName,
        displayAddress: task.executionNodeAddress,
        displayLabel: task.executionNodeLabel,
        identityOnly: task.executionNodeIdentityOnly,
      },
    );
  }
  if (Array.isArray(task.executionNodes) && task.executionNodes.length)
    return task.executionNodes
      .map((node: NodeDisplay & { address?: string; id?: string }) =>
        resolve(node.address || node.id, node),
      )
      .join(", ");
  const identities = task.participatingWorkerNodes?.length
    ? task.participatingWorkerNodes
    : task.eligibleWorkerNodes?.length
      ? task.eligibleWorkerNodes
      : [assignedNode].filter(Boolean);
  return (
    identities
      .map((identity: string) =>
        resolve(
          identity,
          identities.length === 1
            ? {
                displayName: task.executionNodeName,
                displayAddress: task.executionNodeAddress,
                displayLabel: task.executionNodeLabel,
                identityOnly: task.executionNodeIdentityOnly,
              }
            : undefined,
        ),
      )
      .join(", ") || t("等待分配")
  );
}

export type RegisteredNode = NodeDisplay & {
  id?: string;
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
  masterAgentUrl?: string;
};
export function nodeName(node: NodeDisplay & { address: string }) {
  return displayNode(node.address, node);
}
export type NodeChange = {
  address: string;
  label: string;
  advertisedLocalRoots?: string[];
  maxConcurrentChildren?: number;
  objectsPerChild?: number;
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
export async function createAgentInvitation(
  change: Partial<NodeChange> & { masterUrl?: string },
  signal?: AbortSignal,
) {
  const response = await api<AgentInvitation & { data?: AgentInvitation }>(
    "/api/master/agents/invitations",
    { method: "POST", body: change, signal },
  );
  return response.data ?? response;
}
export async function getAgentInvitationStatus(
  id: string,
  signal: AbortSignal,
) {
  const response = await api<{
    data: {
      status: "pending" | "joined" | "expired" | "revoked";
      nodeId?: string;
    };
  }>(`/api/master/agents/invitations/${encodeURIComponent(id)}`, { signal });
  return response.data;
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
