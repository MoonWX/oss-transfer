import { t, useI18n } from "../lib/i18n";
import { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  Box,
  Button,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Stack,
  Tooltip,
  Typography,
} from "@mui/material";
import { SpeedRounded } from "@mui/icons-material";
import { api, ApiError, useNotify } from "../lib/api";
import {
  formatTaskBandwidth,
  parseTaskBandwidth,
  validateTaskBandwidth,
} from "../lib/taskBandwidth";
import { TaskBandwidthField } from "./TaskBandwidthField";
import { TaskRequestLimitsFields } from "./TaskRequestLimitsFields";
import {
  emptyTaskRequestLimits,
  parseTaskRequestLimits,
  taskRequestLimitRows,
  validateTaskRequestLimits,
  type RequestLimits,
} from "../lib/taskRequestLimits";
type BandwidthStatus = {
  limitMbps: number | null;
  requestLimits?: RequestLimits | null;
  applied: boolean;
  sharedTaskCount: number;
  canEdit: boolean;
  nodes: {
    address: string;
    applied: boolean;
    message?: string;
  }[];
};
function readStatus(response: { data?: BandwidthStatus }): BandwidthStatus {
  const status = response.data;
  if (
    !status ||
    !("limitMbps" in status) ||
    typeof status.applied !== "boolean" ||
    typeof status.canEdit !== "boolean" ||
    !Array.isArray(status.nodes) ||
    !(status.sharedTaskCount >= 1)
  )
    throw new Error(t("限速设置响应无效，请重试。"));
  validateTaskBandwidth(status.limitMbps);
  validateTaskRequestLimits(status.requestLimits);
  return status;
}
export default function TaskBandwidth({
  taskId,
  active,
  refreshMs,
  completed = false,
}: {
  taskId: number;
  active: boolean;
  refreshMs: number;
  completed?: boolean;
}) {
  useI18n();
  const path = `/api/master/tasks/${taskId}/bandwidth-limit`;
  const notify = useNotify();
  const [status, setStatus] = useState<BandwidthStatus>();
  const [loading, setLoading] = useState(true);
  const [readError, setReadError] = useState("");
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState("");
  const [requestValues, setRequestValues] = useState(emptyTaskRequestLimits);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");
  const sequence = useRef(0);
  const readController = useRef<AbortController | null>(null);
  const reading = useRef(false);
  const load = useCallback(async () => {
    const current = ++sequence.current;
    readController.current?.abort();
    const controller = new AbortController();
    readController.current = controller;
    reading.current = true;
    setLoading(true);
    try {
      const next = readStatus(
        await api(path, {
          signal: AbortSignal.any([
            controller.signal,
            AbortSignal.timeout(120000),
          ]),
        }),
      );
      if (current === sequence.current) {
        setStatus(next);
        setReadError("");
        return next;
      }
    } catch (error) {
      if (
        current === sequence.current &&
        !(error instanceof DOMException && error.name === "AbortError")
      )
        setReadError(
          error instanceof Error ? error.message : t("读取失败，请重试。"),
        );
    } finally {
      if (current === sequence.current) {
        reading.current = false;
        setLoading(false);
      }
    }
  }, [path]);
  useEffect(() => {
    if (active) void load();
    else setOpen(false);
    return () => {
      ++sequence.current;
      readController.current?.abort();
      reading.current = false;
    };
  }, [active, load]);
  useEffect(() => {
    if (!active || open || refreshMs <= 0) return;
    const timer = window.setInterval(() => {
      if (!document.hidden && !reading.current) void load();
    }, refreshMs);
    return () => window.clearInterval(timer);
  }, [active, open, refreshMs, load]);
  const edit = () => {
    if (completed || !active || !status?.canEdit || readError) return;
    // Editing uses the last confirmed settings; an older background read must
    // not replace the user's input or a subsequent write.
    ++sequence.current;
    readController.current?.abort();
    reading.current = false;
    setLoading(false);
    setValue(status.limitMbps == null ? "" : String(status.limitMbps));
    setRequestValues({
      listQps: status.requestLimits?.listQps
        ? String(status.requestLimits.listQps)
        : "",
      headQps: status.requestLimits?.headQps
        ? String(status.requestLimits.headQps)
        : "",
      putQps: status.requestLimits?.putQps
        ? String(status.requestLimits.putQps)
        : "",
    });
    setSaveError("");
    setOpen(true);
  };
  async function save() {
    if (completed || !active || !status?.canEdit || saving) return;
    let limitMbps: number | null;
    let requestLimits: RequestLimits | null;
    try {
      limitMbps = parseTaskBandwidth(value);
      const parsed = parseTaskRequestLimits(
        requestValues,
        status.requestLimits?.scope || "CLUSTER",
      );
      // Preserve legacy per-node scope and its bandwidth constraint on edits.
      requestLimits =
        parsed ||
        (status.requestLimits?.bandwidthBitsPerSecond
          ? { scope: status.requestLimits.scope || "CLUSTER" }
          : null);
      if (requestLimits && status.requestLimits?.bandwidthBitsPerSecond)
        requestLimits.bandwidthBitsPerSecond =
          status.requestLimits.bandwidthBitsPerSecond;
    } catch (error) {
      setSaveError((error as Error).message);
      return;
    }
    ++sequence.current;
    readController.current?.abort();
    reading.current = false;
    setLoading(false);
    setSaving(true);
    setSaveError("");
    try {
      const next = readStatus(
        await api(path, {
          method: "PUT",
          body: { limitMbps, requestLimits },
          signal: AbortSignal.timeout(120000),
        }),
      );
      setStatus(next);
      if (next.applied) {
        notify(t("任务限速已更新。"));
        setOpen(false);
      }
    } catch (error) {
      if (error instanceof ApiError && error.response?.data) {
        try {
          setStatus(readStatus(error.response));
        } catch {
          /* Retain the last confirmed settings if the response is incomplete. */
        }
      }
      setSaveError(t("正在核对应用结果…"));
      const confirmed = await load();
      if (
        confirmed?.applied &&
        confirmed.limitMbps === limitMbps &&
        sameRequestLimits(confirmed.requestLimits, requestLimits)
      ) {
        setSaveError("");
        setOpen(false);
        notify(t("已核对：任务限速已生效。"));
      } else {
        setSaveError(
          t("应用结果未确认：{value1}", {
            value1:
              error instanceof Error
                ? error.message
                : t("请核对当前设置后重试。"),
          }),
        );
      }
    } finally {
      setSaving(false);
    }
  }
  const partial = status && !status.applied;
  const failures = status?.nodes.filter((node) => !node.applied) || [];
  const warning = (
    <Alert
      severity="warning"
      sx={{ mt: 1.5 }}
      action={
        !open && status?.canEdit && !completed ? (
          <Button color="inherit" size="small" onClick={edit}>
            {t("重试")}
          </Button>
        ) : undefined
      }
    >
      {t("部分节点未生效")}
      {failures.length > 0 && (
        <Stack spacing={0.5} sx={{ mt: 0.75 }}>
          {failures.map((node) => (
            <Typography
              key={node.address}
              variant="body2"
              sx={{ overflowWrap: "anywhere" }}
            >
              {node.address}：{node.message || t("未生效")}
            </Typography>
          ))}
        </Stack>
      )}
    </Alert>
  );
  return (
    <Box sx={{ mb: 3 }}>
      {!status && loading ? (
        <Stack direction="row" gap={1} alignItems="center" role="status">
          <CircularProgress size={16} />
          <Typography variant="body2" color="text.secondary">
            {t("读取限速设置…")}
          </Typography>
        </Stack>
      ) : status ? (
        <Stack direction="row" gap={1} alignItems="center" flexWrap="wrap">
          {status.limitMbps != null && (
            <Typography variant="body2">
              {t("带宽上限 {limit}", {
                limit: formatTaskBandwidth(status.limitMbps),
              })}
            </Typography>
          )}
          {taskRequestLimitRows(status.requestLimits || undefined).map(
            ([label, value]) => (
              <Typography key={label} variant="body2">
                {label} {value}
              </Typography>
            ),
          )}
          <Tooltip
            title={
              completed
                ? t("已完成任务不可调整限速")
                : !status.canEdit
                  ? t("当前任务不可调整限速")
                  : ""
            }
          >
            <span>
              <Button
                size="small"
                onClick={edit}
                disabled={
                  completed || !active || !status.canEdit || Boolean(readError)
                }
                startIcon={
                  status.limitMbps == null ? (
                    <SpeedRounded fontSize="small" />
                  ) : undefined
                }
              >
                {status.limitMbps == null ? t("限速") : t("调整")}
              </Button>
            </span>
          </Tooltip>
        </Stack>
      ) : null}
      {readError && (
        <Alert
          severity="warning"
          action={
            <Button
              color="inherit"
              size="small"
              disabled={loading}
              onClick={() => void load()}
              aria-label={t("重试读取任务限速")}
            >
              {t("重试")}
            </Button>
          }
        >
          {t("无法读取任务限速：{error}", { error: readError })}
        </Alert>
      )}
      {!open && partial && warning}
      <Dialog
        open={open && active}
        onClose={() => {
          if (!saving) setOpen(false);
        }}
        maxWidth="xs"
        fullWidth
        aria-labelledby="task-bandwidth-title"
      >
        <Box
          component="form"
          onSubmit={(event) => {
            event.preventDefault();
            void save();
          }}
        >
          <DialogTitle id="task-bandwidth-title">
            {t("任务 #{id} 限速", { id: taskId })}
          </DialogTitle>
          <DialogContent>
            <Box sx={{ mt: 1 }}>
              <TaskBandwidthField
                value={value}
                onChange={(next) => {
                  setValue(next);
                  setSaveError("");
                }}
                disabled={completed || saving || !status?.canEdit}
                autoFocus
              />
            </Box>
            <Typography
              variant="body2"
              color="text.secondary"
              sx={{ mt: 2, mb: 1.5 }}
            >
              {status?.requestLimits?.scope === "PER_NODE"
                ? t("每节点")
                : t("任务总计")}
            </Typography>
            <Stack spacing={2}>
              <TaskRequestLimitsFields
                value={requestValues}
                onChange={(next) => {
                  setRequestValues(next);
                  setSaveError("");
                }}
                disabled={completed || saving || !status?.canEdit}
              />
            </Stack>
            {(status?.sharedTaskCount || 0) > 1 && (
              <Typography
                variant="caption"
                color="text.secondary"
                sx={{ display: "block", mt: 1.5 }}
              >
                {t("{count} 个子任务共享此上限。", {
                  count: status!.sharedTaskCount,
                })}
              </Typography>
            )}
            {partial && warning}
            {saveError && (
              <Alert severity={saving ? "info" : "error"} sx={{ mt: 1.5 }}>
                {saveError}
              </Alert>
            )}
          </DialogContent>
          <DialogActions>
            <Button disabled={saving} onClick={() => setOpen(false)}>
              {t("取消")}
            </Button>
            <Button
              type="submit"
              variant="contained"
              disabled={completed || saving || !status?.canEdit}
              startIcon={
                saving ? (
                  <CircularProgress size={16} color="inherit" />
                ) : undefined
              }
            >
              {saving ? t("正在应用") : partial ? t("重试应用") : t("应用")}
            </Button>
          </DialogActions>
        </Box>
      </Dialog>
    </Box>
  );
}

function sameRequestLimits(
  left?: RequestLimits | null,
  right?: RequestLimits | null,
) {
  return (
    (left?.scope || "CLUSTER") === (right?.scope || "CLUSTER") &&
    (["listQps", "headQps", "putQps", "bandwidthBitsPerSecond"] as const).every(
      (key) => (left?.[key] || 0) === (right?.[key] || 0),
    )
  );
}
