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

// What the current speed and live bytes count (transferBasis): TARGET is
// bytes sent to the destination; SOURCE (every node an older Agent) and
// MIXED (some) count source reads on those nodes. With an Agent that has no
// live progress at all (liveProgress UNSUPPORTED), progress counts completed
// objects only.
export function speedBasisNote(task: Task): string | null {
  const completedOnly = task.liveProgress === "UNSUPPORTED";
  switch (task.transferBasis) {
    case "SOURCE":
      return completedOnly
        ? t("执行节点版本较旧，速度按读取源端计算，进度只计已完成的对象")
        : t("执行节点版本较旧，速度和进度按读取源端计算");
    case "MIXED":
      return completedOnly
        ? t(
            "部分执行节点版本较旧，这些节点的速度按读取源端计算；进度只计已完成的对象",
          )
        : t("部分执行节点版本较旧，这些节点的速度和进度按读取源端计算");
  }
  return null;
}

export function speedHint(task: Task): string | undefined {
  return task.transferBasis === "TARGET"
    ? t("发往目标端的速率（最近几秒的平均值），包含请求失败后重新发送的字节。")
    : undefined;
}

// The current speed, with a second line when it counts source reads.
export function currentSpeedText(
  task: Task,
  format: (bytes: number) => string,
): string {
  const speed = task.currentSpeedBytesPerSecond;
  if (speed == null) return "—";
  const note = speedBasisNote(task);
  return note ? `${format(speed)}/s\n${note}` : `${format(speed)}/s`;
}

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
