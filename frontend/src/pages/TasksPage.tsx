import { t as tr, useI18n, formatDate } from "../lib/i18n";
import { useWorkerStatus } from "../lib/workerStatus";
import { nodeName, type NodeRegistry } from "../lib/nodeRegistry";
import { parentTaskFilterStates, taskStatusGroup } from "../lib/taskStatus";
import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type UIEvent,
} from "react";
import { Link as RouterLink, useSearchParams } from "react-router-dom";
import {
  Alert,
  Avatar,
  Box,
  Button,
  ButtonBase,
  Checkbox,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Divider,
  Drawer,
  IconButton,
  InputAdornment,
  LinearProgress,
  MenuItem,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TablePagination,
  ToggleButton,
  ToggleButtonGroup,
  TableRow,
  TextField,
  Tooltip,
  Typography,
} from "@mui/material";
import {
  AddRounded,
  ArrowForwardRounded,
  CheckCircleOutlineRounded,
  CloseRounded,
  CloudSyncRounded,
  DeleteOutlineRounded,
  DownloadRounded,
  EastRounded,
  FolderOutlined,
  PauseRounded,
  PlayArrowRounded,
  SearchRounded,
  StorageRounded,
  WarningAmberRounded,
} from "@mui/icons-material";
import { api, saveFile, useApi, useNotify } from "../lib/api";
import { numberSubtasks } from "../lib/subtasks";
import { useTaskChildren, type ChildView } from "../lib/useTaskChildren";
import { downloadTaskReport } from "../lib/taskReport";
import {
  keyMappingLabels,
  mappingExample,
  type OssKeyMappingMode,
} from "../lib/ossKeyMapping";
import {
  refreshDescription,
  useRefreshPreference,
} from "../lib/refreshPreference";
import RefreshControls from "../components/RefreshControls";
import TaskBandwidth from "../components/TaskBandwidth";
import SubtaskDetails from "../components/SubtaskDetails";
import {
  EmptyState,
  ErrorAlert,
  formatBytes,
  formatNumber,
  LoadingState,
  PageHeader,
  StatusChip,
  statusLabels,
} from "../components/Common";

type Task = Record<string, any> & { taskIndex: number; status: string };
const isLocal = (task: Task) =>
  !!task.direction && task.direction !== "OSS_TO_OSS";
const isExploring = (task: Task) =>
  task.ossTaskGroup === true &&
  ["QUEUED", "RUNNING"].includes(task.creationStatus);
const groupIncomplete = (task: Task) =>
  isExploring(task) || (task.ossTaskGroup === true && task.partial === true);
const reportAvailable = (task: Task) =>
  !groupIncomplete(task) &&
  ["STOPPED", "COMPLETED", "FAILED", "PARTIAL_FAILED"].includes(task.status);
function TaskStatus({ task }: { task: Task }) {
  return <StatusChip status={task.status} />;
}
const directionLabel = (task: Task) =>
  task.direction === "OSS_TO_LOCAL"
    ? tr("OSS → 本地")
    : task.direction === "LOCAL_TO_OSS"
      ? tr("本地 → OSS")
      : "OSS → OSS";
const taskWorkers = (task: Task): string[] =>
  (task.participatingWorkerNodes?.length
    ? task.participatingWorkerNodes
    : task.eligibleWorkerNodes?.length
      ? task.eligibleWorkerNodes
      : [task.executionNodeIp || task.workerNode]
  ).filter(Boolean);
const workers = (task: Task) => taskWorkers(task).join(", ") || tr("等待分配");
function endpoint(task: Task, source: boolean) {
  const value = source ? task.sourceEndpoint : task.destinationEndpoint;
  if (
    value?.type === "LOCAL" ||
    (isLocal(task) && source === (task.direction === "LOCAL_TO_OSS"))
  )
    return (
      value?.localPath ||
      value?.nodeBindings?.[0]?.localPath ||
      task.localPath ||
      tr("本地文件系统")
    );
  return (
    [
      source ? task.sourceS3Alias : task.destS3Alias,
      source ? task.sourceBucket : task.destBucket,
    ]
      .filter(Boolean)
      .join("/") ||
    [value?.s3Alias, value?.bucket].filter(Boolean).join("/") ||
    "—"
  );
}
const subtaskNode = (task: Task) =>
  task.executionNodeIp || task.workerNode || "";
const subtaskIdentity = (task: Task) =>
  JSON.stringify([task.taskIndex, task.shardId, subtaskNode(task)]);
function subtaskEndpoint(task: Task, source: boolean) {
  const value = source ? task.sourceEndpoint : task.destinationEndpoint;
  if (
    value?.type === "LOCAL" ||
    (isLocal(task) && source === (task.direction === "LOCAL_TO_OSS"))
  ) {
    const binding = (value?.nodeBindings || task.nodeBindings || []).find(
      (item: any) => item.executionNodeIp === subtaskNode(task),
    );
    return (
      task.localPath ||
      binding?.localPath ||
      value?.localPath ||
      tr("本地文件系统")
    );
  }
  return endpoint(task, source);
}
function progress(task: Task) {
  const total = Number(task.listObjectCount || 0);
  return total
    ? Math.min(
        100,
        ((Number(task.putObjectCount || 0) +
          Number(task.skippedObjectCount || 0)) /
          total) *
          100,
      )
    : task.status === "COMPLETED"
      ? 100
      : 0;
}
const scanFinished = (task: Task) =>
  !groupIncomplete(task) &&
  (typeof task.scanComplete === "boolean"
    ? task.scanComplete
    : ["LIST_DONE", "COMPLETED"].includes(task.status));
const canStartTask = (task: Task) =>
  !isLocal(task) &&
  task.autoStart !== true &&
  task.status === "PENDING" &&
  !isExploring(task);
const canStopTask = (task: Task) =>
  isExploring(task) ||
  [
    "RUNNING",
    "LIST_DONE",
    "PENDING",
    "WAITING",
    "RESUMING",
    "RECOVERABLE",
  ].includes(task.status);
const canResumeTask = (task: Task) =>
  !isExploring(task) &&
  ["FAILED", "PARTIAL_FAILED", "STOPPED"].includes(task.status);
const canDeleteTask = (task: Task) =>
  ![
    "RUNNING",
    "LIST_DONE",
    "STOPPING",
    "RESUMING",
    "RECOVERABLE",
    "WAITING",
  ].includes(task.status);
