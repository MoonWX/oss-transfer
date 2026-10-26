import { t, useI18n, formatDate } from "../lib/i18n";
import { useNodeLabels } from "../lib/workerStatus";
import { taskNodeLabels, type NodeDisplay } from "../lib/nodeRegistry";
import { useSearchParams } from "react-router-dom";
import { useEffect, useMemo, useRef, useState } from "react";
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
import { api, ApiError, useApi, useNotify } from "../lib/api";
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
type DeletedTask = {
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
const errorText = (error: unknown) =>
  error instanceof Error ? error.message : String(error);
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
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(() =>
    savedPageSize("oss-transfer-recycle-page-size", 25),
  );
  const rows = useMemo(
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
  useEffect(() => {
    const lastPage = Math.max(0, Math.ceil(rows.length / pageSize) - 1);
    if (page > lastPage) setPage(lastPage);
  }, [rows.length, page, pageSize]);
  const uniqueIds = [...new Set(rows.map((task) => task.taskIndex))];
  const selectableIds = uniqueIds.slice(0, 128);
  const activeSelection = selectedIds.filter((id) => uniqueIds.includes(id));
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
    setDeleting(true);
    setDeleteError(null);
    try {
      const result = await api("/api/master/permanent-delete", {
        method: "POST",
        body: { taskIds: activeSelection },
      });
      setConfirmDelete(false);
      const detail = result.data || result;
      const failedWorkers = [
        ...new Set([
          ...(detail.failedWorkers || []),
          ...(detail.unreachableWorkers || []),
        ]),
      ];
      const partial =
        detail.partial || Number(detail.failed) > 0 || failedWorkers.length > 0;
      const details = Array.isArray(detail.details) ? detail.details : [];
      const removedIds = details
        .filter((item: any) => item.success)
        .map((item: any) => item.taskId);
      setEarlierRows((previous) =>
        previous.filter((task) => !removedIds.includes(task.taskIndex)),
      );
      const retained = [
        ...new Set(
          details.flatMap((item: any) => item.retainedAgentHistory ?? []),
        ),
      ];
      if (!partial) setSelectedIds([]);
      else
        setDeleteError(
          t("{value1}{value2}", {
            value1: detail.message || t("部分记录未删除，请刷新后重试。"),
            value2: failedWorkers.length
              ? t("（未完成节点：{value1}）", {
                  value1: failedWorkers
                    .map((id) =>
                      nodeLabel(String(id), detail?.nodeDisplays?.[String(id)]),
                    )
                    .join(", "),
                })
              : "",
          }),
        );
      setPage(0);
      notify(
        partial
          ? t("部分记录未删除{value1}。", {
              value1: failedWorkers.length
                ? t("，未响应节点：{value1}", {
                    value1: failedWorkers
                      .map((id) =>
                        nodeLabel(
                          String(id),
                          detail?.nodeDisplays?.[String(id)],
                        ),
                      )
                      .join(", "),
                  })
                : t("，请刷新后重试"),
            })
          : retained.length
            ? t("任务记录已删除；已移除节点的本地历史保留：{value1}。", {
                value1: retained
                  .map((id) =>
                    nodeLabel(String(id), detail?.nodeDisplays?.[String(id)]),
                  )
                  .join(", "),
              })
            : t("永久删除请求已提交，正在刷新回收站。"),
        partial ? "warning" : "info",
      );
      await Promise.all([recycle.refresh(), nodes.refresh()]);
    } catch (error) {
      const response = error instanceof ApiError ? error.response : undefined;
      const detail = response?.data || response;
      const failedWorkers: string[] = [
        ...new Set<string>([
          ...(detail?.failedWorkers || []),
          ...(detail?.unreachableWorkers || []),
        ]),
      ];
      const partial =
        detail?.partial ||
        Number(detail?.failed) > 0 ||
        failedWorkers.length > 0;
      const message = t("{value1}{value2}", {
        value1: errorText(error),
        value2: failedWorkers.length
          ? t("（未完成节点：{value1}）", {
              value1: failedWorkers
                .map((id) =>
                  nodeLabel(String(id), detail?.nodeDisplays?.[String(id)]),
                )
                .join(", "),
            })
          : "",
      });
      setDeleteError(message);
      notify(message, partial ? "warning" : "error");
      if (partial) {
        setConfirmDelete(false);
        setPage(0);
        // Keep the selection; activeSelection excludes IDs removed by the fresh response.
        await Promise.all([recycle.refresh(), nodes.refresh()]);
      }
    } finally {
      setDeleting(false);
    }
  }
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
          {deleteError && <Alert severity="warning">{deleteError}</Alert>}
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
                          ? t(" · 已选择 {value1} 个任务 ID（最多 128 个）", {
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
                  disabled={recycle.loading}
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
                  onClick={() => setConfirmDelete(true)}
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
                              "aria-label":
                                uniqueIds.length > 128
                                  ? t("选择前 128 个已删除任务")
                                  : t("选择全部已删除任务"),
                            }}
                            checked={
                              selectableIds.length > 0 &&
                              selectableIds.every((id) =>
                                activeSelection.includes(id),
                              )
                            }
                            indeterminate={
                              activeSelection.length > 0 &&
                              !selectableIds.every((id) =>
                                activeSelection.includes(id),
                              )
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
                        <TableCell>{t("最终状态")}</TableCell>
                        <TableCell align="right">{t("已传输")}</TableCell>
                      </TableRow>
                    </TableHead>
                    <TableBody>
                      {rows
                        .slice(page * pageSize, page * pageSize + pageSize)
                        .map((task) => (
                          <TableRow
                            hover
                            selected={activeSelection.includes(task.taskIndex)}
                            key={`${task.workerNode}-${task.taskIndex}`}
                          >
                            <TableCell padding="checkbox">
                              <Checkbox
                                inputProps={{
                                  "aria-label": t("选择任务 {value1}", {
                                    value1: task.taskIndex,
                                  }),
                                }}
                                checked={activeSelection.includes(
                                  task.taskIndex,
                                )}
                                disabled={
                                  activeSelection.length >= 128 &&
                                  !activeSelection.includes(task.taskIndex)
                                }
                                onChange={(event) =>
                                  setSelectedIds((ids) =>
                                    event.target.checked
                                      ? [
                                          ...new Set([...ids, task.taskIndex]),
                                        ].slice(0, 128)
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
                              <StatusChip status={task.status || "UNKNOWN"} />
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
                count: activeSelection.length,
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
                  ids: activeSelection.map((id) => `#${id}`).join(", "),
                })}
              </Typography>
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
