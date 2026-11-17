import { t } from "./i18n";

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

// The bar and the byte label of a row show the Master's values, as the
// details and the CLI do. The Master keeps live progress from dipping on a
// sample taken before the ledger credited an object, and the Agent on a
// retried range, so a lower value is real: a failed or cancelled object whose
// bytes are lost, or, with liveProgress PARTIAL, a node that did not report
// (see unreportedNodes).
export function shownProgress(
  task: Task,
  percent = taskPercent(task),
): { percent: number; bytes: number } {
  return { percent, bytes: transferredBytes(task) };
}

export type UnreportedNode = {
  displayLabel?: string;
  executionNodeIp?: string;
};

/** Names of the nodes missing from a PARTIAL live sample, if any. */
export function unreportedNodeNames(task: Task): string | undefined {
  const nodes = task.unreportedNodes as UnreportedNode[] | undefined;
  if (!Array.isArray(nodes) || nodes.length === 0) return undefined;
  return nodes
    .map((node) => node.displayLabel || node.executionNodeIp || "")
    .filter(Boolean)
    .join(t("、"));
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
