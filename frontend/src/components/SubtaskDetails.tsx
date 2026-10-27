import { t, useI18n, formatDate } from "../lib/i18n";
import { taskRequestLimitRows } from "../lib/taskRequestLimits";
import {
  Alert,
  Box,
  Button,
  Chip,
  Divider,
  IconButton,
  Paper,
  Stack,
  Typography,
} from "@mui/material";
import {
  ArrowBackRounded,
  CloseRounded,
  EastRounded,
} from "@mui/icons-material";
import { formatBytes, formatNumber, LoadingState, StatusChip } from "./Common";
import { keyMappingLabels, type OssKeyMappingMode } from "../lib/ossKeyMapping";
import ChildRecovery from "./ChildRecovery";
import LiveFileProgress from "./LiveFileProgress";
import { useNodeLabels } from "../lib/workerStatus";
import { taskNodeLabels } from "../lib/nodeRegistry";
type Props = {
  childId: string;
  task?: Record<string, any>;
  direction: string;
  source: string;
  destination: string;
  loading: boolean;
  partial: boolean;
  error?: string | null;
  canRecover?: boolean;
  refreshMs?: number;
  liveProgressEnabled?: boolean;
  onBack: () => void;
  onClose: () => void;
  onRefresh: () => void;
};
// Only timestamp fields call this helper. Keep unrecognized legacy values intact.
const timestamp = (value: unknown) =>
  typeof value === "number" ||
  (typeof value === "string" &&
    /^\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?)?$/.test(
      value,
    ))
    ? formatDate(value)
    : value;
const speed = (value: unknown) =>
  value == null ? null : `${formatBytes(value)}/s`;
