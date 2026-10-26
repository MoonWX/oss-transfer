import { api, ApiError, getToken } from "./api";
import { t } from "./i18n";

const inFlight = new Set<string>();
export type BatchProgress = {
  total: number;
  processed: number;
  accepted: number;
  ignored: number;
  failed: number;
  uncertain: number;
  unsubmitted: number;
};
export type BatchRunResult = {
  progress: BatchProgress;
  successfulIds: number[];
  failedIds: number[];
  uncertainIds: number[];
  unsubmittedIds: number[];
  issues: { taskIds: number[]; message: string }[];
};
export type TaskBatchRequest = { path: string; taskIds: number[] };

// Keep requests short and report progress without replaying an uncertain result.
// This is a transport batch size, not a selection limit.
const batchSize = 64;
export async function runTaskBatches(
  requests: TaskBatchRequest[],
  options: {
    isCurrent?: () => boolean;
    prepare?: (ids: number[]) => Promise<Record<string, unknown>>;
    onProgress?: (progress: BatchProgress) => void;
    onResult?: (
      detail: any,
      ids: number[],
      body: Record<string, unknown>,
    ) => void;
    onError?: (error: unknown, ids: number[], submitted: boolean) => void;
  } = {},
): Promise<BatchRunResult> {
  const batches = requests.flatMap(({ path, taskIds }) =>
    Array.from({ length: Math.ceil(taskIds.length / batchSize) }, (_, i) => ({
      path,
      taskIds: taskIds.slice(i * batchSize, (i + 1) * batchSize),
    })),
  );
  const pending = new Set(requests.flatMap((request) => request.taskIds));
  const progress: BatchProgress = {
    total: pending.size,
    processed: 0,
    accepted: 0,
    ignored: 0,
    failed: 0,
    uncertain: 0,
    unsubmitted: pending.size,
  };
  const result: BatchRunResult = {
    progress,
    successfulIds: [],
    failedIds: [],
    uncertainIds: [],
    unsubmittedIds: [],
    issues: [],
  };
  const token = getToken();
  const locks = requests.flatMap(({ path, taskIds }) =>
    taskIds.map((id) => `${token}\0${path.split("/").pop()}\0${id}`),
  );
  if (locks.some((key) => inFlight.has(key)))
    throw new ApiError(t("所选任务正在处理，请勿重复提交。"), 409);
  locks.forEach((key) => inFlight.add(key));
  const current = () => token === getToken() && (options.isCurrent?.() ?? true);
  const emit = () => {
    if (current()) options.onProgress?.({ ...progress });
  };
  const issue = (error: unknown, taskIds: number[]) =>
    result.issues.push({
      taskIds,
      message: error instanceof Error ? error.message : String(error),
    });
  try {
    emit();
    for (const { path, taskIds } of batches) {
      if (!current()) break;
      let body: Record<string, unknown>;
      try {
        body = { ...(await options.prepare?.(taskIds)), taskIds };
      } catch (error) {
        issue(error, taskIds);
        if (current()) options.onError?.(error, taskIds, false);
        break;
      }
      if (!current()) break;
      taskIds.forEach((id) => pending.delete(id));
      progress.unsubmitted = pending.size;
      let response: any;
      try {
        response = await api(path, { method: "POST", body });
      } catch (error) {
        const definitive =
          error instanceof ApiError &&
          error.status >= 400 &&
          error.status < 500 &&
          error.status !== 408;
        progress.processed += taskIds.length;
        if (definitive) {
          progress.failed += taskIds.length;
          result.failedIds.push(...taskIds);
        } else {
          progress.uncertain += taskIds.length;
          result.uncertainIds.push(...taskIds);
        }
        issue(error, taskIds);
        if (current()) options.onError?.(error, taskIds, true);
        emit();
        if (
          !definitive ||
          !current() ||
          (error instanceof ApiError && [401, 403].includes(error.status))
        )
          break;
        continue;
      }
      const detail = response.data || response;
      const details: any[] = Array.isArray(detail.details)
        ? detail.details
        : [];
      const byId = new Map(details.map((item) => [item.taskId, item]));
      const aggregateSuccess =
        !details.length &&
        !detail.partial &&
        !Number(detail.failed) &&
        !detail.failedWorkers?.length &&
        !detail.unreachableWorkers?.length &&
        Number(detail.accepted ?? 0) + Number(detail.ignored ?? 0) ===
          taskIds.length;
      const uncertain: number[] = [];
      for (const id of taskIds) {
        const item = byId.get(id);
        if (item?.success === false || item?.error) {
          progress.failed++;
          result.failedIds.push(id);
        } else if (item?.success === true || aggregateSuccess) {
          result.successfulIds.push(id);
          if (item?.ignored) progress.ignored++;
          else progress.accepted++;
        } else {
          progress.uncertain++;
          result.uncertainIds.push(id);
          uncertain.push(id);
        }
      }
      if (aggregateSuccess && Number(detail.ignored) > 0) {
        const ignored = Math.min(taskIds.length, Number(detail.ignored));
        progress.accepted -= ignored;
        progress.ignored += ignored;
      }
      progress.processed += taskIds.length;
      if (uncertain.length)
        issue(t("部分结果待确认，请刷新任务状态。"), uncertain);
      // Callbacks run outside the transport catch: a rendering error must not
      // reclassify an already acknowledged batch or count it twice.
      if (current()) options.onResult?.(detail, taskIds, body);
      emit();
      if (uncertain.length) break;
    }
  } finally {
    locks.forEach((key) => inFlight.delete(key));
  }
  result.unsubmittedIds = [...pending];
  progress.unsubmitted = pending.size;
  emit();
  return result;
}
