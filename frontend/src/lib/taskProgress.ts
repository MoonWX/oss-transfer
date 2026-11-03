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
