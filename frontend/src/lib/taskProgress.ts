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

const highest = new Map<string, number>();
// Live bytes drop when a failed object attempt restarts. A bar must not move
// backwards on that transient dip, so remember the highest value shown per
// task generation. This is display only; the server value stays exact.
export function monotonicPercent(task: Task, percent: number): number {
  const key = [task.taskIndex, task.subtaskNumber ?? "", task.generation].join(
    ":",
  );
  const shown = Math.max(percent, highest.get(key) ?? 0);
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
