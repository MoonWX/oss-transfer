import { api, getToken } from "./api";
import { throwIfAborted } from "./abort";
import { t } from "./i18n";

type Endpoint = {
  type?: string;
  localPath?: string;
  nodeBindings?: { localPath?: string }[];
  s3Alias?: string;
  bucket?: string;
};

export type TaskCatalogEntry = {
  taskIndex: number;
  status: string;
  generation?: number;
  direction?: string;
  sourceS3Alias?: string;
  destS3Alias?: string;
  sourceBucket?: string;
  destBucket?: string;
  localPath?: string;
  sourceEndpoint?: Endpoint;
  destinationEndpoint?: Endpoint;
  failedObjectCount?: number;
};

export async function loadTaskCatalog(
  signal: AbortSignal,
): Promise<TaskCatalogEntry[]> {
  const token = getToken();
  const tasks = new Map<number, TaskCatalogEntry>();
  let before: number | undefined;
  for (;;) {
    throwIfAborted(signal);
    if (token !== getToken()) throw new Error(t("登录已过期，请重新登录。"));
    const query = new URLSearchParams({ limit: "1000" });
    if (before !== undefined) query.set("beforeId", String(before));
    const page = await api<{
      tasks: TaskCatalogEntry[];
      hasMore?: boolean;
      nextBeforeId?: number;
      partial?: boolean;
      pageComplete?: boolean;
      truncated?: boolean;
    }>(`/api/master/status?${query}`, {
      signal,
      timeoutMs: 15000,
    });
    throwIfAborted(signal);
    if (token !== getToken()) throw new Error(t("登录已过期，请重新登录。"));
    if (
      !Array.isArray(page.tasks) ||
      page.partial ||
      page.pageComplete === false
    )
      throw new Error(t("无法完整读取任务列表，请刷新后重试。"));
    for (const task of page.tasks) {
      if (
        !Number.isSafeInteger(task.taskIndex) ||
        task.taskIndex <= 0 ||
        typeof task.status !== "string"
      )
        throw new Error(t("无法完整读取任务列表，请刷新后重试。"));
      if (!tasks.has(task.taskIndex)) tasks.set(task.taskIndex, task);
    }
    if (!page.hasMore && !page.truncated) break;
    const next = page.nextBeforeId;
    if (
      !Number.isSafeInteger(next) ||
      Number(next) <= 0 ||
      (before !== undefined && Number(next) >= before)
    )
      throw new Error(t("无法完整读取任务列表，请刷新后重试。"));
    before = Number(next);
  }
  return [...tasks.values()].sort((a, b) => b.taskIndex - a.taskIndex);
}

export function catalogEndpoint(task: TaskCatalogEntry, source: boolean) {
  const value = source ? task.sourceEndpoint : task.destinationEndpoint;
  if (
    value?.type === "LOCAL" ||
    (task.direction &&
      task.direction !== "OSS_TO_OSS" &&
      source === (task.direction === "LOCAL_TO_OSS"))
  )
    return (
      value?.localPath ||
      value?.nodeBindings?.[0]?.localPath ||
      task.localPath ||
      t("本地文件系统")
    );
  return (
    [
      source ? task.sourceS3Alias : task.destS3Alias,
      source ? task.sourceBucket : task.destBucket,
    ]
      .filter(Boolean)
      .join("/") ||
    [value?.s3Alias, value?.bucket].filter(Boolean).join("/") ||
    "—"
  );
}