export default function SubtaskDetails({
  childId,
  task,
  direction,
  source,
  destination,
  loading,
  partial,
  error,
  canRecover = false,
  refreshMs = 0,
  liveProgressEnabled = true,
  onBack,
  onClose,
  onRefresh,
}: Props) {
  useI18n();
  const nodeLabel = useNodeLabels();
  const sourceEndpoint = task?.sourceEndpoint;
  const interrupted =
    task?.status === "RECOVERABLE" && task.errorMessage === "context canceled";
  const rows = task
    ? [
        [t("执行节点"), taskNodeLabels(task, nodeLabel)],
        ...taskRequestLimitRows(task.requestLimits),
        [t("当前速度"), speed(task.currentSpeedBytesPerSecond)],
        [t("平均速度"), speed(task.averageSpeedBytesPerSecond)],
        [t("创建时间"), timestamp(task.createTime)],
        [t("开始时间"), timestamp(task.startTime)],
        [t("结束时间"), timestamp(task.endTime)],
        [t("耗时"), task.duration],
        [t("起始 Key"), task.startKey],
        [t("结束 Key"), task.endKey],
        [t("源对象 Key"), task.objectKey || sourceEndpoint?.objectKey],
        [
          t("源前缀"),
          task.prefix ||
            (sourceEndpoint?.prefixFilterEnabled
              ? sourceEndpoint.prefix
              : null),
        ],
        [t("目标相对路径"), task.targetRelativePath],
        [
          t("目标命名"),
          task.keyMapping
            ? keyMappingLabels[task.keyMapping.mode as OssKeyMappingMode]
            : null,
        ],
        [t("目标前缀"), task.keyMapping?.targetPrefix],
        [t("目标对象 Key"), task.keyMapping?.targetKey],
        [t("修改时间晚于"), timestamp(task.dateAfter)],
        [t("修改时间早于"), timestamp(task.dateBefore)],
        [
          t("冲突策略"),
          task.conflictPolicy ||
            (task.overwrite == null
              ? null
              : task.overwrite
                ? "OVERWRITE"
                : "SKIP"),
        ],
      ].filter(([, value]) => value != null && value !== "")
    : [];
  return (
    <Box>
      <Stack
        direction="row"
        justifyContent="space-between"
        alignItems="center"
        sx={{ mb: 2 }}
      >
        <Button
          startIcon={<ArrowBackRounded />}
          onClick={onBack}
          sx={{ ml: -1 }}
        >
          {t("返回主任务")}
        </Button>
        <IconButton aria-label={t("关闭详情")} onClick={onClose}>
          <CloseRounded />
        </IconButton>
      </Stack>
      <Typography
        variant="h5"
        tabIndex={-1}
        data-subtask-heading
        sx={{ mb: 2, outline: "none", overflowWrap: "anywhere" }}
      >
        {t("子任务 #{id}", { id: childId })}
      </Typography>
      {error && (
        <Alert
          severity="error"
          sx={{ mb: 2 }}
          action={
            <Button color="inherit" size="small" onClick={onRefresh}>
              {t("刷新")}
            </Button>
          }
        >
          {error}
        </Alert>
      )}
      {partial && (
        <Alert severity="warning" sx={{ mb: 2 }}>
          {t("子任务信息不完整，请刷新或检查执行节点。")}
        </Alert>
      )}
      {!task ? (
        loading ? (
          <LoadingState />
        ) : (
          <Alert
            severity="info"
            action={
              <Button color="inherit" size="small" onClick={onRefresh}>
                {t("刷新")}
              </Button>
            }
          >
            {partial || error
              ? t("当前节点未返回此子任务，请刷新后重试。")
              : t("未找到此子任务。")}
          </Alert>
        )
      ) : (
        <>
          <Stack direction="row" gap={1} flexWrap="wrap" sx={{ mb: 3 }}>
            <StatusChip status={task.status} />
            <Chip variant="outlined" size="small" label={direction} />
          </Stack>
          {canRecover && task.status === "RECOVERABLE" && (
            <ChildRecovery
              key={childId}
              childId={childId}
              attempt={task.attempt}
              observedAt={task.observedAt}
              onRefresh={onRefresh}
            />
          )}
          {(task.errorMessage || task.errorCode) && (
            <Alert
              severity={interrupted ? "warning" : "error"}
              sx={{ mb: 3, overflowWrap: "anywhere" }}
            >
              {interrupted
                ? t("传输已中断，等待恢复。")
                : task.errorMessage || task.errorCode}
            </Alert>
          )}
          <Paper variant="outlined" sx={{ p: 2.5, mb: 3 }}>
            <Typography variant="caption" color="text.secondary">
              {t("来源")}
            </Typography>
            <Typography sx={{ overflowWrap: "anywhere", mt: 0.5 }}>
              {source}
            </Typography>
            <EastRounded sx={{ my: 1.5, color: "text.secondary" }} />
            <Typography
              variant="caption"
              color="text.secondary"
              display="block"
            >
              {t("目标")}
            </Typography>
            <Typography sx={{ overflowWrap: "anywhere", mt: 0.5 }}>
              {destination}
            </Typography>
          </Paper>
          <Typography variant="h6" sx={{ mb: 2 }}>
            {t("传输统计")}
          </Typography>
          <Box
            sx={{
              display: "grid",
              gridTemplateColumns: "repeat(2, minmax(0, 1fr))",
              gap: 2.5,
              mb: 3,
            }}
          >
            {[
              [t("已扫描"), task.listObjectCount, task.listObjectSize],
              [t("已迁移"), task.putObjectCount, task.putObjectSize],
              [t("已跳过"), task.skippedObjectCount, task.skippedObjectSize],
              [t("失败对象"), task.failedObjectCount, task.failedObjectSize],
            ].map(([label, count, bytes]) => (
              <Box key={label}>
                <Typography variant="caption" color="text.secondary">
                  {label}
                </Typography>
                <Typography sx={{ fontWeight: 500 }}>
                  {count == null ? "—" : formatNumber(count)}{" "}
                  <Box
                    component="span"
                    sx={{ fontSize: 12, color: "text.secondary" }}
                  >
                    / {bytes == null ? "—" : formatBytes(bytes)}
                  </Box>
                </Typography>
              </Box>
            ))}
          </Box>
          <LiveFileProgress
            taskId={Number(childId.split("-")[0])}
            childNumber={Number(childId.split("-")[1])}
            generation={task.generation}
            status={task.status}
            refreshMs={refreshMs}
            enabled={liveProgressEnabled}
          />
          <Divider sx={{ mb: 3 }} />
          <Typography variant="h6" sx={{ mb: 2 }}>
            {t("执行信息")}
          </Typography>
          <Stack gap={2}>
            {rows.map(([label, value]) => (
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
        </>
      )}
    </Box>
  );
}
