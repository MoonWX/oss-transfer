import { taskNodeLabels, type NodeLabels } from "./nodeRegistry";

export type ExceptionMembership = {
  queriedWorkers?: string[];
  respondingNodes?: string[];
  failedWorkers?: string[];
  unreachableWorkers?: string[];
};

export function missingExceptionNodes(
  snapshot: ExceptionMembership | null,
  legacyNodes: string[],
): string[] {
  if (!snapshot) return [];
  const expected = snapshot.queriedWorkers ?? legacyNodes;
  return [
    ...new Set([
      ...(snapshot.respondingNodes
        ? expected.filter((node) => !snapshot.respondingNodes!.includes(node))
        : []),
      ...(snapshot.failedWorkers ?? []),
      ...(snapshot.unreachableWorkers ?? []),
    ]),
  ];
}

type ExceptionRecord = Record<string, unknown>;
type Field = { key: string; label: string; date?: boolean };
const fields: Field[] = [
  { key: "parentTaskId", label: "任务 ID" },
  { key: "childTaskId", label: "子任务 ID" },
  { key: "workerNode", label: "执行节点" },
  { key: "direction", label: "迁移方向" },
  { key: "sourceS3Alias", label: "源存储连接" },
  { key: "sourceBucket", label: "源 Bucket" },
  { key: "key", label: "源对象 Key" },
  { key: "destS3Alias", label: "目标存储连接" },
  { key: "destBucket", label: "目标 Bucket" },
  { key: "destinationKey", label: "目标对象 Key" },
  { key: "localPath", label: "本地路径" },
  { key: "logicalKey", label: "逻辑 Key" },
  { key: "objectSize", label: "对象大小（字节）" },
  { key: "stage", label: "失败阶段" },
  { key: "errorCode", label: "错误代码" },
  { key: "reasonCode", label: "原因代码" },
  { key: "errorMessage", label: "错误信息" },
  { key: "retryCount", label: "重试次数" },
  { key: "timestampFormatted", label: "记录时间", date: true },
  { key: "observedAt", label: "观测时间", date: true },
  { key: "targetCommitted", label: "目标已写入" },
  { key: "tmpFilePath", label: "临时文件路径" },
];

function timestamp(value: unknown): string | undefined {
  if (typeof value !== "number" && typeof value !== "string") return;
  if (value === "") return;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString();
}

function normalize(
  item: ExceptionRecord,
  resolve?: NodeLabels,
): ExceptionRecord {
  return {
    ...item,
    parentTaskId: item.parentTaskId ?? item.taskIndex,
    workerNode:
      item.workerNode || item.executionNodeIp
        ? taskNodeLabels(item, resolve)
        : undefined,
    nodeId: item.workerNode || item.executionNodeIp,
    timestampFormatted:
      timestamp(item.timestamp) ??
      timestamp(item.timestampFormatted) ??
      timestamp(item.observedAt),
    observedAt: timestamp(item.observedAt),
  };
}

export function exceptionDetails(item: ExceptionRecord, resolve?: NodeLabels) {
  const record = normalize(item, resolve);
  return fields.flatMap((field) => {
    const value = record[field.key];
    if (value === undefined || value === null || value === "") return [];
    if (field.key === "logicalKey" && value === record.key) return [];
    if (field.key === "reasonCode" && value === record.errorCode) return [];
    if (field.key === "observedAt" && value === record.timestampFormatted)
      return [];
    return [{ ...field, value }];
  });
}

export function exceptionCsv(
  rows: ExceptionRecord[],
  resolve?: NodeLabels,
): string {
  const columns = [...fields, { key: "nodeId", label: "节点标识" }];
  const cell = (value: unknown) => {
    let text = value == null ? "" : String(value);
    if (/^[=+\-@\t\r]/.test(text)) text = `'${text}`;
    return `"${text.replaceAll('"', '""')}"`;
  };
  return (
    "\uFEFF" +
    [
      columns.map(({ key }) => cell(key)).join(","),
      ...rows.map((row) => {
        const record = normalize(row, resolve);
        return columns.map(({ key }) => cell(record[key])).join(",");
      }),
    ].join("\r\n")
  );
}