function Metrics({ data }: { data: any }) {
  useI18n();
  const cards = [
    {
      label: tr("迁移任务"),
      value: formatNumber(data?.totalTasks),
      icon: <CloudSyncRounded />,
      color: "primary.main",
    },
    {
      label: tr("执行中任务"),
      value: formatNumber(
        (data?.runningTasks || 0) + (data?.listDoneTasks || 0),
      ),
      icon: <PlayArrowRounded />,
      color: "info.main",
    },
    {
      label: tr("已迁移数据"),
      value: formatBytes(data?.totalPutObjectSize),
      detail: tr("{value1} 个对象已传输", {
        value1: formatNumber(data?.totalPutObjectCount),
      }),
      icon: <CheckCircleOutlineRounded />,
      color: "success.main",
    },
    {
      label: tr("失败对象"),
      value: formatNumber(data?.totalFailedObjectCount),
      detail: tr("{value1} 个失败或部分失败任务", {
        value1: formatNumber(
          (data?.failedTasks || 0) + (data?.partialFailedTasks || 0),
        ),
      }),
      icon: <WarningAmberRounded />,
      color: "warning.main",
    },
  ];
  return (
    <Box
      sx={{
        display: "grid",
        gridTemplateColumns: {
          xs: "repeat(2, minmax(0, 1fr))",
          lg: "repeat(4, minmax(0, 1fr))",
        },
        gap: { xs: 1.5, md: 2 },
        mb: 3,
      }}
    >
      {cards.map((card) => (
        <Paper
          variant="outlined"
          key={card.label}
          sx={{ p: { xs: 2, md: 2.5 }, borderRadius: "18px" }}
        >
          <Stack
            direction="row"
            alignItems="center"
            justifyContent="space-between"
          >
            <Typography color="text.secondary" variant="body2">
              {card.label}
            </Typography>
            <Box
              sx={{
                color: card.color,
                display: "flex",
                "& svg": { fontSize: 21 },
              }}
            >
              {card.icon}
            </Box>
          </Stack>
          <Typography
            sx={{
              fontSize: { xs: 26, md: 32 },
              fontWeight: 500,
              letterSpacing: "-.8px",
              my: 1.5,
            }}
          >
            {data ? card.value : "—"}
          </Typography>
          {card.detail && (
            <Typography variant="caption" color="text.secondary">
              {data ? card.detail : tr("等待服务响应")}
            </Typography>
          )}
        </Paper>
      ))}
    </Box>
  );
}
function TaskDetail({
  task,
  taskId,
  loading,
  busy,
  onAction,
  onRefresh,
  operationError,
  childId: requestedChildId,
  childNode: requestedChildNode,
  onSelectChild,
  loadError,
  loadPartial,
  onClose,
}: {
  task: Task | undefined;
  taskId: number | undefined;
  loading: boolean;
  busy: boolean;
  onAction: (task: Task, operation: string) => void;
  onRefresh: () => void;
  operationError: string;
  childId?: string;
  childNode?: string;
  onSelectChild: (id?: string, node?: string) => void;
  loadError?: string | null;
  loadPartial: boolean;
  onClose: () => void;
}) {
  useI18n();
  const notify = useNotify();
  const [exporting, setExporting] = useState(false);
  const local = task && isLocal(task);
  const { refreshMs } = useRefreshPreference();
  const childNumber = Number(requestedChildId?.split("-")[1]);
  const [pageWindow, setPageWindow] = useState({
    parentId: taskId,
    offset:
      Number.isInteger(childNumber) && childNumber > 0
        ? Math.floor((childNumber - 1) / 25) * 25
        : 0,
  });
  const offset =
    pageWindow.parentId === taskId
      ? pageWindow.offset
      : Number.isInteger(childNumber) && childNumber > 0
        ? Math.floor((childNumber - 1) / 25) * 25
        : 0;
  const [visibility, setVisibility] = useState({
    parentId: taskId,
    visible: false,
  });
  const pageVisible = visibility.parentId === taskId && visibility.visible;
  const childrenRegion = useRef<HTMLDivElement>(null);
  const regionObserver = useRef<IntersectionObserver | undefined>(undefined);
  const attachChildrenRegion = useCallback(
    (region: HTMLDivElement | null) => {
      childrenRegion.current = region;
      regionObserver.current?.disconnect();
      setVisibility({ parentId: taskId, visible: false });
      if (region) {
        regionObserver.current = new IntersectionObserver(([entry]) =>
          setVisibility({ parentId: taskId, visible: entry.isIntersecting }),
        );
        regionObserver.current.observe(region);
      }
    },
    [taskId],
  );
  const [childrenFilter, setChildrenFilter] = useState<{
    parentId?: number;
    view: ChildView;
  }>({ parentId: taskId, view: "all" });
  const childView =
    childrenFilter.parentId === taskId ? childrenFilter.view : "all";
  const visibleChildOffsets = useRef(new Set<number>());
  const visibleChildElements = useRef(new Set<Element>());
  const childRowsObserver = useRef<IntersectionObserver | undefined>(undefined);
  const observeChildRow = useCallback((element: HTMLDivElement | null) => {
    if (!element) return;
    childRowsObserver.current ??= new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting)
          visibleChildElements.current.add(entry.target);
        else visibleChildElements.current.delete(entry.target);
      }
      visibleChildOffsets.current = new Set(
        [...visibleChildElements.current].map((row) =>
          Number(row.getAttribute("data-child-offset")),
        ),
      );
    });
    const observer = childRowsObserver.current;
    observer.observe(element);
    return () => {
      observer.unobserve(element);
      visibleChildElements.current.delete(element);
      visibleChildOffsets.current = new Set(
        [...visibleChildElements.current].map((row) =>
          Number(row.getAttribute("data-child-offset")),
        ),
      );
    };
  }, []);
  const children = useTaskChildren({
    parentId: taskId,
    view: childView,
    initialOffset: childView === "active" ? 0 : offset,
    enabled: !!taskId && !requestedChildId && pageVisible,
    refreshMs,
    visibleOffsets: visibleChildOffsets,
  });
  const [moreVisible, setMoreVisible] = useState(false);
  const moreObserver = useRef<IntersectionObserver | undefined>(undefined);
  const attachMore = useCallback(
    (element: HTMLDivElement | null) => {
      moreObserver.current?.disconnect();
      setMoreVisible(false);
      if (element) {
        moreObserver.current = new IntersectionObserver(
          ([entry]) => setMoreVisible(entry.isIntersecting),
          { rootMargin: "0px 0px 160px 0px" },
        );
        moreObserver.current.observe(element);
      }
    },
    [taskId, childView],
  );
  useEffect(() => {
    if (moreVisible && !children.loading && !children.error) {
      setMoreVisible(false);
      children.loadMore();
    }
  }, [moreVisible, children.loading, children.error, children.loadMore]);
  useEffect(
    () => () => {
      regionObserver.current?.disconnect();
      childRowsObserver.current?.disconnect();
      moreObserver.current?.disconnect();
    },
    [],
  );
  const validChild =
    !!taskId &&
    requestedChildId === `${taskId}-${childNumber}` &&
    Number.isInteger(childNumber) &&
    childNumber > 0;
  const child = useApi(
    validChild
      ? `/api/master/tasks/${taskId}/children/${childNumber}${requestedChildNode ? `?executionNodeIp=${encodeURIComponent(requestedChildNode)}` : ""}`
      : null,
    refreshMs,
  );
  const pageData = children.data;
  const childData =
    taskId != null &&
    child.data?.data?.parentTaskId === taskId &&
    child.data.data.offset === childNumber - 1
      ? child.data.data
      : undefined;
  const lastView = useRef<
    | {
        taskId: number;
        childId?: string;
        childNode?: string;
        current: Task | undefined;
        shards: any[];
        shardsLoading: boolean;
        shardsComplete: boolean;
        childrenTotal: number;
        childShards: any[];
        childLoading: boolean;
        childPartial: boolean;
        childError?: string | null;
        loading: boolean;
        operationError: string;
        error: string | null;
        partial: boolean;
      }
    | undefined
  >(undefined);
  if (taskId) {
    lastView.current = {
      taskId,
      childId: requestedChildId,
      childNode: requestedChildNode,
      current: task,
      shards: pageData?.items || [],
      shardsLoading: !pageData && !children.error,
      shardsComplete: !!pageData && !children.error && !pageData.partial,
      childrenTotal: pageData?.totalCount || 0,
      childShards: childData?.items || [],
      childLoading: validChild && !childData && !child.error,
      childPartial: !!childData?.partial,
      childError: child.error,
      loading,
      operationError,
      error: loadError || null,
      partial: loadPartial || !!task?.partial || !!pageData?.partial,
    };
  }
  // Polling follows the live task above; only presentation survives the exit.
  const view = lastView.current;
  const current = view?.current;
  const childId = view?.childId;
  const childNode = view?.childNode;
  const sourceObjectKey =
    current?.objectKey || current?.sourceEndpoint?.objectKey;
  const sourcePrefix =
    current?.prefix ||
    (current?.sourceEndpoint?.prefixFilterEnabled === true
      ? current.sourceEndpoint.prefix
      : current?.keyMapping?.mode === "REPLACE_PREFIX"
        ? current.keyMapping.sourcePrefix
        : undefined);
  const shards = numberSubtasks(
    (view?.shards || []).filter(
      (shard) =>
        shard.taskIndex !== current?.taskIndex || shard.shardId !== "discovery",
    ),
    current?.childTaskIds,
  );
  const paperRef = useRef<HTMLDivElement>(null);
  const parentScroll = useRef(0);
  const previousScreen = useRef({ taskId, childId, childNode });
  const parentFocus = useRef<{ id: string; node?: string } | undefined>(
    undefined,
  );
  const selectedIdentity = useRef<{ key: string; identity?: string }>({
    key: "",
  });
  const selectionKey = JSON.stringify([view?.taskId, childId, childNode]);
  if (selectedIdentity.current.key !== selectionKey)
    selectedIdentity.current = { key: selectionKey };
  const matches = numberSubtasks(view?.childShards || []).filter(
    (shard: Task) =>
      childId === `${view?.taskId}-${shard.subtaskNumber}` &&
      (!childNode || subtaskNode(shard) === childNode),
  );
  const selectedChild = selectedIdentity.current.identity
    ? matches.find(
        (shard: Task) =>
          subtaskIdentity(shard) === selectedIdentity.current.identity,
      )
    : matches.length === 1
      ? matches[0]
      : undefined;
  if (selectedChild)
    selectedIdentity.current.identity = subtaskIdentity(selectedChild);
  useLayoutEffect(() => {
    if (!taskId) {
      parentFocus.current = undefined;
      return;
    }
    const paper = paperRef.current;
    const previous = previousScreen.current;
    if (paper) {
      if (previous.taskId !== taskId) parentScroll.current = 0;
      if (childId) {
        paper.scrollTop = 0;
        paper
          .querySelector<HTMLElement>("[data-subtask-heading]")
          ?.focus({ preventScroll: true });
      } else if (previous.childId) {
        parentFocus.current = {
          id: previous.childId,
          node: previous.childNode,
        };
        paper.scrollTop = parentScroll.current;
        if (!pageData)
          childrenRegion.current?.scrollIntoView({ block: "start" });
      }
      if (!childId && parentFocus.current) {
        const target = parentFocus.current;
        const button = Array.from(
          paper.querySelectorAll<HTMLButtonElement>("[data-child-id]"),
        ).find(
          (button) =>
            button.dataset.childId === target.id &&
            (!target.node || button.dataset.childNode === target.node),
        );
        if (button) {
          button.scrollIntoView({ block: "nearest" });
          button.focus({ preventScroll: true });
          parentFocus.current = undefined;
        }
      }
    }
    previousScreen.current = { taskId, childId, childNode };
  }, [taskId, childId, childNode, pageData?.items.length]);
  const exportReport = async () => {
    if (!current || !reportAvailable(current)) return;
    const taskId = current.taskIndex;
    setExporting(true);
    try {
      await downloadTaskReport(taskId, (message) =>
        notify(
          tr("任务 #{value1}：{value2}", { value1: taskId, value2: message }),
          "error",
        ),
      );
      notify(tr("报告下载已请求，请查看浏览器下载列表。"), "info");
    } catch (error) {
      notify(
        error instanceof Error ? error.message : tr("报告下载失败，请重试。"),
        "error",
      );
    } finally {
      setExporting(false);
    }
  };
  return (
    <Drawer
      anchor="right"
      open={!!taskId}
      onClose={onClose}
      slotProps={{
        paper: {
          ref: paperRef,
          onScroll: (event: UIEvent<HTMLDivElement>) => {
            if (!childId) parentScroll.current = event.currentTarget.scrollTop;
          },
        },
        transition: {
          onExited: () => {
            lastView.current = undefined;
          },
        },
      }}
      sx={{
        "& .MuiDrawer-paper": {
          width: { xs: "100%", sm: 560 },
          p: { xs: 2.5, sm: 3.5 },
        },
      }}
    >
      {childId && (
        <SubtaskDetails
          childId={childId}
          task={selectedChild}
          direction={selectedChild ? directionLabel(selectedChild as Task) : ""}
          source={
            selectedChild ? subtaskEndpoint(selectedChild as Task, true) : ""
          }
          destination={
            selectedChild ? subtaskEndpoint(selectedChild as Task, false) : ""
          }
          loading={!!view?.childLoading}
          partial={!!view?.childPartial}
          error={view?.childError}
          onBack={() => onSelectChild()}
          onClose={onClose}
          onRefresh={() => {
            onRefresh();
            void child.refresh();
          }}
        />
      )}
      {!current && !childId && (
        <>
          <Stack
            direction="row"
            justifyContent="space-between"
            alignItems="center"
            sx={{ mb: 3 }}
          >
            <Typography variant="h5">
              {tr("迁移任务 #")}
              {view?.taskId}
            </Typography>
            <IconButton onClick={onClose} aria-label={tr("关闭详情")}>
              <CloseRounded />
            </IconButton>
          </Stack>
          {view?.loading ? (
            <LoadingState />
          ) : (
            <Alert
              severity="info"
              action={
                <Button onClick={onRefresh} size="small" color="inherit">
                  {tr("刷新")}
                </Button>
              }
            >
              {tr("当前列表中未找到此任务，请刷新任务状态。")}
            </Alert>
          )}
        </>
      )}
      {current && (
        <Box sx={{ display: childId ? "none" : "block" }}>
          <Stack
            direction="row"
            alignItems="center"
            justifyContent="space-between"
            sx={{ mb: 3 }}
          >
            <Box>
              <Typography variant="overline" color="text.secondary">
                {tr("任务详情")}
              </Typography>
              <Typography variant="h5">
                {tr("迁移任务 #")}
                {current.taskIndex}
              </Typography>
            </Box>
            <IconButton onClick={onClose} aria-label={tr("关闭详情")}>
              <CloseRounded />
            </IconButton>
          </Stack>
          <Stack direction="row" gap={1} sx={{ mb: 3 }}>
            <TaskStatus task={current} />
            <Chip
              variant="outlined"
              size="small"
              label={directionLabel(current)}
            />
          </Stack>
          {(canStopTask(current) ||
            canResumeTask(current) ||
            canDeleteTask(current)) && (
            <Stack direction="row" gap={1} flexWrap="wrap" sx={{ mb: 3 }}>
              {canStartTask(current) && (
                <Button
                  variant="contained"
                  startIcon={<PlayArrowRounded />}
                  disabled={busy}
                  onClick={() => onAction(current, "启动")}
                >
                  {tr("启动任务")}
                </Button>
              )}
              {canStopTask(current) && (
                <Button
                  variant="outlined"
                  color="warning"
                  startIcon={<PauseRounded />}
                  disabled={busy}
                  onClick={() => onAction(current, "停止")}
                >
                  {tr("停止任务")}
                </Button>
              )}
              {canResumeTask(current) && (
                <Button
                  variant="contained"
                  startIcon={<PlayArrowRounded />}
                  disabled={busy}
                  onClick={() => onAction(current, "继续传输")}
                >
                  {tr("继续传输")}
                </Button>
              )}
              {canDeleteTask(current) && (
                <Button
                  variant="text"
                  color="error"
                  startIcon={<DeleteOutlineRounded />}
                  disabled={busy}
                  onClick={() => onAction(current, "删除")}
                >
                  {tr("删除任务")}
                </Button>
              )}
            </Stack>
          )}
          {view?.operationError && (
            <Alert severity="warning" sx={{ mb: 2 }}>
              {view.operationError}
            </Alert>
          )}
          <TaskBandwidth
            completed={current.status === "COMPLETED"}
            key={current.taskIndex}
            taskId={current.taskIndex}
            active={!!taskId}
            refreshMs={refreshMs}
          />
          <ErrorAlert error={view?.error || null} onRetry={onRefresh} />
          {view?.partial && (
            <Alert severity="warning" sx={{ mb: 2 }}>
              {tr("任务信息不完整，请刷新或检查执行节点。")}
            </Alert>
          )}
          {current.errorMessage && (
            <Alert severity="error" sx={{ mb: 2 }}>
              {current.errorMessage}
            </Alert>
          )}
          <Paper variant="outlined" sx={{ p: 2.5, mb: 3 }}>
            <Typography variant="caption" color="text.secondary">
              {tr("来源")}
            </Typography>
            <Typography sx={{ overflowWrap: "anywhere", mt: 0.5 }}>
              {endpoint(current, true)}
            </Typography>
            <EastRounded sx={{ my: 1.5, color: "text.secondary" }} />
            <Typography
              variant="caption"
              display="block"
              color="text.secondary"
            >
              {tr("目标")}
            </Typography>
            <Typography sx={{ overflowWrap: "anywhere", mt: 0.5 }}>
              {endpoint(current, false)}
            </Typography>
          </Paper>
          <Typography variant="h6" sx={{ mb: 2 }}>
            {tr("传输统计")}
          </Typography>
          <Box
            sx={{
              display: "grid",
              gridTemplateColumns: "1fr 1fr",
              gap: 2.5,
              mb: 3,
            }}
          >
            {[
              [tr("已扫描"), current.listObjectCount, current.listObjectSize],
              [tr("已迁移"), current.putObjectCount, current.putObjectSize],
              [
                tr("已跳过"),
                current.skippedObjectCount,
                current.skippedObjectSize,
              ],
              [
                tr("失败对象"),
                current.failedObjectCount,
                current.failedObjectSize,
              ],
            ].map(([label, count, bytes]) => (
              <Box key={label}>
                <Typography variant="caption" color="text.secondary">
                  {label}
                </Typography>
                <Typography sx={{ fontWeight: 500 }}>
                  {formatNumber(count)}{" "}
                  <Box
                    component="span"
                    sx={{ fontSize: 12, color: "text.secondary" }}
                  >
                    / {formatBytes(bytes)}
                  </Box>
                </Typography>
              </Box>
            ))}
          </Box>
          <Box sx={{ mb: 3 }}>
            <Button
              variant="outlined"
              startIcon={
                exporting ? (
                  <CircularProgress size={16} color="inherit" />
                ) : (
                  <DownloadRounded />
                )
              }
              disabled={exporting || !reportAvailable(current)}
              onClick={() => void exportReport()}
            >
              {exporting ? tr("正在准备下载") : tr("导出任务报告")}
            </Button>
            {!reportAvailable(current) && (
              <Typography
                variant="caption"
                color="text.secondary"
                sx={{ display: "block", mt: 1 }}
              >
                {current.ossTaskGroup && current.partial
                  ? tr("节点状态恢复后可导出报告。")
                  : tr("停止或结束后可导出报告。")}
              </Typography>
            )}
          </Box>
          {(current.failedObjectCount > 0 ||
            ["FAILED", "PARTIAL_FAILED"].includes(current.status)) && (
            <Button
              component={RouterLink}
              to={`/exceptions?taskId=${current.taskIndex}&type=${local ? "local" : "oss"}`}
              onClick={onClose}
              color="warning"
              variant="outlined"
              endIcon={<ArrowForwardRounded />}
              sx={{ mb: 3 }}
            >
              {current.failedObjectCount > 0
                ? tr("查看失败对象")
                : tr("查看异常记录")}
            </Button>
          )}
          <Divider sx={{ mb: 3 }} />
          <Typography variant="h6" sx={{ mb: 2 }}>
            {tr("执行信息")}
          </Typography>
          <Stack gap={2}>
            {[
              [tr("执行节点"), workers(current)],
              [
                tr("节点分发方式"),
                current.distributionMode === "balanced"
                  ? tr("均衡分发")
                  : tr("亲和分发"),
              ],
              [tr("共享存储 ID"), current.storageId],
              [
                tr("当前速度"),
                current.currentSpeedBytesPerSecond == null
                  ? null
                  : `${formatBytes(current.currentSpeedBytesPerSecond)}/s`,
              ],
              [
                tr("平均速度"),
                current.averageSpeedBytesPerSecond == null
                  ? null
                  : `${formatBytes(current.averageSpeedBytesPerSecond)}/s`,
              ],
              [
                tr("创建时间"),
                current.createTime == null || current.createTime === ""
                  ? null
                  : formatDate(current.createTime),
              ],
              [
                tr("开始时间"),
                current.startTime == null || current.startTime === ""
                  ? null
                  : formatDate(current.startTime),
              ],
              [
                tr("结束时间"),
                current.endTime == null || current.endTime === ""
                  ? null
                  : formatDate(current.endTime),
              ],
              [tr("耗时"), current.duration],
              [tr("起始 Key"), current.startKey],
              [tr("结束 Key"), current.endKey],
              [tr("源对象 Key"), !local ? sourceObjectKey : null],
              [tr("源前缀"), !local ? sourcePrefix : null],
              [
                tr("目标命名"),
                !local
                  ? keyMappingLabels[
                      (current.keyMapping?.mode ||
                        "PRESERVE") as OssKeyMappingMode
                    ] || current.keyMapping?.mode
                  : null,
              ],
              [
                tr("目标前缀"),
                !local && current.keyMapping?.targetPrefix != null
                  ? JSON.stringify(current.keyMapping.targetPrefix)
                  : null,
              ],
              [
                tr("目标对象 Key"),
                !local ? current.keyMapping?.targetKey : null,
              ],
              [
                sourceObjectKey ? tr("Key 映射") : tr("Key 示例"),
                !local && current.keyMapping
                  ? mappingExample(
                      sourceObjectKey
                        ? "object"
                        : sourcePrefix
                          ? "prefix"
                          : "all",
                      sourceObjectKey || sourcePrefix || "",
                      current.keyMapping,
                    )
                  : null,
              ],
              [
                tr("修改时间晚于"),
                current.dateAfter == null || current.dateAfter === ""
                  ? null
                  : formatDate(current.dateAfter),
              ],
              [
                tr("修改时间早于"),
                current.dateBefore == null || current.dateBefore === ""
                  ? null
                  : formatDate(current.dateBefore),
              ],
              [
                tr("冲突策略"),
                current.conflictPolicy ||
                  (current.overwrite ? "OVERWRITE" : "SKIP"),
              ],
            ]
              .filter(([, value]) => value != null && value !== "")
              .map(([label, value]) => (
                <Box
                  key={label}
                  sx={{
                    display: "grid",
                    gridTemplateColumns: "105px minmax(0, 1fr)",
                    gap: 2,
                  }}
                >
                  <Typography variant="body2" color="text.secondary">
                    {label}
                  </Typography>
                  <Typography
                    variant="body2"
                    sx={{ overflowWrap: "anywhere", whiteSpace: "pre-wrap" }}
                  >
                    {String(value)}
                  </Typography>
                </Box>
              ))}
          </Stack>
          {(local || current.ossTaskGroup) && (
            <>
              <Divider sx={{ my: 3 }} />
              <Box
                ref={attachChildrenRegion}
                data-testid="task-children-region"
              >
                <Stack
                  direction="row"
                  justifyContent="space-between"
                  alignItems="center"
                  spacing={1}
                  sx={{ mb: 2 }}
                >
                  <Typography variant="h6">{tr("子任务")}</Typography>
                  <ToggleButtonGroup
                    value={childView}
                    exclusive
                    size="small"
                    aria-label={tr("子任务筛选")}
                    onChange={(_, value: ChildView | null) => {
                      if (value)
                        setChildrenFilter({ parentId: taskId, view: value });
                    }}
                  >
                    <ToggleButton value="all">{tr("全部")}</ToggleButton>
                    <ToggleButton value="active">{tr("进行中")}</ToggleButton>
                  </ToggleButtonGroup>
                </Stack>
                {childView === "all" && offset > 0 && (
                  <Button
                    size="small"
                    sx={{ mb: 1 }}
                    onClick={() =>
                      setPageWindow({ parentId: taskId, offset: 0 })
                    }
                  >
                    {tr("从头查看子任务")}
                  </Button>
                )}
                {view?.shardsLoading ? (
                  <Stack
                    direction="row"
                    alignItems="center"
                    gap={1}
                    role="status"
                  >
                    <CircularProgress size={16} />
                    <Typography variant="body2" color="text.secondary">
                      {tr("正在加载子任务…")}
                    </Typography>
                  </Stack>
                ) : shards.length === 0 ? (
                  view?.shardsComplete ? (
                    <Typography variant="body2" color="text.secondary">
                      {childView === "active"
                        ? tr("暂无进行中的子任务")
                        : view?.childrenTotal
                          ? tr("此范围暂无子任务")
                          : tr("尚未生成子任务")}
                    </Typography>
                  ) : null
                ) : (
                  <Stack gap={1.5}>
                    {shards.map((shard: any) => (
                      <Paper
                        variant="outlined"
                        key={subtaskIdentity(shard)}
                        ref={observeChildRow}
                        data-child-offset={children.pageOffset(shard)}
                        sx={{ overflow: "hidden" }}
                      >
                        <ButtonBase
                          aria-label={tr("查看子任务 {value1}-{value2}", {
                            value1: current.taskIndex,
                            value2: shard.subtaskNumber,
                          })}
                          data-child-id={`${current.taskIndex}-${shard.subtaskNumber}`}
                          data-child-node={subtaskNode(shard)}
                          onClick={() =>
                            onSelectChild(
                              `${current.taskIndex}-${shard.subtaskNumber}`,
                              subtaskNode(shard),
                            )
                          }
                          sx={{
                            p: 2,
                            width: "100%",
                            textAlign: "left",
                            display: "flex",
                            gap: 1.5,
                            justifyContent: "space-between",
                            "&:hover": { bgcolor: "action.hover" },
                            "&.Mui-focusVisible": {
                              bgcolor: "action.focus",
                              outline: "2px solid",
                              outlineColor: "primary.main",
                              outlineOffset: -2,
                            },
                          }}
                        >
                          <Box sx={{ minWidth: 0, flex: 1 }}>
                            <Stack
                              direction="row"
                              justifyContent="space-between"
                              gap={1}
                            >
                              <Typography
                                variant="body2"
                                sx={{ overflowWrap: "anywhere" }}
                              >
                                {tr("子任务 #")}
                                {current.taskIndex}-{shard.subtaskNumber} ·{" "}
                                {shard.executionNodeIp || shard.workerNode}
                              </Typography>
                              <StatusChip status={shard.status} />
                            </Stack>
                            <Typography
                              variant="caption"
                              color="text.secondary"
                            >
                              {tr("已迁移 {transferred} · 失败 {failed}", {
                                transferred: formatNumber(shard.putObjectCount),
                                failed: formatNumber(shard.failedObjectCount),
                              })}
                            </Typography>
                          </Box>
                          <ArrowForwardRounded
                            sx={{ fontSize: 18, color: "text.secondary" }}
                          />
                        </ButtonBase>
                      </Paper>
                    ))}
                  </Stack>
                )}
                {(pageData || children.error) && (
                  <Box
                    ref={attachMore}
                    data-testid="task-children-more"
                    sx={{ pt: 2, minHeight: 32, textAlign: "center" }}
                  >
                    {children.error ? (
                      <ErrorAlert
                        error={children.error}
                        onRetry={children.retry}
                      />
                    ) : children.loadingMore ? (
                      <CircularProgress size={18} />
                    ) : pageData?.hasMore ? (
                      <Typography variant="caption" color="text.secondary">
                        {tr("向下滚动加载更多")}
                      </Typography>
                    ) : shards.length > 0 ? (
                      <Typography variant="caption" color="text.secondary">
                        {tr("已加载全部 {count} 个子任务", {
                          count: shards.length,
                        })}
                      </Typography>
                    ) : null}
                  </Box>
                )}
              </Box>
            </>
          )}
        </Box>
      )}
    </Drawer>
  );
}
export default function TasksPage({
  overview = false,
}: {
  overview?: boolean;
}) {
  const { locale } = useI18n();
  const notify = useNotify();
  const { refreshMs, refreshSeconds } = useRefreshPreference();
  const { data, loading, error, refresh } = useApi(
    "/api/master/status",
    refreshMs,
    true,
  );
  const [historyTasks, setHistoryTasks] = useState<Task[] | null>(null);
  const [historyCursor, setHistoryCursor] = useState<number | null>();
  const [historyLoading, setHistoryLoading] = useState(false);
  const [historyError, setHistoryError] = useState<string | null>(null);
  const historyRequest = useRef<AbortController | null>(null);
  useEffect(() => () => historyRequest.current?.abort(), []);
  const mergeTasks = (older: Task[], newer: Task[]) => [
    ...new Map(
      [...older, ...newer].map((task) => [task.taskIndex, task]),
    ).values(),
  ];
  useEffect(() => {
    if (data?.tasks)
      setHistoryTasks((previous) =>
        previous === null ? null : mergeTasks(previous, data.tasks),
      );
  }, [data]);
  const nextHistoryCursor =
    historyCursor === undefined ? data?.nextBeforeId : historyCursor;
  async function loadEarlierTasks() {
    if (!nextHistoryCursor || historyRequest.current) return;
    const controller = new AbortController();
    historyRequest.current = controller;
    setHistoryLoading(true);
    setHistoryError(null);
    try {
      const next = await api<{ tasks: Task[]; nextBeforeId?: number }>(
        `/api/master/status?beforeId=${nextHistoryCursor}&limit=1000`,
        { signal: controller.signal },
      );
      setHistoryTasks((previous) =>
        mergeTasks(mergeTasks(previous ?? [], data?.tasks ?? []), next.tasks),
      );
      setHistoryCursor(next.nextBeforeId ?? null);
    } catch (cause) {
      if (!controller.signal.aborted)
        setHistoryError(
          cause instanceof Error ? cause.message : tr("加载失败"),
        );
    } finally {
      historyRequest.current = null;
      if (!controller.signal.aborted) setHistoryLoading(false);
    }
  }
  const health = useWorkerStatus();
  const nodeHealth = useApi("/api/master/local-nodes", refreshMs);
  const registry = useApi<{ data: NodeRegistry }>(
    "/api/master/nodes",
    refreshMs,
  );
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState("");
  const [direction, setDirection] = useState("");
  const [worker, setWorker] = useState("");
  const [selected, setSelected] = useState<number[]>([]);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(10);
  const [searchParams, setSearchParams] = useSearchParams();
  const childId = searchParams.get("childTaskId") || undefined;
  const childNode = searchParams.get("childNode") || undefined;
  const setChildId = (id?: string, node?: string) => {
    setSearchParams((current) => {
      const next = new URLSearchParams(current);
      if (id) next.set("childTaskId", id);
      else next.delete("childTaskId");
      if (id && node) next.set("childNode", node);
      else next.delete("childNode");
      return next;
    });
  };
  const requestedId = Number(searchParams.get("taskId"));
  const linkedTask =
    data?.tasks?.find((task: Task) => task.taskIndex === requestedId) ||
    data?.tasks?.find(
      (task: Task) =>
        task.ossTaskGroup && task.childTaskIds?.includes(requestedId),
    );
  const detailId =
    Number.isSafeInteger(requestedId) && requestedId > 0
      ? (linkedTask?.taskIndex ?? requestedId)
      : undefined;
  const detailFallback = useApi<{ data: Task }>(
    detailId !== undefined && !linkedTask
      ? `/api/master/tasks/${detailId}`
      : null,
    refreshMs,
  );
  useEffect(() => {
    const task = detailFallback.data?.data;
    if (task)
      setHistoryTasks((previous) =>
        previous === null ? null : mergeTasks(previous, [task]),
      );
  }, [detailFallback.data]);
  useEffect(() => {
    if (detailId === undefined || detailId === requestedId) return;
    setSearchParams(
      (current) => {
        const next = new URLSearchParams(current);
        next.set("taskId", String(detailId));
        return next;
      },
      { replace: true },
    );
  }, [detailId, requestedId, setSearchParams]);
  const setDetailId = (id: number | undefined) => {
    setSearchParams((current) => {
      const next = new URLSearchParams(current);
      next.delete("childTaskId");
      next.delete("childNode");
      if (id === undefined) {
        next.delete("taskId");
        next.delete("creationId");
      } else next.set("taskId", String(id));
      return next;
    });
  };
  const [operation, setOperation] = useState("");
  const [operationIds, setOperationIds] = useState<number[] | null>(null);
  const [operationOpen, setOperationOpen] = useState(false);
  const [confirmationTasks, setConfirmationTasks] = useState<Task[]>([]);
  const [busy, setBusy] = useState(false);
  const [actionResult, setActionResult] = useState("");
  const tasks: Task[] = useMemo(
    () => mergeTasks(historyTasks ?? [], data?.tasks ?? []),
    [historyTasks, data],
  );
  const stateOptions = parentTaskFilterStates(tasks, status);
  const workerOptions = useMemo(() => {
    const nodes = new Map<string, string>();
    const registered = registry.data?.data?.nodes ?? [];
    const nameCounts = new Map<string, number>();
    for (const node of registered) {
      const name = nodeName(node);
      nameCounts.set(name, (nameCounts.get(name) ?? 0) + 1);
    }
    for (const node of registered) {
      const name = nodeName(node);
      nodes.set(
        node.address,
        (nameCounts.get(name) ?? 0) > 1 ? `${name} (${node.address})` : name,
      );
    }
    for (const task of tasks) {
      for (const address of taskWorkers(task)) {
        if (!nodes.has(address)) nodes.set(address, address);
      }
    }
    if (worker && !nodes.has(worker)) nodes.set(worker, worker);
    return [...nodes.entries()].sort((a, b) =>
      a[1].localeCompare(b[1], locale),
    );
  }, [registry.data, tasks, worker, locale]);
  const detailTask = linkedTask || detailFallback.data?.data;
  const actionCandidates = detailTask ? mergeTasks(tasks, [detailTask]) : tasks;
  const filtered = useMemo(
    () =>
      tasks
        .filter(
          (t) =>
            (!status || taskStatusGroup(t.status) === status) &&
            (!direction || (t.direction || "OSS_TO_OSS") === direction) &&
            (!worker || taskWorkers(t).includes(worker)) &&
            (!query ||
              `${t.taskIndex} ${endpoint(t, true)} ${endpoint(t, false)} ${workers(t)} ${t.storageId || ""}`
                .toLowerCase()
                .includes(query.toLowerCase())),
        )
        .sort((a, b) => b.taskIndex - a.taskIndex),
    [tasks, query, status, direction, worker, locale],
  );
  useEffect(() => {
    setPage(0);
    setSelected([]);
  }, [query, status, direction, worker]);
  useEffect(() => {
    if (data) {
      setSelected((s) =>
        s.filter((id) => tasks.some((t: Task) => t.taskIndex === id)),
      );
      setPage((p) =>
        Math.min(p, Math.max(0, Math.ceil(filtered.length / pageSize) - 1)),
      );
    }
  }, [data, tasks, filtered.length, pageSize]);
  const shown = overview
    ? filtered.slice(0, 5)
    : filtered.slice(page * pageSize, (page + 1) * pageSize);
  const chosen = tasks.filter((t) => selected.includes(t.taskIndex));
  const actionTasks =
    operationIds === null
      ? chosen
      : actionCandidates.filter((t) => operationIds.includes(t.taskIndex));
  const openOperation = (next: string, ids: number[] | null) => {
    setOperationIds(ids);
    setConfirmationTasks(
      ids === null
        ? chosen
        : actionCandidates.filter((task) => ids.includes(task.taskIndex)),
    );
    setOperation(next);
    setOperationOpen(true);
  };
  const canStart = chosen.length > 0 && chosen.every(canStartTask);
  const canStop = chosen.length > 0 && chosen.every(canStopTask);
  const canResume = chosen.length > 0 && chosen.every(canResumeTask);
  const canDelete = chosen.length > 0 && chosen.every(canDeleteTask);
  const offline = Object.values(health.data?.workers || {}).filter(
    (w: any) => w.success === false || w.error || w.reachable === false,
  );
  const missingNodes = (nodeHealth.data?.data?.nodes || []).some(
    (n: any) =>
      !n.reachable ||
      (health.data && !health.data.workers?.[n.executionNodeIp]),
  );
  const toggle = (id: number) =>
    setSelected((s) =>
      s.includes(id) ? s.filter((i) => i !== id) : [...s, id].slice(0, 128),
    );
  async function execute() {
    if (!actionTasks.length) return;
    setBusy(true);
    setActionResult("");
    const ids = (local: boolean) =>
      actionTasks.filter((t) => isLocal(t) === local).map((t) => t.taskIndex);
    const requests: [string, number[]][] =
      operation === "启动"
        ? [["/start", ids(false)]]
        : operation === "停止"
          ? [
              ["/stop", ids(false)],
              ["/local-transfers/stop", ids(true)],
            ]
          : operation === "继续传输"
            ? [
                ["/resume", ids(false)],
                ["/local-transfers/resume", ids(true)],
              ]
            : [["/clear", actionTasks.map((task) => task.taskIndex)]];
    const issues: string[] = [];
    const summaries: string[] = [];
    for (const [path, taskIds] of requests) {
      if (!taskIds.length) continue;
      try {
        const result = await api(`/api/master${path}`, {
          method: "POST",
          body: { taskIds },
        });
        const detail = result.data || result;
        const unavailable = [
          ...new Set([
            ...(detail.unreachableWorkers || []),
            ...(detail.failedWorkers || []),
          ]),
        ];
        const operationSummary = Number.isFinite(detail.accepted)
          ? tr(
              "{value1}已受理 {value2}，忽略 {value3}，失败 {value4}{value5}",
              {
                value1: path.startsWith("/local-transfers/")
                  ? tr("本地执行项")
                  : tr("执行项"),
                value2: detail.accepted,
                value3: detail.ignored ?? 0,
                value4: detail.failed ?? 0,
                value5: unavailable.length
                  ? tr("；未响应节点：{value1}", {
                      value1: unavailable.join("、"),
                    })
                  : "",
              },
            )
          : "";
        if (
          detail.partial ||
          detail.failed?.length ||
          Number(detail.failed) > 0 ||
          unavailable.length
        )
          issues.push(
            operationSummary || detail.message || JSON.stringify(detail),
          );
        else if (operationSummary) summaries.push(operationSummary);
        const workerResults = result.details
          ? Array.isArray(result.details)
            ? result.details
            : Object.values(result.details)
          : [];
        for (const item of workerResults) {
          if (
            typeof item === "object" &&
            item &&
            ((item as any).success === false || (item as any).error)
          )
            issues.push((item as any).message || JSON.stringify(item));
          if (
            typeof item === "string" &&
            /失败|错误|拒绝|异常|不支持/.test(item)
          )
            issues.push(item);
        }
        if (/失败[:：]\s*[1-9]/.test(result.message || ""))
          issues.push(result.message);
      } catch (e) {
        issues.push((e as Error).message);
      }
    }
    setOperationOpen(false);
    if (
      !issues.length &&
      operation === "删除" &&
      actionTasks.some((task) => task.taskIndex === detailId)
    )
      setDetailId(undefined);
    await Promise.all([refresh(), detailFallback.refresh()]);
    if (!issues.length && operation === "删除")
      setHistoryTasks(
        (previous) =>
          previous?.filter(
            (task) =>
              !actionTasks.some(
                (removed) => removed.taskIndex === task.taskIndex,
              ),
          ) ?? null,
      );
    setBusy(false);
    if (issues.length) {
      setActionResult(
        tr("部分请求未完成：{value1}", { value1: issues.join("；") }),
      );
      notify(tr("部分操作未完成，详见列表上方的结果。"), "warning");
    } else {
      notify(
        tr("{value1}请求已提交{value2}。请以刷新后的任务状态为准。", {
          value1: tr(operation),
          value2: summaries.length ? `，${summaries.join("；")}` : "",
        }),
        "info",
      );
      setSelected([]);
    }
  }
  const rowRoute = (task: Task) => (
    <Stack spacing={0.5} sx={{ minWidth: 0 }}>
      <Typography
        variant="body2"
        sx={{ fontWeight: 500, overflowWrap: "anywhere" }}
      >
        {endpoint(task, true)}
      </Typography>
      <Stack direction="row" gap={0.7} alignItems="center">
        <EastRounded sx={{ fontSize: 14, color: "text.secondary" }} />
        <Typography
          variant="caption"
          color="text.secondary"
          sx={{ overflowWrap: "anywhere" }}
        >
          {endpoint(task, false)}
        </Typography>
      </Stack>
    </Stack>
  );
  const rowProgress = (task: Task) => {
    const scanned = Number(task.listObjectCount || 0);
    const handled =
      Number(task.putObjectCount || 0) + Number(task.skippedObjectCount || 0);
    const countsCurrent = handled <= scanned;
    const complete = scanFinished(task) && countsCurrent;
    const scanning = ["RUNNING", "LIST_DONE"].includes(task.status);
    return (
      <Box sx={{ minWidth: 120 }}>
        <Stack direction="row" justifyContent="space-between" gap={1}>
          <Typography variant="caption" color="text.secondary">
            {countsCurrent
              ? `${formatNumber(task.putObjectCount)} / ${formatNumber(task.listObjectCount)}`
              : tr("{value1} 已迁移", {
                  value1: formatNumber(task.putObjectCount),
                })}
          </Typography>
          <Typography variant="caption">
            {complete
              ? `${Math.floor(progress(task))}%`
              : !countsCurrent
                ? tr("统计更新中")
                : task.status === "PENDING"
                  ? tr("待扫描")
                  : scanning
                    ? tr("扫描中")
                    : tr("扫描未完成")}
          </Typography>
        </Stack>
        <LinearProgress
          variant={!complete && scanning ? "indeterminate" : "determinate"}
          value={complete ? progress(task) : 0}
          color={
            task.failedObjectCount > 0
              ? "warning"
              : task.status === "COMPLETED"
                ? "success"
                : "primary"
          }
          sx={{ height: 4, borderRadius: 2, mt: 0.7, bgcolor: "action.hover" }}
        />
      </Box>
    );
  };
  return (
    <>
      <PageHeader
        title={overview ? tr("迁移概览") : tr("迁移任务")}
        description={
          overview
            ? tr("任务统计与近期传输状态。")
            : tr("查看传输进度，启动、停止或继续任务。")
        }
        action={
          <Stack direction="row" gap={1} flexWrap="wrap" alignItems="center">
            <RefreshControls
              refreshLabel={tr("刷新任务")}
              onRefresh={() =>
                Promise.all([refresh(), health.refresh(), nodeHealth.refresh()])
              }
              loading={loading || health.loading || nodeHealth.loading}
            />
            <Button
              component={RouterLink}
              to="/create"
              variant="contained"
              startIcon={<AddRounded />}
            >
              {tr("创建迁移")}
            </Button>
          </Stack>
        }
      />
      <ErrorAlert error={error} onRetry={refresh} />
      {!overview && (data?.truncated || nextHistoryCursor) && (
        <Alert
          severity="info"
          action={
            nextHistoryCursor ? (
              <Button
                disabled={historyLoading}
                onClick={() => void loadEarlierTasks()}
              >
                {historyLoading ? tr("加载中") : tr("加载更早任务")}
              </Button>
            ) : undefined
          }
        >
          {tr("已加载 {count} 个任务。统计仅包含最近一页。", {
            count: formatNumber(tasks.length),
          })}
        </Alert>
      )}
      <ErrorAlert
        error={historyError}
        onRetry={() => void loadEarlierTasks()}
      />
      {(health.error ||
        nodeHealth.error ||
        offline.length > 0 ||
        missingNodes) && (
        <Alert severity="warning" sx={{ mb: 2 }}>
          {tr("部分节点状态不可用，任务统计可能不完整。")}
          <Button component={RouterLink} to="/nodes" size="small">
            {tr("查看节点")}
          </Button>
        </Alert>
      )}
      <Metrics data={data} />
      {overview && (
        <Paper
          variant="outlined"
          sx={{
            p: { xs: 2.5, md: 3 },
            mb: 3,
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            flexWrap: "wrap",
            gap: 3,
          }}
        >
          <Stack direction="row" alignItems="center" gap={2}>
            <Avatar
              sx={{
                width: 48,
                height: 48,
                bgcolor: "action.selected",
                color: "primary.main",
                borderRadius: "16px",
              }}
            >
              <StorageRounded />
            </Avatar>
            <Box>
              <Typography variant="h6">{tr("存储连接")}</Typography>
              <Typography
                variant="body2"
                color="text.secondary"
                sx={{ mt: 0.5 }}
              >
                {tr("管理对象存储的访问地址和凭证。")}
              </Typography>
            </Box>
          </Stack>
          <Button
            component={RouterLink}
            to="/storage"
            endIcon={<ArrowForwardRounded />}
          >
            {tr("管理存储连接")}
          </Button>
        </Paper>
      )}
      {actionResult && (
        <Alert
          severity="warning"
          onClose={() => setActionResult("")}
          sx={{ mb: 2 }}
        >
          {actionResult}
        </Alert>
      )}
      <Paper
        variant="outlined"
        sx={{ overflow: "hidden", borderRadius: "20px" }}
      >
        <Stack
          direction="row"
          alignItems="center"
          justifyContent="space-between"
          sx={{ px: { xs: 2, md: 3 }, py: 2.5 }}
        >
          <Stack direction="row" alignItems="center" gap={1.5}>
            <Typography variant="h6">
              {overview ? tr("最近任务") : tr("全部任务")}
            </Typography>
            <Chip
              size="small"
              label={formatNumber(filtered.length)}
              sx={{ height: 22, fontSize: 12 }}
            />
          </Stack>
          {overview && (
            <Button
              component={RouterLink}
              to="/tasks"
              size="small"
              endIcon={<ArrowForwardRounded />}
            >
              {tr("查看全部")}
            </Button>
          )}
        </Stack>
        {!overview && (
          <>
            <Box
              sx={{
                display: "grid",
                gridTemplateColumns: {
                  xs: "1fr 1fr",
                  sm: "repeat(2, minmax(0, 1fr))",
                  lg: "minmax(200px, 1.6fr) 1fr 1fr 1fr",
                },
                gap: 1.5,
                px: { xs: 2, md: 3 },
                pb: 2.5,
              }}
            >
              <TextField
                placeholder={tr("搜索任务、存储桶或路径")}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                sx={{ gridColumn: { xs: "1 / -1", sm: "auto" } }}
                slotProps={{
                  htmlInput: { "aria-label": tr("搜索任务") },
                  input: {
                    startAdornment: (
                      <InputAdornment position="start">
                        <SearchRounded fontSize="small" />
                      </InputAdornment>
                    ),
                  },
                }}
              />
              <TextField
                select
                label={tr("任务状态")}
                value={status}
                onChange={(e) => setStatus(e.target.value)}
              >
                <MenuItem value="">{tr("全部状态")}</MenuItem>
                {stateOptions.map((key) => (
                  <MenuItem key={key} value={key}>
                    {statusLabels[key]}
                  </MenuItem>
                ))}
              </TextField>
              <TextField
                select
                label={tr("传输方向")}
                value={direction}
                onChange={(e) => setDirection(e.target.value)}
              >
                <MenuItem value="">{tr("全部方向")}</MenuItem>
                <MenuItem value="OSS_TO_OSS">OSS → OSS</MenuItem>
                <MenuItem value="OSS_TO_LOCAL">{tr("OSS → 本地")}</MenuItem>
                <MenuItem value="LOCAL_TO_OSS">{tr("本地 → OSS")}</MenuItem>
              </TextField>
              <TextField
                select
                label={tr("执行节点")}
                value={worker}
                onChange={(e) => setWorker(e.target.value)}
                sx={{ gridColumn: { xs: "1 / -1", sm: "auto" } }}
              >
                <MenuItem value="">{tr("全部节点")}</MenuItem>
                {workerOptions.map(([address, label]) => (
                  <MenuItem key={address} value={address}>
                    {label}
                  </MenuItem>
                ))}
              </TextField>
            </Box>
            {selected.length > 0 && (
              <Stack
                direction="row"
                gap={1}
                flexWrap="wrap"
                alignItems="center"
                sx={{ p: 2, bgcolor: "action.selected" }}
              >
                <Typography variant="body2" sx={{ mr: 1 }}>
                  {tr("已选 {count} 项（最多 128 项）", {
                    count: formatNumber(selected.length),
                  })}
                </Typography>
                {filtered.some(
                  (task) => !selected.includes(task.taskIndex),
                ) && (
                  <Button
                    size="small"
                    onClick={() =>
                      setSelected(
                        filtered.slice(0, 128).map((task) => task.taskIndex),
                      )
                    }
                  >
                    {filtered.length > 128
                      ? tr("选择前 128 个结果")
                      : tr("选择全部 {value1} 个结果", {
                          value1: filtered.length,
                        })}
                  </Button>
                )}
                {[
                  ["启动", canStart, <PlayArrowRounded />],
                  ["停止", canStop, <PauseRounded />],
                  ["继续传输", canResume, <PlayArrowRounded />],
                  ["删除", canDelete, <DeleteOutlineRounded />],
                ].map(([label, enabled, icon]) => (
                  <Tooltip
                    key={String(label)}
                    title={
                      enabled
                        ? tr("{value1}选中任务", { value1: tr(String(label)) })
                        : label === "启动"
                          ? tr("请选择待启动的 OSS→OSS 任务")
                          : label === "停止"
                            ? tr("请选择待启动或运行中的任务")
                            : label === "继续传输"
                              ? tr("请选择已停止或失败的任务")
                              : tr("请选择未在运行的任务")
                    }
                  >
                    <span>
                      <Button
                        size="small"
                        startIcon={icon as any}
                        disabled={!enabled || busy}
                        onClick={() => openOperation(String(label), null)}
                      >
                        {tr(String(label))}
                      </Button>
                    </span>
                  </Tooltip>
                ))}
                <Button
                  size="small"
                  startIcon={<DownloadRounded />}
                  onClick={() =>
                    saveFile(
                      JSON.stringify(chosen, null, 2),
                      "transfer-tasks.json",
                    )
                  }
                >
                  {tr("导出 JSON")}
                </Button>
                <Button size="small" onClick={() => setSelected([])}>
                  {tr("取消选择")}
                </Button>
              </Stack>
            )}
          </>
        )}
        {loading && !data ? (
          <LoadingState />
        ) : !shown.length ? (
          <EmptyState
            title={
              query || status || direction || worker
                ? tr("没有匹配的任务")
                : error
                  ? tr("任务加载失败")
                  : tr("暂无迁移任务")
            }
            description={
              query || status || direction || worker
                ? tr("请调整筛选条件或搜索关键词。")
                : error
                  ? tr("请检查连接后重试。")
                  : tr("配置存储连接后创建迁移任务。")
            }
            action={
              !query && !status && !direction && !worker && !error ? (
                <Button
                  component={RouterLink}
                  to="/create"
                  variant="contained"
                  startIcon={<AddRounded />}
                >
                  {tr("创建迁移任务")}
                </Button>
              ) : undefined
            }
          />
        ) : (
          <>
            <TableContainer sx={{ display: { xs: "none", md: "block" } }}>
              <Table
                size="small"
                sx={{ tableLayout: "fixed", minWidth: 1080 }}
                aria-label={tr("迁移任务列表")}
              >
                <TableHead>
                  <TableRow>
                    {!overview && (
                      <TableCell padding="checkbox" sx={{ width: 48 }}>
                        <Checkbox
                          size="small"
                          checked={
                            shown.length > 0 &&
                            shown.every((t) => selected.includes(t.taskIndex))
                          }
                          indeterminate={
                            shown.some((t) => selected.includes(t.taskIndex)) &&
                            !shown.every((t) => selected.includes(t.taskIndex))
                          }
                          slotProps={{
                            input: { "aria-label": tr("选择当前页任务") },
                          }}
                          onChange={(_, checked) =>
                            setSelected((s) =>
                              checked
                                ? [
                                    ...new Set([
                                      ...s,
                                      ...shown.map((t) => t.taskIndex),
                                    ]),
                                  ].slice(0, 128)
                                : s.filter(
                                    (id) =>
                                      !shown.some((t) => t.taskIndex === id),
                                  ),
                            )
                          }
                        />
                      </TableCell>
                    )}
                    <TableCell sx={{ width: 100 }}>{tr("任务")}</TableCell>
                    <TableCell sx={{ width: "30%" }}>
                      {tr("来源 / 目标")}
                    </TableCell>
                    <TableCell sx={{ width: "21%" }}>
                      {tr("已迁移 / 已扫描")}
                    </TableCell>
                    <TableCell sx={{ width: 112 }}>{tr("状态")}</TableCell>
                    <TableCell sx={{ width: 128 }}>{tr("执行节点")}</TableCell>
                    <TableCell align="right" sx={{ width: 75 }}>
                      {tr("失败")}
                    </TableCell>
                    <TableCell sx={{ width: 65 }} />
                  </TableRow>
                </TableHead>
                <TableBody>
                  {shown.map((task) => (
                    <TableRow
                      key={task.taskIndex}
                      hover
                      selected={selected.includes(task.taskIndex)}
                    >
                      {!overview && (
                        <TableCell padding="checkbox">
                          <Checkbox
                            size="small"
                            checked={selected.includes(task.taskIndex)}
                            disabled={
                              selected.length >= 128 &&
                              !selected.includes(task.taskIndex)
                            }
                            onChange={() => toggle(task.taskIndex)}
                            slotProps={{
                              input: {
                                "aria-label": tr("选择任务 {value1}", {
                                  value1: task.taskIndex,
                                }),
                              },
                            }}
                          />
                        </TableCell>
                      )}
                      <TableCell>
                        <Button
                          size="small"
                          onClick={() => setDetailId(task.taskIndex)}
                          sx={{ minWidth: 0, px: 0 }}
                        >
                          #{task.taskIndex}
                        </Button>
                        <Typography
                          variant="caption"
                          display="block"
                          color="text.secondary"
                          sx={{ fontSize: 10, whiteSpace: "nowrap" }}
                        >
                          {directionLabel(task)}
                        </Typography>
                      </TableCell>
                      <TableCell>{rowRoute(task)}</TableCell>
                      <TableCell>{rowProgress(task)}</TableCell>
                      <TableCell>
                        <TaskStatus task={task} />
                      </TableCell>
                      <TableCell>
                        <Typography
                          variant="caption"
                          sx={{
                            overflowWrap: "anywhere",
                            fontFamily: "monospace",
                          }}
                        >
                          {workers(task)}
                        </Typography>
                        <Typography
                          variant="caption"
                          display="block"
                          color="text.secondary"
                        >
                          {task.duration || "—"}
                        </Typography>
                      </TableCell>
                      <TableCell align="right">
                        {task.failedObjectCount > 0 ? (
                          <Button
                            size="small"
                            color="warning"
                            component={RouterLink}
                            to={`/exceptions?taskId=${task.taskIndex}&type=${isLocal(task) ? "local" : "oss"}`}
                            sx={{ px: 0, minWidth: 0 }}
                          >
                            {formatNumber(task.failedObjectCount)}
                          </Button>
                        ) : (
                          <Typography variant="body2" color="text.secondary">
                            0
                          </Typography>
                        )}
                      </TableCell>
                      <TableCell>
                        <Tooltip title={tr("查看详情")}>
                          <IconButton
                            size="small"
                            aria-label={tr("查看任务 {value1}", {
                              value1: task.taskIndex,
                            })}
                            onClick={() => setDetailId(task.taskIndex)}
                          >
                            <ArrowForwardRounded sx={{ fontSize: 18 }} />
                          </IconButton>
                        </Tooltip>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
            <Stack
              sx={{ display: { xs: "flex", md: "none" } }}
              divider={<Divider />}
            >
              {shown.map((task) => (
                <Box key={task.taskIndex} sx={{ p: 2 }}>
                  <Stack
                    direction="row"
                    justifyContent="space-between"
                    alignItems="center"
                    sx={{ mb: 1.5 }}
                  >
                    <Stack direction="row" alignItems="center">
                      {!overview && (
                        <Checkbox
                          checked={selected.includes(task.taskIndex)}
                          disabled={
                            selected.length >= 128 &&
                            !selected.includes(task.taskIndex)
                          }
                          onChange={() => toggle(task.taskIndex)}
                          slotProps={{
                            input: {
                              "aria-label": tr("选择任务 {value1}", {
                                value1: task.taskIndex,
                              }),
                            },
                          }}
                          size="small"
                          sx={{ ml: -1 }}
                        />
                      )}
                      <Button
                        size="small"
                        onClick={() => setDetailId(task.taskIndex)}
                      >
                        #{task.taskIndex}
                      </Button>
                      <Typography variant="caption" color="text.secondary">
                        {directionLabel(task)}
                      </Typography>
                    </Stack>
                    <TaskStatus task={task} />
                  </Stack>
                  {rowRoute(task)}
                  <Box sx={{ my: 2 }}>{rowProgress(task)}</Box>
                  <Stack
                    direction="row"
                    justifyContent="space-between"
                    alignItems="center"
                  >
                    <Typography variant="caption" color="text.secondary">
                      {workers(task)} · {task.duration || "—"}
                    </Typography>
                    <Button
                      size="small"
                      onClick={() => setDetailId(task.taskIndex)}
                    >
                      {tr("查看详情")}
                    </Button>
                  </Stack>
                </Box>
              ))}
            </Stack>
          </>
        )}
        {!overview && filtered.length > 0 && (
          <TablePagination
            component="div"
            count={filtered.length}
            page={page}
            onPageChange={(_, p) => setPage(p)}
            rowsPerPage={pageSize}
            onRowsPerPageChange={(e) => {
              setPageSize(Number(e.target.value));
              setPage(0);
            }}
            rowsPerPageOptions={[10, 25, 50]}
            labelRowsPerPage={tr("每页")}
            labelDisplayedRows={({ from, to, count }) =>
              `${from}–${to} / ${count}`
            }
            getItemAriaLabel={(type) =>
              type === "next" ? tr("下一页") : tr("上一页")
            }
            sx={{
              borderTop: "1px solid",
              borderColor: "divider",
              ".MuiTablePagination-toolbar": {
                flexWrap: "wrap",
                justifyContent: "flex-end",
              },
            }}
          />
        )}
      </Paper>
      <Stack
        direction="row"
        justifyContent="space-between"
        sx={{ mt: 2, px: 0.5 }}
      >
        <Typography variant="caption" color="text.secondary">
          {overview
            ? tr("最近创建的 5 个任务")
            : tr("共 {value1} 个任务", {
                value1: formatNumber(filtered.length),
              })}{" "}
          · {refreshDescription(refreshSeconds)}
        </Typography>
        <Typography
          variant="caption"
          color="text.secondary"
          sx={{ display: { xs: "none", sm: "block" } }}
        >
          {tr("进度包含成功与跳过对象；扫描中总量可能变化")}
        </Typography>
      </Stack>
      <TaskDetail
        task={detailTask}
        taskId={detailId}
        childId={childId}
        childNode={childNode}
        onSelectChild={setChildId}
        loadError={linkedTask ? error : detailFallback.error}
        loadPartial={
          !!data?.partial && !data?.truncated && !detailFallback.data
        }
        loading={linkedTask ? loading : detailFallback.loading}
        busy={busy}
        onRefresh={() =>
          void Promise.all([refresh(), detailFallback.refresh()])
        }
        operationError={actionResult}
        onAction={(task, next) => openOperation(next, [task.taskIndex])}
        onClose={() => setDetailId(undefined)}
      />
      <Dialog
        open={operationOpen}
        onClose={() => !busy && setOperationOpen(false)}
        slotProps={{
          transition: {
            onExited: () => {
              setOperation("");
              setOperationIds(null);
              setConfirmationTasks([]);
            },
          },
        }}
        fullWidth
        maxWidth="xs"
      >
        <DialogTitle>
          {tr("{operation} {count} 个任务？", {
            operation: tr(operation),
            count: formatNumber(confirmationTasks.length),
          })}
        </DialogTitle>
        <DialogContent>
          <Typography variant="body2" color="text.secondary">
            {operation === "删除"
              ? tr("任务记录将移至回收站，不会删除源或目标存储中的对象。")
              : operation === "停止"
                ? tr("停止扫描和传输，保留已完成的对象与进度。")
                : operation === "继续传输"
                  ? tr("继续扫描和处理未完成的对象，保留已有传输进度。")
                  : tr("任务将开始传输，请确认来源和目标配置。")}
          </Typography>
          <Typography variant="body2" sx={{ mt: 2 }}>
            {tr("任务：")}
            {confirmationTasks.map((task) => `#${task.taskIndex}`).join("、")}
          </Typography>
        </DialogContent>
        <DialogActions sx={{ p: 3 }}>
          <Button onClick={() => setOperationOpen(false)} disabled={busy}>
            {tr("取消")}
          </Button>
          <Button
            variant="contained"
            color={operation === "删除" ? "error" : "primary"}
            onClick={execute}
            disabled={busy}
          >
            {busy
              ? tr("提交中…")
              : tr("确认{value1}", { value1: tr(operation) })}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}
