import { useEffect, useState, useSyncExternalStore } from "react";
import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
} from "@mui/material";
import { ReplayRounded } from "@mui/icons-material";
import { api, ApiError, getToken, useNotify } from "../lib/api";
import { t as tr, useI18n } from "../lib/i18n";
import { formatNumber } from "./Common";

// Retrying the failed objects of a RUNNING task reopens its finished child
// tasks in place; the transfer keeps running. The server ignores children
// that are already retrying, so this lock only prevents duplicate clicks
// until a fresh snapshot shows the retry (fewer failed objects, or the task
// left RUNNING), or a short while has passed.
export type RunningRetryTask = {
  taskIndex: number;
  status: string;
  generation?: number;
  failedObjectCount?: number;
  legacyHistorical?: boolean;
  migrationRecoveryRequired?: boolean;
};
type Pending = {
  phase: "submitting" | "accepted";
  failed: number;
  until: number;
  session: string | null;
};
type RetryResult = {
  accepted: number;
  ignored: number;
  failed: number;
  retriedObjects: number;
  hasMore?: boolean;
  message?: string;
};

const lockMs = 20000;
const listeners = new Set<() => void>();
let pending: Record<number, Pending> = {};
function publish(next: Record<number, Pending>) {
  pending = next;
  listeners.forEach((listener) => listener());
}
function release(id: number) {
  const next = { ...pending };
  delete next[id];
  publish(next);
}
function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}
const failedCount = (task: RunningRetryTask) =>
  Math.max(0, Number(task.failedObjectCount) || 0);

export function canRetryRunningFailures(
  task?: RunningRetryTask | null,
): task is RunningRetryTask {
  return (
    !!task &&
    task.status === "RUNNING" &&
    failedCount(task) > 0 &&
    !task.legacyHistorical &&
    !task.migrationRecoveryRequired &&
    Number.isSafeInteger(task.generation) &&
    Number(task.generation) > 0
  );
}

