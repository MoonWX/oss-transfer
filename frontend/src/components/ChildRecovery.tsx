import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { Alert, Button, CircularProgress, Stack } from "@mui/material";
import { PlayArrowRounded } from "@mui/icons-material";
import { api, ApiError, getToken, useNotify } from "../lib/api";
import { t } from "../lib/i18n";

type Recovery = {
  phase: "submitting" | "accepted";
  baselineAttempt?: number;
  token: string | null;
  observedAt?: number;
  sawRunning: boolean;
};
const recoveries = new Map<string, Recovery>();
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((listener) => listener());
const subscribe = (listener: () => void) => {
  listeners.add(listener);
  return () => listeners.delete(listener);
};
function clearRecovery(id: string, recovery: Recovery) {
  if (recoveries.get(id) === recovery) {
    recoveries.delete(id);
    emit();
  }
}
function observeRecovery(id: string, recovery: Recovery) {
  // Keep accepted requests locked across drawer/page changes. An old cached
  // RECOVERABLE row is not evidence that another recovery is needed.
  setTimeout(async () => {
    if (recoveries.get(id) !== recovery) return;
    if (getToken() !== recovery.token) {
      clearRecovery(id, recovery);
      return;
    }
    const [parent, child] = id.split("-");
    try {
      const result = await api<{
        data: {
          items: Array<{
            status: string;
            observedAt?: number;
            attempt?: number;
          }>;
        };
      }>(`/api/master/tasks/${parent}/children/${child}`, {
        signal: AbortSignal.timeout(15000),
      });
      const current = result.data.items[0];
      if (current) {
        if (
          ["COMPLETED", "PARTIAL_FAILED", "FAILED", "STOPPED"].includes(
            current.status,
          ) ||
          (current.status === "RECOVERABLE" &&
            ((current.attempt !== undefined &&
              recovery.baselineAttempt !== undefined &&
              current.attempt > recovery.baselineAttempt) ||
              (recovery.sawRunning &&
                current.observedAt !== undefined &&
                recovery.observedAt !== undefined &&
                current.observedAt > recovery.observedAt)))
        ) {
          clearRecovery(id, recovery);
          return;
        }
        if (current.status === "RUNNING") {
          recovery.sawRunning = true;
          recovery.observedAt = current.observedAt;
        }
      }
    } catch (cause) {
      if (cause instanceof ApiError && [401, 403, 404].includes(cause.status)) {
        clearRecovery(id, recovery);
        return;
      }
    }
    observeRecovery(id, recovery);
  }, 2000);
}

export default function ChildRecovery({
  childId,
  attempt,
  observedAt,
  onRefresh,
}: {
  childId: string;
  attempt?: number;
  observedAt?: number;
  onRefresh: () => void;
}) {
  const notify = useNotify();
  const [error, setError] = useState("");
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  const phase = useSyncExternalStore(subscribe, () => {
    const current = recoveries.get(childId);
    return current?.token === getToken() ? current.phase : undefined;
  });

  const recover = async () => {
    const match = /^([1-9]\d*)-([1-9]\d*)$/.exec(childId);
    const existing = recoveries.get(childId);
    if (!match || (existing && existing.token === getToken())) return;
    const recovery: Recovery = {
      phase: "submitting",
      baselineAttempt: attempt,
      observedAt,
      token: getToken(),
      sawRunning: false,
    };
    recoveries.set(childId, recovery);
    emit();
    setError("");
    try {
      const result = await api<{
        data: { outcome: string; child?: { observedAt?: number } };
      }>(`/api/master/tasks/${match[1]}/children/${match[2]}/recover`, {
        method: "POST",
        body: {},
        signal: AbortSignal.timeout(120000),
      });
      if (getToken() !== recovery.token) {
        clearRecovery(childId, recovery);
        return;
      }
      if (result.data.outcome === "WAITING_CAPACITY") {
        notify(t("节点并发已满，有空位后会自动恢复。"), "info");
      } else if (result.data.outcome === "STARTED") {
        notify(t("子任务已恢复。"), "success");
        recovery.sawRunning = true;
      } else if (result.data.outcome === "ALREADY_RUNNING") {
        notify(t("子任务已在执行。"), "info");
        recovery.sawRunning = true;
      } else {
        throw new Error(t("尚未确认恢复结果，请刷新子任务状态。"));
      }
      recovery.phase = "accepted";
      emit();
      observeRecovery(childId, recovery);
      if (mounted.current) onRefresh();
    } catch (cause) {
      clearRecovery(childId, recovery);
      if (getToken() !== recovery.token) return;
      if (mounted.current) {
        const message = cause instanceof Error ? cause.message : String(cause);
        const uncertain =
          !(cause instanceof ApiError) ||
          cause.status === 0 ||
          cause.status >= 500;
        setError(
          uncertain
            ? `${t("尚未确认恢复结果，请刷新子任务状态。")} ${message}`
            : message,
        );
        onRefresh();
      }
    }
  };

  return (
    <Stack alignItems="flex-start" gap={1.5} sx={{ mb: 3 }}>
      <Button
        variant="outlined"
        startIcon={
          phase ? (
            <CircularProgress size={16} color="inherit" />
          ) : (
            <PlayArrowRounded />
          )
        }
        disabled={!!phase}
        onClick={() => void recover()}
      >
        {phase === "submitting"
          ? t("正在检查恢复条件…")
          : phase
            ? t("处理中…")
            : t("立即恢复")}
      </Button>
      {error && <Alert severity="warning">{error}</Alert>}
    </Stack>
  );
}
