import { useEffect, useRef, useState } from "react";
import {
  Alert,
  Box,
  Button,
  Checkbox,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  LinearProgress,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from "@mui/material";
import { RefreshRounded, ReplayRounded } from "@mui/icons-material";
import { ApiError } from "../lib/api";
import { t, useI18n } from "../lib/i18n";
import {
  catalogEndpoint,
  loadTaskCatalog,
  type TaskCatalogEntry,
} from "../lib/taskCatalog";
import {
  runTaskBatches,
  type BatchProgress,
  type BatchRunResult,
} from "../lib/taskBatches";
import { useTaskRecovery } from "../lib/taskRecovery";
import { formatNumber, StatusChip } from "./Common";
import ListPagination from "./ListPagination";

export default function FailedTaskRetryDialog({
  open,
  onClose,
  onSubmitted,
}: {
  open: boolean;
  onClose: () => void;
  onSubmitted: () => void;
}) {
  useI18n();
  const recovery = useTaskRecovery();
  const [tasks, setTasks] = useState<TaskCatalogEntry[]>([]);
  const [selected, setSelected] = useState<number[]>([]);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [revision, setRevision] = useState(0);
  const [progress, setProgress] = useState<BatchProgress | null>(null);
  const [result, setResult] = useState<BatchRunResult | null>(null);
  const [attempted, setAttempted] = useState<number[]>([]);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(25);
  const inFlight = useRef(false);
  const mounted = useRef(false);
  const opened = useRef(open);
  opened.current = open;
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  useEffect(() => {
    if (!open) return;
    const controller = new AbortController();
    setLoading(true);
    setTasks([]);
    setSelected([]);
    setAttempted([]);
    setError("");
    setResult(null);
    setProgress(null);
    setPage(0);
    void loadTaskCatalog(controller.signal)
      .then((items) => {
        if (!controller.signal.aborted)
          setTasks(
            items.filter((task) =>
              ["FAILED", "PARTIAL_FAILED"].includes(task.status),
            ),
          );
      })
      .catch((cause) => {
        if (!controller.signal.aborted)
          setError(cause instanceof Error ? cause.message : String(cause));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [open, revision]);
  const attemptedIds = new Set(attempted);
  const eligible = tasks.filter(
    (task) =>
      !recovery.states[task.taskIndex] &&
      !attemptedIds.has(task.taskIndex) &&
      Number.isSafeInteger(task.generation) &&
      Number(task.generation) > 0,
  );
  const eligibleIds = new Set(eligible.map((task) => task.taskIndex));
  const chosen = selected.filter((id) => eligibleIds.has(id));
  const selectedIds = new Set(chosen);
  const toggle = (id: number) =>
    setSelected((ids) =>
      ids.includes(id) ? ids.filter((value) => value !== id) : [...ids, id],
    );
  async function submit(ids: number[]) {
    if (
      loading ||
      error ||
      busy ||
      inFlight.current ||
      !ids.length ||
      ids.some((id) => !eligibleIds.has(id))
    )
      return;
    const requestedIds = new Set(ids);
    const snapshots = new Map(
      tasks
        .filter((task) => requestedIds.has(task.taskIndex))
        .map((task) => [task.taskIndex, task]),
    );
    if (!recovery.begin(ids)) return;
    inFlight.current = true;
    setBusy(true);
    setResult(null);
    setProgress(null);
    const settled = new Set<number>();
    const prepared = new Set<number>();
    const itemIssues: BatchRunResult["issues"] = [];
    const current = () =>
      mounted.current && opened.current && recovery.isCurrent();
    try {
      const outcome = await runTaskBatches(
        [{ path: "/api/master/retry", taskIds: ids }],
        {
          isCurrent: current,
          prepare: async (batch) => {
            batch.forEach((id) => prepared.add(id));
            return {
              expectedGenerations: Object.fromEntries(
                batch.map((id) => [id, snapshots.get(id)!.generation]),
              ),
            };
          },
          onProgress: setProgress,
          onResult: (detail, batch) => {
            const rows = new Map<number, any>(
              (detail.details ?? []).map((row: any) => [row.taskId, row]),
            );
            recovery.settle(
              batch.map((id) => {
                const row = rows.get(id);
                settled.add(id);
                if (row?.success === false || row?.error) {
                  itemIssues.push({
                    taskIds: [id],
                    message: row.message || row.error || t("请求失败"),
                  });
                  return { id, phase: "release" as const };
                }
                if (
                  row?.success === true ||
                  (!rows.size &&
                    !detail.partial &&
                    !detail.failed &&
                    Number(detail.accepted ?? 0) +
                      Number(detail.ignored ?? 0) ===
                      batch.length)
                )
                  return {
                    id,
                    phase: "accepted" as const,
                    generation:
                      row?.generation ??
                      Number(snapshots.get(id)!.generation) +
                        (row?.ignored || detail.ignored === batch.length
                          ? 0
                          : 1),
                  };
                return {
                  id,
                  phase: "checking" as const,
                  generation: snapshots.get(id)!.generation,
                };
              }),
            );
          },
          onError: (cause, batch, submitted) => {
            const definitive =
              !submitted ||
              (cause instanceof ApiError &&
                cause.status >= 400 &&
                cause.status < 500 &&
                cause.status !== 408);
            recovery.settle(
              batch.map((id) => {
                settled.add(id);
                return {
                  id,
                  phase: definitive
                    ? ("release" as const)
                    : ("checking" as const),
                  generation: snapshots.get(id)!.generation,
                };
              }),
            );
          },
        },
      );
      // A navigation can happen while a request is in flight. Settle its guard
      // even when the dialog no longer owns a visible result, and release only
      // requests that were never sent.
      const unsent = new Set(outcome.unsubmittedIds);
      recovery.settle(
        ids
          .filter((id) => !settled.has(id))
          .map((id) => ({
            id,
            phase: unsent.has(id)
              ? ("release" as const)
              : ("checking" as const),
            generation: snapshots.get(id)!.generation,
          })),
      );
      if (!current()) return;
      outcome.issues.push(...itemIssues);
      setResult(outcome);
      setAttempted((previous) => [
        ...new Set([...previous, ...ids.filter((id) => !unsent.has(id))]),
      ]);
      setSelected(outcome.failedIds.concat(outcome.unsubmittedIds));
      onSubmitted();
    } catch (cause) {
      recovery.settle(
        ids
          .filter((id) => !settled.has(id))
          .map((id) => ({
            id,
            phase: prepared.has(id)
              ? ("checking" as const)
              : ("release" as const),
            generation: snapshots.get(id)!.generation,
          })),
      );
      if (current())
        setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      inFlight.current = false;
      if (mounted.current) setBusy(false);
    }
  }
  const shown = tasks.slice(page * pageSize, (page + 1) * pageSize);
  return (
    <Dialog
      open={open}
      onClose={() => !busy && onClose()}
      maxWidth="md"
      fullWidth
    >
      <DialogTitle>{t("批量重试失败任务")}</DialogTitle>
      <DialogContent>
        <Stack spacing={2}>
          <Typography variant="body2" color="text.secondary">
            {t("所有失败和部分失败的任务，不受异常记录筛选条件影响。")}
          </Typography>
          <Typography variant="body2" color="text.secondary">
            {t("重试失败和未完成的对象，保留已完成的传输进度。")}
          </Typography>
          <Box>
            <Button
              size="small"
              startIcon={<RefreshRounded />}
              disabled={loading || busy}
              onClick={() => setRevision((value) => value + 1)}
            >
              {t("刷新")}
            </Button>
          </Box>
          {loading && (
            <Stack direction="row" spacing={1} alignItems="center">
              <CircularProgress size={18} />
              <Typography>{t("正在读取全部任务…")}</Typography>
            </Stack>
          )}
          {error && <Alert severity="error">{error}</Alert>}
          {result && (
            <Alert
              severity={
                result.progress.failed ||
                result.progress.uncertain ||
                result.progress.unsubmitted
                  ? "warning"
                  : "success"
              }
            >
              <Typography variant="body2">
                {t(
                  "已受理 {accepted}，忽略 {ignored}，失败 {failed}",
                  result.progress,
                )}
              </Typography>
              {!!(result.progress.uncertain || result.progress.unsubmitted) && (
                <Typography variant="body2">
                  {t(
                    "待确认 {uncertain}，未提交 {unsubmitted}",
                    result.progress,
                  )}
                </Typography>
              )}
              {result.issues.map((issue, i) => (
                <Typography variant="body2" key={i}>
                  {issue.taskIds.map((id) => `#${id}`).join(", ")}:{" "}
                  {issue.message}
                </Typography>
              ))}
            </Alert>
          )}
          {busy && progress && (
            <Stack spacing={1} aria-label={t("批量操作进度")}>
              <LinearProgress
                variant="determinate"
                value={
                  progress.total
                    ? (progress.processed / progress.total) * 100
                    : 0
                }
              />
              <Typography variant="body2">
                {t("已处理 {processed} / {total}", progress)}
              </Typography>
            </Stack>
          )}
          {!loading && !error && !tasks.length && (
            <Typography>{t("没有需要重试的失败任务。")}</Typography>
          )}
          {!!tasks.length && (
            <>
              <TableContainer sx={{ maxHeight: 380 }}>
                <Table size="small" stickyHeader aria-label={t("失败任务")}>
                  <TableHead>
                    <TableRow>
                      <TableCell padding="checkbox">
                        <Checkbox
                          inputProps={{ "aria-label": t("选择全部失败任务") }}
                          checked={
                            eligible.length > 0 &&
                            chosen.length === eligible.length
                          }
                          indeterminate={
                            chosen.length > 0 && chosen.length < eligible.length
                          }
                          disabled={
                            busy || loading || !!error || !eligible.length
                          }
                          onChange={(_, checked) =>
                            setSelected(
                              checked
                                ? eligible.map((task) => task.taskIndex)
                                : [],
                            )
                          }
                        />
                      </TableCell>
                      <TableCell>{t("任务")}</TableCell>
                      <TableCell>{t("来源 / 目标")}</TableCell>
                      <TableCell>{t("状态")}</TableCell>
                      <TableCell align="right">{t("失败对象")}</TableCell>
                    </TableRow>
                  </TableHead>
                  <TableBody>
                    {shown.map((task) => (
                      <TableRow
                        key={task.taskIndex}
                        selected={selectedIds.has(task.taskIndex)}
                      >
                        <TableCell padding="checkbox">
                          <Checkbox
                            inputProps={{
                              "aria-label": t("选择任务 #{id}", {
                                id: task.taskIndex,
                              }),
                            }}
                            checked={selectedIds.has(task.taskIndex)}
                            disabled={busy || !eligibleIds.has(task.taskIndex)}
                            onChange={() => toggle(task.taskIndex)}
                          />
                        </TableCell>
                        <TableCell sx={{ whiteSpace: "nowrap" }}>
                          #{task.taskIndex}
                        </TableCell>
                        <TableCell
                          sx={{ maxWidth: 340, overflowWrap: "anywhere" }}
                        >
                          {catalogEndpoint(task, true)}
                          <Typography variant="body2" color="text.secondary">
                            → {catalogEndpoint(task, false)}
                          </Typography>
                        </TableCell>
                        <TableCell>
                          {recovery.states[task.taskIndex] ||
                          attemptedIds.has(task.taskIndex) ? (
                            <Typography variant="body2">
                              {result?.failedIds.includes(task.taskIndex)
                                ? t("任务提交失败")
                                : recovery.states[task.taskIndex]?.phase ===
                                    "checking"
                                  ? t("结果待确认")
                                  : t("已提交")}
                            </Typography>
                          ) : !Number.isSafeInteger(task.generation) ||
                            Number(task.generation) <= 0 ? (
                            <Typography variant="body2">
                              {t("状态暂不可用")}
                            </Typography>
                          ) : (
                            <StatusChip status={task.status} />
                          )}
                        </TableCell>
                        <TableCell align="right">
                          {formatNumber(task.failedObjectCount)}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </TableContainer>
              <ListPagination
                ariaLabel={t("失败任务分页")}
                count={tasks.length}
                page={page}
                pageSize={pageSize}
                onPageChange={setPage}
                onPageSizeChange={(size) => {
                  setPageSize(size);
                  setPage(0);
                }}
              />
            </>
          )}
        </Stack>
      </DialogContent>
      <DialogActions sx={{ px: 3, pb: 2, flexWrap: "wrap", gap: 1 }}>
        <Button disabled={busy} onClick={onClose}>
          {t("关闭")}
        </Button>
        <Button
          disabled={loading || busy || !!error || !eligible.length}
          onClick={() => void submit(eligible.map((task) => task.taskIndex))}
        >
          {t("重试全部（{count}）", { count: formatNumber(eligible.length) })}
        </Button>
        <Button
          variant="contained"
          startIcon={
            busy ? (
              <CircularProgress size={16} color="inherit" />
            ) : (
              <ReplayRounded />
            )
          }
          disabled={loading || busy || !!error || !chosen.length}
          onClick={() => void submit(chosen)}
        >
          {busy
            ? t("提交中…")
            : t("重试所选（{count}）", { count: formatNumber(chosen.length) })}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
