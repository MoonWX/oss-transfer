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

type Props = {
  childId: string;
  task?: Record<string, any>;
  direction: string;
  source: string;
  destination: string;
  loading: boolean;
  partial: boolean;
  error?: string | null;
  onBack: () => void;
  onClose: () => void;
  onRefresh: () => void;
};

const timestamp = (value: unknown) =>
  typeof value === "number" ? new Date(value).toLocaleString("zh-CN") : value;
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
  onBack,
  onClose,
  onRefresh,
}: Props) {
  const sourceEndpoint = task?.sourceEndpoint;
  const rows = task
    ? [
        ["执行节点", task.executionNodeIp || task.workerNode || "等待分配"],
        ["当前速度", speed(task.currentSpeedBytesPerSecond)],
        ["平均速度", speed(task.averageSpeedBytesPerSecond)],
        ["创建时间", timestamp(task.createTime)],
        ["开始时间", timestamp(task.startTime)],
        ["结束时间", timestamp(task.endTime)],
        ["耗时", task.duration],
        ["起始 Key", task.startKey],
        ["结束 Key", task.endKey],
        ["源对象 Key", task.objectKey || sourceEndpoint?.objectKey],
        [
          "源前缀",
          task.prefix ||
            (sourceEndpoint?.prefixFilterEnabled
              ? sourceEndpoint.prefix
              : null),
        ],
        ["目标相对路径", task.targetRelativePath],
        [
          "目标命名",
          task.keyMapping
            ? keyMappingLabels[task.keyMapping.mode as OssKeyMappingMode]
            : null,
        ],
        ["目标前缀", task.keyMapping?.targetPrefix],
        ["目标对象 Key", task.keyMapping?.targetKey],
        ["修改时间晚于", task.dateAfter],
        ["修改时间早于", task.dateBefore],
        [
          "冲突策略",
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
          返回主任务
        </Button>
        <IconButton aria-label="关闭详情" onClick={onClose}>
          <CloseRounded />
        </IconButton>
      </Stack>
      <Typography
        variant="h5"
        tabIndex={-1}
        data-subtask-heading
        sx={{ mb: 2, outline: "none", overflowWrap: "anywhere" }}
      >
        子任务 #{childId}
      </Typography>
      {error && (
        <Alert
          severity="error"
          sx={{ mb: 2 }}
          action={
            <Button color="inherit" size="small" onClick={onRefresh}>
              刷新
            </Button>
          }
        >
          {error}
        </Alert>
      )}
      {partial && (
        <Alert severity="warning" sx={{ mb: 2 }}>
          子任务信息不完整，请刷新或检查执行节点。
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
                刷新
              </Button>
            }
          >
            {partial || error
              ? "当前节点未返回此子任务，请刷新后重试。"
              : "未找到此子任务。"}
          </Alert>
        )
      ) : (
        <>
          <Stack direction="row" gap={1} flexWrap="wrap" sx={{ mb: 3 }}>
            <StatusChip status={task.status} />
            <Chip variant="outlined" size="small" label={direction} />
          </Stack>
          {(task.errorMessage || task.errorCode) && (
            <Alert severity="error" sx={{ mb: 3, overflowWrap: "anywhere" }}>
              {task.errorMessage || task.errorCode}
            </Alert>
          )}
          <Paper variant="outlined" sx={{ p: 2.5, mb: 3 }}>
            <Typography variant="caption" color="text.secondary">
              来源
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
              目标
            </Typography>
            <Typography sx={{ overflowWrap: "anywhere", mt: 0.5 }}>
              {destination}
            </Typography>
          </Paper>
          <Typography variant="h6" sx={{ mb: 2 }}>
            传输统计
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
              ["已扫描", task.listObjectCount, task.listObjectSize],
              ["已迁移", task.putObjectCount, task.putObjectSize],
              ["已跳过", task.skippedObjectCount, task.skippedObjectSize],
              ["失败对象", task.failedObjectCount, task.failedObjectSize],
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
          <Divider sx={{ mb: 3 }} />
          <Typography variant="h6" sx={{ mb: 2 }}>
            执行信息
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