export default function RunningFailureRetry({
  task,
  disabled,
  size = "medium",
  onSubmitted,
  onStale,
}: {
  task: RunningRetryTask;
  disabled?: boolean;
  size?: "small" | "medium";
  onSubmitted?: () => void;
  // Receives a fresh snapshot that no longer offers this retry (for example,
  // the task ended), so the page can use its ordinary action instead.
  onStale?: (task: RunningRetryTask) => void;
}) {
  useI18n();
  const notify = useNotify();
  const states = useSyncExternalStore(subscribe, () => pending);
  const [open, setOpen] = useState(false);
  const [error, setError] = useState("");
  const [checking, setChecking] = useState(false);
  const [confirmed, setConfirmed] = useState<RunningRetryTask>(task);
  const [, setTick] = useState(0);
  const id = task.taskIndex;
  const failed = failedCount(task);
  const entry = states[id]?.session === getToken() ? states[id] : undefined;
  const waiting =
    !!entry &&
    (entry.phase === "submitting" ||
      (Date.now() < entry.until &&
        task.status === "RUNNING" &&
        failed >= entry.failed));
  useEffect(() => {
    if (!entry || entry.phase !== "accepted") return;
    if (!waiting) {
      release(id);
      return;
    }
    // Re-render when the lock expires, also without automatic refresh.
    const timer = window.setTimeout(
      () => setTick((value) => value + 1),
      Math.max(0, entry.until - Date.now()) + 50,
    );
    return () => window.clearTimeout(timer);
  }, [entry, waiting, id]);
  const submitting = entry?.phase === "submitting";
  // A list snapshot can be older than the task: confirm it is still running
  // with failed objects before offering the retry, and show the fresh count.
  async function prepare() {
    if (checking || waiting) return;
    const session = getToken();
    setError("");
    setChecking(true);
    try {
      const { data } = await api<{ data: RunningRetryTask }>(
        `/api/master/tasks/${id}`,
        { timeoutMs: 10000 },
      );
      if (session !== getToken()) return;
      const fresh = { ...data, taskIndex: id };
      if (canRetryRunningFailures(fresh)) {
        setConfirmed(fresh);
        setOpen(true);
      } else if (onStale) onStale(fresh);
      else {
        notify(tr("任务状态已更新，请查看任务详情。"), "info");
        onSubmitted?.();
      }
    } catch (cause) {
      if (session !== getToken()) return;
      notify(cause instanceof Error ? cause.message : String(cause), "error");
    } finally {
      setChecking(false);
    }
  }
  async function submit() {
    if (waiting || !canRetryRunningFailures(confirmed)) return;
    const session = getToken();
    const submittedFailed = failedCount(confirmed);
    publish({
      ...pending,
      [id]: { phase: "submitting", failed: submittedFailed, until: 0, session },
    });
    setError("");
    try {
      const { data } = await api<{ data: RetryResult }>(
        `/api/master/tasks/${id}/retry-failed`,
        {
          method: "POST",
          body: { expectedGeneration: confirmed.generation },
          timeoutMs: 70000,
        },
      );
      if (session !== getToken()) return;
      setOpen(false);
      if (data.accepted > 0) {
        publish({
          ...pending,
          [id]: {
            phase: "accepted",
            failed: Math.min(failed, submittedFailed),
            until: Date.now() + lockMs,
            session,
          },
        });
        const summary =
          tr(
            "已开始重试任务 #{id} 的 {objects} 个失败对象（{children} 个子任务），正在进行的传输不受影响。",
            {
              id,
              objects: formatNumber(data.retriedObjects),
              children: formatNumber(data.accepted),
            },
          ) +
          (data.hasMore ? tr("还有更多失败的子任务，稍后可再次重试。") : "");
        if (data.failed > 0 && data.message) {
          setError(
            tr("部分失败项未能重试：{value1}", { value1: data.message }),
          );
          notify(summary, "warning");
        } else notify(summary, "success");
      } else {
        release(id);
        const message = data.message || tr("没有可立即重试的失败对象。");
        if (data.failed > 0) setError(message);
        notify(message, data.failed > 0 ? "warning" : "info");
      }
      onSubmitted?.();
    } catch (cause) {
      if (session !== getToken()) return;
      release(id);
      const message =
        cause instanceof ApiError || cause instanceof Error
          ? cause.message
          : String(cause);
      setError(message);
      notify(message, "error");
      onSubmitted?.();
    }
  }
  const titleId = `running-retry-title-${id}`;
  return (
    <>
      <Button
        size={size}
        variant="contained"
        startIcon={<ReplayRounded />}
        disabled={disabled || waiting || checking}
        onClick={() => void prepare()}
      >
        {submitting
          ? tr("提交中…")
          : waiting
            ? tr("重试中…")
            : checking
              ? tr("检查状态…")
              : tr("重试失败项")}
      </Button>
      {error && !open && (
        <Alert
          severity="warning"
          onClose={() => setError("")}
          sx={{ flexBasis: "100%" }}
        >
          {error}
        </Alert>
      )}
      <Dialog
        open={open}
        onClose={() => {
          if (!submitting) setOpen(false);
        }}
        aria-labelledby={titleId}
      >
        <DialogTitle id={titleId}>{tr("重试运行中任务的失败项？")}</DialogTitle>
        <DialogContent>
          <DialogContentText>
            {tr(
              "仅重新传输任务 #{id} 中已结束子任务的失败对象（当前共 {count} 个失败对象）。正在进行的传输不会中断，已成功或已跳过的对象不会重复传输。",
              { id, count: formatNumber(failedCount(confirmed)) },
            )}
          </DialogContentText>
          <DialogContentText sx={{ mt: 1.5 }}>
            {tr("仍在运行的子任务中的失败对象，需等该子任务结束后再重试。")}
          </DialogContentText>
          {error && open && (
            <Alert severity="warning" sx={{ mt: 2 }}>
              {error}
            </Alert>
          )}
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setOpen(false)} disabled={submitting}>
            {tr("取消")}
          </Button>
          <Button
            variant="contained"
            onClick={() => void submit()}
            disabled={submitting || waiting}
          >
            {submitting ? tr("提交中…") : tr("确认重试")}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}
