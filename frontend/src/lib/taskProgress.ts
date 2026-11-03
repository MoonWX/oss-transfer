import { t as tr, getLocale } from "./i18n";

type Task = Record<string, any>;

// Master computes the percent so the UI and CLI agree. Older Masters send
// counts only, so keep their count formula as the fallback.
export function taskPercent(task: Task): number {
  if (typeof task.progressPercent === "number") return task.progressPercent;
  const total = Number(task.listObjectCount || 0);
  if (!total) return task.status === "COMPLETED" ? 100 : 0;
  const handled =
    Number(task.putObjectCount || 0) + Number(task.skippedObjectCount || 0);
  return Math.min(100, (handled / total) * 100);
}

export const transferredBytes = (task: Task): number =>
  typeof task.transferredBytes === "number"
    ? task.transferredBytes
    : Number(task.putObjectSize || 0);

export const byteProgress = (task: Task) =>
  task.progressBasis === "BYTES" && Number(task.listObjectSize) > 0;

const highest = new Map<string, { percent: number; bytes: number }>();
// Live bytes drop when a failed object attempt restarts. A bar must not move
// backwards on that transient dip, so remember the highest sample shown while
// the row is live. Ended rows and non-LIVE samples show the exact server value:
// a stop, failure or failover discards in-flight bytes for good. The attempt
// and node are in the key so a reassigned child starts fresh. The byte label
// follows the same sample so it never contradicts the bar.
export function shownProgress(
  task: Task,
  percent = taskPercent(task),
): { percent: number; bytes: number } {
  const key = [
    task.taskIndex,
    task.subtaskNumber ?? "",
    task.generation,
    task.attempt ?? "",
    task.executionNodeName ?? "",
  ].join(":");
  const sample = { percent, bytes: transferredBytes(task) };
  if (
    !["RUNNING", "STOPPING"].includes(task.status) ||
    task.liveProgress !== "LIVE"
  ) {
    highest.delete(key);
    return sample;
  }
  const previous = highest.get(key);
  const shown = previous && previous.percent > percent ? previous : sample;
  highest.delete(key);
  highest.set(key, shown);
  if (highest.size > 1000) highest.delete(highest.keys().next().value!);
  return shown;
}

type RequestKinds = {
  list?: number;
  get?: number;
  head?: number;
  put?: number;
};
export type TaskRequestRates = {
  observed?: RequestKinds | null;
  configured?: RequestKinds | null;
  effective?: RequestKinds | null;
};

const rate = (value?: number) =>
  Number(value || 0).toLocaleString(getLocale(), { maximumFractionDigits: 1 });
// Limits follow the task-limit convention: zero means unlimited.
const limits = (value: RequestKinds) =>
  (
    [
      ["LIST / GET", value.list],
      ["HEAD", value.head],
      ["PUT", value.put],
    ] as const
  )
    .map(([name, limit]) => `${name} ${limit ? rate(limit) : tr("不限")}`)
    .join(" · ");

// Observed rates count admitted HTTP attempts, retries included. Unknown
// (an unreachable or older Agent) shows "—" rather than a partial sum.
export function taskRequestRateText(rates?: TaskRequestRates | null) {
  if (!rates) return null;
  const observed = rates.observed;
  const value = observed
    ? tr("{value1} 次/秒", {
        value1: (
          [
            ["LIST", observed.list],
            ["GET", observed.get],
            ["HEAD", observed.head],
            ["PUT", observed.put],
          ] as const
        )
          .map(([name, count]) => `${name} ${rate(count)}`)
          .join(" · "),
      })
    : "—";
  const detail = [
    rates.configured &&
      tr("任务上限 {value1}", { value1: limits(rates.configured) }),
    rates.effective &&
      tr("生效上限 {value1}", { value1: limits(rates.effective) }),
  ].filter((line): line is string => !!line);
  return { value, detail };
}
