import { t, useI18n, formatDate } from "../lib/i18n";
import { useNodeLabels } from "../lib/workerStatus";
import { taskNodeLabels, type NodeDisplay } from "../lib/nodeRegistry";
import { useSearchParams } from "react-router-dom";
import {
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import {
  Alert,
  Box,
  Button,
  Checkbox,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Divider,
  LinearProgress,
  Paper,
  Stack,
  Tab,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Tabs,
  TextField,
  Typography,
} from "@mui/material";
import {
  CompareArrowsRounded,
  DeleteForeverRounded,
  RefreshRounded,
  SystemUpdateAltRounded,
} from "@mui/icons-material";
import { api, getToken, useApi, useNotify } from "../lib/api";
import { runTaskBatches, type BatchProgress } from "../lib/taskBatches";
import ListPagination, {
  rememberPageSize,
  savedPageSize,
} from "../components/ListPagination";
import ObjectComparison from "../components/ObjectComparison";
import SoftwareUpdates from "../components/SoftwareUpdates";
import type { NodeRegistry } from "../lib/nodeRegistry";
import {
  EmptyState,
  PageHeader,
  StatusChip,
  formatBytes,
  formatNumber,
} from "../components/Common";
type CleanupProgress = {
  status: "QUEUED" | "RUNNING" | "WAITING" | "DONE";
  phase: "AGENTS" | "MASTER";
  recordsDeleted: number;
  completedNodes: number;
  totalNodes: number;
  updatedAt?: string;
  error?: string;
};
type DeletedTask = {
  cleanup?: CleanupProgress;
  taskIndex: number;
  sourceBucket?: string;
  destBucket?: string;
  direction?: string;
  status?: string;
  createTime?: number;
  putObjectCount?: number;
  putObjectSize?: number;
  workerNode?: string;
  localPath?: string;
  executionNodeIp?: string;
  participatingWorkerNodes?: string[];
  sourceEndpoint?: {
    type?: string;
    localPath?: string;
    bucket?: string;
    s3Alias?: string;
    nodeBindings?: {
      localPath?: string;
    }[];
  };
  destinationEndpoint?: DeletedTask["sourceEndpoint"];
};
function deletedEndpoint(task: DeletedTask, source: boolean) {
  const value = source ? task.sourceEndpoint : task.destinationEndpoint;
  const local =
    value?.type === "LOCAL" ||
    (source && task.direction === "LOCAL_TO_OSS") ||
    (!source && task.direction === "OSS_TO_LOCAL");
  if (local)
    return (
      value?.localPath ||
      value?.nodeBindings?.[0]?.localPath ||
      task.localPath ||
      t("本地文件系统")
    );
  return (
    [
      value?.s3Alias,
      value?.bucket || (source ? task.sourceBucket : task.destBucket),
    ]
      .filter(Boolean)
      .join("/") || "—"
  );
}
type RecycleRow = DeletedTask & { workerNode: string };
type CleanupUpdate = {
  row: RecycleRow;
  cleanup: CleanupProgress;
  purged?: boolean;
};
function CleanupStatus({ task }: { task: RecycleRow }) {
  const cleanup = task.cleanup!;
  const waiting = cleanup.status === "WAITING";
  return (
    <Stack
      spacing={0.5}
      aria-label={t("任务 {id} 清理进度", { id: task.taskIndex })}
    >
      <Typography
        variant="body2"
        color={waiting ? "warning.main" : "primary.main"}
      >
        {cleanup.status === "DONE"
          ? t("已清理")
          : cleanup.status === "QUEUED"
            ? t("等待清理")
            : waiting
              ? t("等待自动继续")
              : cleanup.phase === "MASTER"
                ? t("清理主节点记录")
                : t("清理执行节点记录")}
      </Typography>
      <Typography variant="caption" color="text.secondary">
        {t("已清理 {count} 条记录", {
          count: formatNumber(cleanup.recordsDeleted),
        })}
        {cleanup.totalNodes > 0 &&
          ` · ${t("节点 {done}/{total}", { done: cleanup.completedNodes, total: cleanup.totalNodes })}`}
      </Typography>
      {cleanup.error && (
        <Typography
          variant="caption"
          color="warning.main"
          sx={{ maxWidth: 320, overflowWrap: "anywhere" }}
        >
          {cleanup.error}
        </Typography>
      )}
    </Stack>
  );
}
const errorText = (error: unknown) =>
  error instanceof Error ? error.message : String(error);
// A confirmed operation keeps running across SPA navigation. Share the guard
// across page instances so navigating back cannot submit the same batch again.
let deletingSession: string | null = null;
const deletionListeners = new Set<() => void>();
function subscribeDeletion(listener: () => void) {
  deletionListeners.add(listener);
  return () => {
    deletionListeners.delete(listener);
  };
}
function setDeletingSession(token: string | null) {
  deletingSession = token;
  deletionListeners.forEach((listener) => listener());
}
export default function ToolsPage() {
  useI18n();
  const nodeLabel = useNodeLabels();
  const [params, setParams] = useSearchParams();
  const tab =
    params.get("tab") === "recycle"
      ? 1
      : params.get("tab") === "updates"
        ? 2
        : 0;
  const notify = useNotify();
  const recycle = useApi<{
    deletedTasks: Record<string, DeletedTask[] | number>;
    totalCount: number;
    complete?: boolean;
    partial?: boolean;
    failedWorkers?: string[];
    unreachableWorkers?: string[];
    queriedWorkers?: string[];
    truncated?: boolean;
    nextBeforeId?: number;
    nodeDisplays?: Record<string, NodeDisplay>;
  }>(tab === 1 ? "/api/master/deleted-tasks" : null);
  const [earlierRows, setEarlierRows] = useState<
    (DeletedTask & {
      workerNode: string;
    })[]
  >([]);
  const [earlierCursor, setEarlierCursor] = useState<number | null>();
  const [earlierLoading, setEarlierLoading] = useState(false);
  const [earlierError, setEarlierError] = useState<string | null>(null);
  const earlierRequest = useRef<AbortController | null>(null);
  useEffect(() => () => earlierRequest.current?.abort(), []);
  const nextEarlierCursor =
    earlierCursor === undefined ? recycle.data?.nextBeforeId : earlierCursor;
  async function loadEarlierRecords() {
    if (!nextEarlierCursor || earlierRequest.current) return;
    const controller = new AbortController();
    earlierRequest.current = controller;
    setEarlierLoading(true);
    setEarlierError(null);
    try {
      const value = await api<{
        deletedTasks: Record<string, DeletedTask[] | number>;
        nextBeforeId?: number;
      }>(`/api/master/deleted-tasks?beforeId=${nextEarlierCursor}&limit=1000`, {
        signal: controller.signal,
      });
      const items = Object.entries(value.deletedTasks).flatMap(
        ([worker, tasks]) =>
          Array.isArray(tasks)
            ? tasks.map((task) => ({ ...task, workerNode: worker }))
            : [],
      );
      setEarlierRows((previous) => [...previous, ...items]);
      setEarlierCursor(value.nextBeforeId ?? null);
    } catch (cause) {
      if (!controller.signal.aborted) setEarlierError(errorText(cause));
    } finally {
      earlierRequest.current = null;
      if (!controller.signal.aborted) setEarlierLoading(false);
    }
  }
  // Recycle completeness belongs to this snapshot, not the live status poll.
  const nodes = useApi<{
    data: NodeRegistry;
  }>(tab === 1 ? "/api/master/nodes" : null);
  const hasMembershipSnapshot = Array.isArray(recycle.data?.queriedWorkers);
  const nodeDiscoveryFailed = !hasMembershipSnapshot && !!nodes.error;
  const membershipKnown = hasMembershipSnapshot || !!nodes.data?.data?.nodes;
  const [selectedIds, setSelectedIds] = useState<number[]>([]);
  const [cleanupUpdates, setCleanupUpdates] = useState<
    Record<number, CleanupUpdate>
  >({});
  const [cleanupPollError, setCleanupPollError] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const deletionSession = useSyncExternalStore(
    subscribeDeletion,
    () => deletingSession,
  );
  const deleting = deletionSession !== null && deletionSession === getToken();
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [backgroundCleanup, setBackgroundCleanup] = useState(false);
  const [deleteProgress, setDeleteProgress] = useState<BatchProgress | null>(
    null,
  );
  const [confirmedIds, setConfirmedIds] = useState<number[]>([]);
  const [showDeleteDetails, setShowDeleteDetails] = useState(false);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(() =>
    savedPageSize("oss-transfer-recycle-page-size", 25),
  );
  const snapshotRows = useMemo(
    () =>
      [
        ...new Map(
          [
            ...earlierRows,
            ...Object.entries(recycle.data?.deletedTasks ?? {}).flatMap(
              ([worker, tasks]) =>
                Array.isArray(tasks)
                  ? tasks.map((task) => ({ ...task, workerNode: worker }))
                  : [],
            ),
          ].map((task) => [`${task.taskIndex}:${task.workerNode}`, task]),
        ).values(),
      ].sort(
        (left, right) =>
          right.taskIndex - left.taskIndex ||
          left.workerNode.localeCompare(right.workerNode),
      ),
    [recycle.data, earlierRows],
  );
  // Keep admitted records visible until the server confirms cleanup completion,
  // including records loaded from an earlier history page.
  useEffect(() => {
    const pending = snapshotRows.filter((row) => row.cleanup);
    if (!pending.length) return;
    setCleanupUpdates((previous) => {
      let next = previous;
      for (const row of pending) {
        if (previous[row.taskIndex]) continue;
        if (next === previous) next = { ...previous };
        next[row.taskIndex] = { row, cleanup: row.cleanup! };
      }
      return next;
    });
  }, [snapshotRows]);
  const rows = useMemo(() => {
    const items = new Map(
      [
        ...Object.values(cleanupUpdates).map((item) => item.row),
        ...snapshotRows,
      ].map((row) => [`${row.taskIndex}:${row.workerNode}`, row]),
    );
    return [...items.values()]
      .filter((row) => !cleanupUpdates[row.taskIndex]?.purged)
      .map((row) =>
        cleanupUpdates[row.taskIndex]
          ? { ...row, cleanup: cleanupUpdates[row.taskIndex].cleanup }
          : row,
      )
      .sort(
        (left, right) =>
          right.taskIndex - left.taskIndex ||
          left.workerNode.localeCompare(right.workerNode),
      );
  }, [snapshotRows, cleanupUpdates]);
  const cleanupIDs = [
    ...new Set(rows.filter((row) => row.cleanup).map((row) => row.taskIndex)),
  ]
    .sort((a, b) => a - b)
    .join(",");
  useEffect(() => {
    if (tab !== 1 || !cleanupIDs) return;
    const ids = cleanupIDs.split(",").map(Number);
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      if (document.hidden) {
        timer = setTimeout(() => void poll(), 1000);
        return;
      }
      try {
        let completed = 0;
        for (let start = 0; start < ids.length; start += 128) {
          const value = await api<{
            tasks: {
              taskId: number;
              cleanup?: CleanupProgress;
              purged: boolean;
              missing?: boolean;
            }[];
          }>("/api/master/cleanup-status", {
            method: "POST",
            body: { taskIds: ids.slice(start, start + 128) },
            signal: controller.signal,
            timeoutMs: 15000,
          });
          if (controller.signal.aborted) return;
          if (
            !Array.isArray(value.tasks) ||
            value.tasks.some((item) => item.missing)
          )
            throw new Error("Cleanup status unavailable");
          completed += value.tasks.filter((item) => item.purged).length;
          setCleanupUpdates((previous) => {
            const next = { ...previous };
            for (const item of value.tasks) {
              const known = previous[item.taskId];
              if (!known) continue;
              next[item.taskId] = {
                ...known,
                cleanup: item.cleanup ?? known.cleanup,
                purged: item.purged,
              };
            }
            return next;
          });
        }
        setCleanupPollError(false);
        if (completed) {
          notify(t("已完成 {count} 个任务的清理。", { count: completed }));
          void recycle.refresh();
        }
      } catch {
        if (!controller.signal.aborted) setCleanupPollError(true);
      } finally {
        if (!controller.signal.aborted)
          timer = setTimeout(() => void poll(), 1000);
      }
    };
    timer = setTimeout(() => void poll(), 1000);
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [tab, cleanupIDs, notify, recycle.refresh]);
  useEffect(() => {
    const lastPage = Math.max(0, Math.ceil(rows.length / pageSize) - 1);
    if (page > lastPage) setPage(lastPage);
  }, [rows.length, page, pageSize]);
  const uniqueIds = [...new Set(rows.map((task) => task.taskIndex))];
  const cleaningIds = new Set(
    rows.filter((task) => task.cleanup).map((task) => task.taskIndex),
  );
  const availableIds = new Set(uniqueIds.filter((id) => !cleaningIds.has(id)));
  const selectableIds = uniqueIds.filter((id) => availableIds.has(id));
  const activeSelection = selectedIds.filter((id) => availableIds.has(id));
  const selectedSet = new Set(activeSelection);
  const missingNodes = [
    ...new Set([
      ...(
        recycle.data?.queriedWorkers ??
        nodes.data?.data?.nodes.map((node) => node.address) ??
        []
      ).filter((ip) => !Array.isArray(recycle.data?.deletedTasks?.[ip])),
      ...(recycle.data?.failedWorkers || []),
      ...(recycle.data?.unreachableWorkers || []),
    ]),
  ];
  async function deleteTasks() {
    const token = getToken();
    if (!token || deletingSession === token || !confirmedIds.length) return;
    const taskIds = confirmedIds.filter((id) => availableIds.has(id));
    if (!taskIds.length) {
      setConfirmDelete(false);
      return;
    }
    setDeletingSession(token);
    const isCurrent = () => token === getToken();
    const canUpdateView = () => mounted.current && isCurrent();
    setDeleteError(null);
    setBackgroundCleanup(false);
    setShowDeleteDetails(false);
    const issues: string[] = [];
    const retained = new Set<string>();
    const queued = new Set<number>();
    try {
      const result = await runTaskBatches(
        [{ path: "/api/master/permanent-delete", taskIds }],
        {
          isCurrent,
          onProgress: (progress) => {
            if (canUpdateView()) setDeleteProgress(progress);
          },
          onResult: (detail, ids) => {
            const entries = Array.isArray(detail.details) ? detail.details : [];
            for (const item of entries) {
              if (item.success === false || item.error)
                issues.push(
                  `#${item.taskId}: ${item.message || item.error || t("请求失败")}`,
                );
              for (const id of item.retainedAgentHistory ?? [])
                retained.add(
                  nodeLabel(String(id), detail.nodeDisplays?.[String(id)]),
                );
            }
            const unavailable = [
              ...new Set<string>([
                ...(detail.failedWorkers || []),
                ...(detail.unreachableWorkers || []),
              ]),
            ];
            if (unavailable.length)
              issues.push(
                t("（未完成节点：{value1}）", {
                  value1: unavailable
                    .map((id) => nodeLabel(id, detail.nodeDisplays?.[id]))
                    .join(", "),
                }),
              );
            if (detail.message && (detail.partial || Number(detail.failed) > 0))
              issues.push(detail.message);
            // New servers acknowledge admission; older servers delete synchronously.
            const admitted = entries.filter(
              (item: any) =>
                item.success &&
                item.cleanup &&
                !item.purged &&
                item.cleanup.status !== "DONE",
            );
            admitted.forEach((item: any) => queued.add(item.taskId));
            if (canUpdateView()) {
              if (admitted.length) setBackgroundCleanup(true);
              setCleanupUpdates((previous) => {
                const next = { ...previous };
                for (const item of admitted) {
                  const row = rows.find(
                    (candidate) => candidate.taskIndex === item.taskId,
                  );
                  if (row) next[item.taskId] = { row, cleanup: item.cleanup };
                }
                return next;
              });
            }
            const removed = new Set(
              entries
                .filter(
                  (item: any) =>
                    item.success &&
                    (!item.cleanup ||
                      item.purged ||
                      item.cleanup.status === "DONE"),
                )
                .map((item: any) => item.taskId),
            );
            if (
              !entries.length &&
              !detail.partial &&
              !detail.failed &&
              !detail.failedWorkers?.length &&
              !detail.unreachableWorkers?.length &&
              Number(detail.accepted) === ids.length
            )
              ids.forEach((id) => removed.add(id));
            if (canUpdateView()) {
              setSelectedIds((previous) =>
                previous.filter((id) => !removed.has(id) && !queued.has(id)),
              );
              setEarlierRows((previous) =>
                previous.filter((task) => !removed.has(task.taskIndex)),
              );
            }
          },
        },
      );
      if (!isCurrent()) return;
      const successful = new Set(result.successfulIds);
      if (canUpdateView()) {
        setSelectedIds((previous) =>
          previous.filter((id) => !successful.has(id)),
        );
        setEarlierRows((previous) =>
          previous.filter(
            (task) =>
              !successful.has(task.taskIndex) || queued.has(task.taskIndex),
          ),
        );
      }
      for (const issue of result.issues)
        issues.push(
          `${issue.taskIds
            .slice(0, 5)
            .map((id) => `#${id}`)
            .join(
              ", ",
            )}${issue.taskIds.length > 5 ? "…" : ""}: ${issue.message}`,
        );
      const partial =
        result.failedIds.length > 0 ||
        result.uncertainIds.length > 0 ||
        result.unsubmittedIds.length > 0 ||
        issues.length > 0;
      if (partial) {
        if (result.uncertainIds.length || result.unsubmittedIds.length)
          issues.push(
            t("待确认 {uncertain}，未提交 {unsubmitted}", {
              uncertain: result.uncertainIds.length,
              unsubmitted: result.unsubmittedIds.length,
            }),
          );
        if (canUpdateView())
          setDeleteError(
            issues.join("\n") || t("部分记录未删除，请刷新后重试。"),
          );
      }
      if (canUpdateView()) setConfirmDelete(false);
      notify(
        partial
          ? t("部分记录未删除，请刷新后重试。")
          : queued.size
            ? retained.size
              ? t("已受理清理；已移除节点的本地历史保留：{value1}。", {
                  value1: [...retained].join(", "),
                })
              : t("已开始后台清理，可离开此页面。")
            : retained.size
              ? t("任务记录已删除；已移除节点的本地历史保留：{value1}。", {
                  value1: [...retained].join(", "),
                })
              : t("永久删除请求已提交，正在刷新回收站。"),
        partial ? "warning" : "info",
      );
      if (canUpdateView())
        void Promise.allSettled([recycle.refresh(), nodes.refresh()]);
    } catch (error) {
      if (isCurrent()) {
        notify(errorText(error), "error");
        if (canUpdateView()) {
          setDeleteError(errorText(error));
          setConfirmDelete(false);
          void recycle.refresh();
        }
      }
    } finally {
      if (deletingSession === token) setDeletingSession(null);
    }
  }
  const deletionProgress = deleteProgress && (
    <Stack spacing={1} role="status" aria-live="polite">
      <Typography variant="body2">
        {t("已处理 {done} / {total}", {
          done: deleteProgress.processed,
          total: deleteProgress.total,
        })}
      </Typography>
      <LinearProgress
        aria-label={t("批量删除进度")}
        variant="determinate"
        value={
          deleteProgress.total
            ? (deleteProgress.processed / deleteProgress.total) * 100
            : 0
        }
      />
      <Typography variant="body2" color="text.secondary">
        {t("已受理 {accepted}，忽略 {ignored}，失败 {failed}", deleteProgress)}
        {!deleting &&
        (deleteProgress.uncertain > 0 || deleteProgress.unsubmitted > 0)
          ? ` · ${t("待确认 {uncertain}，未提交 {unsubmitted}", deleteProgress)}`
          : ""}
      </Typography>
    </Stack>
  );
  return (
    <Stack spacing={3}>
      <PageHeader
        title={t("运维工具")}
        description={t("比对存储桶或对象，管理回收站和软件更新。")}
      />
      <Tabs
        value={tab}
        variant="scrollable"
        scrollButtons="auto"
        allowScrollButtonsMobile
        onChange={(_, value) => {
          const next = new URLSearchParams(params);
          if (value === 1) next.set("tab", "recycle");
          else if (value === 2) next.set("tab", "updates");
          else next.delete("tab");
          setParams(next, { replace: true });
        }}
        aria-label={t("运维工具")}
        sx={{ borderBottom: 1, borderColor: "divider" }}
      >
        <Tab
          icon={<CompareArrowsRounded fontSize="small" />}
          iconPosition="start"
          label={t("对象比对")}
        />
        <Tab
          icon={<DeleteForeverRounded fontSize="small" />}
          iconPosition="start"
          label={t("任务回收站")}
        />
        <Tab
          icon={<SystemUpdateAltRounded fontSize="small" />}
          iconPosition="start"
          label={t("版本更新")}
        />
      </Tabs>
      {tab === 0 && <ObjectComparison />}
      {tab === 2 && <SoftwareUpdates />}
      {tab === 1 && (
        <>
          <Alert severity="info">
            {t(
              "回收站保留已标记删除的任务。永久删除仅清理任务记录，无法恢复；不删除源端或目标端的数据对象。",
            )}
          </Alert>
          {recycle.error && <Alert severity="error">{recycle.error}</Alert>}
          {cleanupPollError && (
            <Alert severity="warning">
              {t("暂时无法获取清理进度，恢复连接后自动刷新。")}
            </Alert>
          )}
          {!deleting &&
            deleteProgress &&
            (!backgroundCleanup || !!deleteError) && (
              <Alert severity={deleteError ? "warning" : "success"}>
                {deletionProgress}
                {deleteError && (
                  <>
                    <Button
                      size="small"
                      onClick={() => setShowDeleteDetails((value) => !value)}
                    >
                      {showDeleteDetails
                        ? t("收起失败详情")
                        : t("查看失败详情")}
                    </Button>
                    {showDeleteDetails && (
                      <Typography
                        component="pre"
                        variant="body2"
                        sx={{
                          whiteSpace: "pre-wrap",
                          overflowWrap: "anywhere",
                          maxHeight: 240,
                          overflow: "auto",
                        }}
                      >
                        {deleteError}
                      </Typography>
                    )}
                  </>
                )}
              </Alert>
            )}
          {deleteError && !deleteProgress && (
            <Alert severity="warning">{deleteError}</Alert>
          )}
          {nodeDiscoveryFailed && (
            <Alert severity="warning">
              {t("节点清单读取失败，无法确认回收站是否覆盖全部节点。")}
            </Alert>
          )}
          {recycle.data && missingNodes.length > 0 && (
            <Alert severity="warning">
              {t("以下节点未返回回收站数据，当前列表可能不完整：{nodes}", {
                nodes: missingNodes
                  .map((id) => nodeLabel(id, recycle.data?.nodeDisplays?.[id]))
                  .join(", "),
              })}
            </Alert>
          )}
          {(recycle.data?.partial || recycle.data?.complete === false) &&
            !recycle.data?.truncated &&
            missingNodes.length === 0 && (
              <Alert severity="warning">
                {t("部分回收站记录未返回，当前列表可能不完整。")}
              </Alert>
            )}
          {nextEarlierCursor && (
            <Alert
              severity="info"
              action={
                <Button
                  disabled={earlierLoading}
                  onClick={() => void loadEarlierRecords()}
                >
                  {earlierLoading ? t("加载中") : t("加载更早记录")}
                </Button>
              }
            >
              {t("已加载 {count} 条记录。", {
                count: formatNumber(rows.length),
              })}
            </Alert>
          )}
          {earlierError && <Alert severity="error">{earlierError}</Alert>}
          <Paper variant="outlined" sx={{ overflow: "hidden" }}>
            <Stack
              direction={{ xs: "column", sm: "row" }}
              spacing={2}
              justifyContent="space-between"
              alignItems={{ sm: "center" }}
              sx={{ p: 2.5 }}
            >
              <Box>
                <Typography variant="h6">{t("已删除任务")}</Typography>
                <Typography variant="body2" color="text.secondary">
                  {recycle.data
                    ? t("{value1} 条节点任务记录{value2}", {
                        value1: formatNumber(rows.length),
                        value2: activeSelection.length
                          ? t(" · 已选择 {value1} 个任务 ID", {
                              value1: activeSelection.length,
                            })
                          : "",
                      })
                    : t("正在读取回收站")}
                </Typography>
              </Box>
              <Stack direction="row" spacing={1}>
                <Button
                  startIcon={<RefreshRounded />}
                  disabled={recycle.loading || deleting}
                  onClick={() => {
                    earlierRequest.current?.abort();
                    earlierRequest.current = null;
                    setEarlierLoading(false);
                    setEarlierRows([]);
                    setEarlierCursor(undefined);
                    setEarlierError(null);
                    void recycle.refresh();
                    void nodes.refresh();
                  }}
                >
                  {t("刷新")}
                </Button>
                <Button
                  variant="outlined"
                  color="error"
                  startIcon={<DeleteForeverRounded />}
                  disabled={
                    !activeSelection.length || deleting || recycle.loading
                  }
                  onClick={() => {
                    setConfirmedIds([...activeSelection]);
                    setConfirmDelete(true);
                  }}
                >
                  {t("永久删除")}
                </Button>
              </Stack>
            </Stack>
            <Box sx={{ height: 4 }}>
              <LinearProgress
                aria-label={t("刷新回收站")}
                sx={{ visibility: recycle.loading ? "visible" : "hidden" }}
              />
            </Box>
            {rows.length > 0 ? (
              <>
                <TableContainer>
                  <Table size="small" sx={{ minWidth: 700 }}>
                    <TableHead>
                      <TableRow>
                        <TableCell padding="checkbox">
                          <Checkbox
                            inputProps={{
                              "aria-label": t("选择全部已删除任务"),
                            }}
                            disabled={deleting || !selectableIds.length}
                            checked={
                              selectableIds.length > 0 &&
                              selectableIds.every((id) => selectedSet.has(id))
                            }
                            indeterminate={
                              activeSelection.length > 0 &&
                              !selectableIds.every((id) => selectedSet.has(id))
                            }
                            onChange={(event) =>
                              setSelectedIds(
                                event.target.checked ? selectableIds : [],
                              )
                            }
                          />
                        </TableCell>
                        <TableCell>{t("任务")}</TableCell>
                        <TableCell>{t("迁移路径")}</TableCell>
                        <TableCell>{t("节点")}</TableCell>
                        <TableCell>{t("状态")}</TableCell>
                        <TableCell align="right">{t("已传输")}</TableCell>
                      </TableRow>
                    </TableHead>
                    <TableBody>
                      {rows
                        .slice(page * pageSize, page * pageSize + pageSize)
                        .map((task) => (
                          <TableRow
                            hover
                            selected={selectedSet.has(task.taskIndex)}
                            key={`${task.workerNode}-${task.taskIndex}`}
                          >
                            <TableCell padding="checkbox">
                              <Checkbox
                                inputProps={{
                                  "aria-label": t("选择任务 {value1}", {
                                    value1: task.taskIndex,
                                  }),
                                }}
                                checked={selectedSet.has(task.taskIndex)}
                                disabled={
                                  deleting || cleaningIds.has(task.taskIndex)
                                }
                                onChange={(event) =>
                                  setSelectedIds((ids) =>
                                    event.target.checked
                                      ? [...new Set([...ids, task.taskIndex])]
                                      : ids.filter(
                                          (id) => id !== task.taskIndex,
                                        ),
                                  )
                                }
                              />
                            </TableCell>
                            <TableCell>
                              <Typography variant="body2">
                                #{task.taskIndex}
                              </Typography>
                              <Typography
                                variant="caption"
                                color="text.secondary"
                              >
                                {task.createTime
                                  ? formatDate(task.createTime, {
                                      year: "numeric",
                                      month: "numeric",
                                      day: "numeric",
                                    })
                                  : "—"}
                              </Typography>
                            </TableCell>
                            <TableCell sx={{ maxWidth: 330 }}>
                              <Typography
                                variant="body2"
                                sx={{ overflowWrap: "anywhere" }}
                              >
                                {deletedEndpoint(task, true)}
                              </Typography>
                              <Typography
                                variant="caption"
                                color="text.secondary"
                                sx={{ overflowWrap: "anywhere" }}
                              >
                                → {deletedEndpoint(task, false)}
                              </Typography>
                            </TableCell>
                            <TableCell
                              sx={{
                                fontFamily: "monospace",
                                whiteSpace: "nowrap",
                              }}
                            >
                              {taskNodeLabels(task, nodeLabel)}
                            </TableCell>
                            <TableCell>
                              {task.cleanup ? (
                                <CleanupStatus task={task} />
                              ) : (
                                <StatusChip status={task.status || "UNKNOWN"} />
                              )}
                            </TableCell>
                            <TableCell
                              align="right"
                              sx={{ whiteSpace: "nowrap" }}
                            >
                              <Typography variant="body2">
                                {task.putObjectCount === undefined
                                  ? "—"
                                  : t("{value1} 个对象", {
                                      value1: formatNumber(task.putObjectCount),
                                    })}
                              </Typography>
                              <Typography
                                variant="caption"
                                color="text.secondary"
                              >
                                {task.putObjectSize === undefined
                                  ? "—"
                                  : formatBytes(task.putObjectSize)}
                              </Typography>
                            </TableCell>
                          </TableRow>
                        ))}
                    </TableBody>
                  </Table>
                </TableContainer>
                <ListPagination
                  count={rows.length}
                  page={page}
                  pageSize={pageSize}
                  onPageChange={setPage}
                  onPageSizeChange={(size) => {
                    rememberPageSize("oss-transfer-recycle-page-size", size);
                    setPage(Math.floor((page * pageSize) / size));
                    setPageSize(size);
                  }}
                />
              </>
            ) : (
              !recycle.error &&
              !!recycle.data &&
              !recycle.data.partial &&
              recycle.data.complete !== false &&
              !nodeDiscoveryFailed &&
              membershipKnown &&
              missingNodes.length === 0 && (
                <EmptyState
                  title={t("暂无已删除任务")}
                  description={t("从任务列表删除的记录会移入回收站。")}
                />
              )
            )}
          </Paper>
          <Dialog
            open={confirmDelete}
            onClose={() => !deleting && setConfirmDelete(false)}
            maxWidth="sm"
            fullWidth
          >
            <DialogTitle>
              {t("永久删除 {count} 个任务？", {
                count: confirmedIds.length,
              })}
            </DialogTitle>
            <DialogContent>
              <Alert severity="warning" sx={{ mb: 2 }}>
                {t(
                  "此操作无法撤销，将清理所有执行节点上同 ID 的已删除任务记录。",
                )}
              </Alert>
              <Typography variant="body2" sx={{ overflowWrap: "anywhere" }}>
                {t("任务 ID：{ids}", {
                  ids:
                    confirmedIds
                      .slice(0, 20)
                      .map((id) => `#${id}`)
                      .join(", ") + (confirmedIds.length > 20 ? "…" : ""),
                })}
              </Typography>
              {deleting && <Box sx={{ mt: 2 }}>{deletionProgress}</Box>}
              <Divider sx={{ my: 2 }} />
              <Typography variant="body2" color="text.secondary">
                {t("源端和目标端的数据对象不受影响。")}
              </Typography>
            </DialogContent>
            <DialogActions>
              <Button
                disabled={deleting}
                onClick={() => setConfirmDelete(false)}
              >
                {t("取消")}
              </Button>
              <Button
                variant="contained"
                color="error"
                disabled={deleting}
                startIcon={
                  deleting ? (
                    <CircularProgress size={16} color="inherit" />
                  ) : (
                    <DeleteForeverRounded />
                  )
                }
                onClick={() => void deleteTasks()}
              >
                {t("确认永久删除")}
              </Button>
            </DialogActions>
          </Dialog>
        </>
      )}
    </Stack>
  );
}
