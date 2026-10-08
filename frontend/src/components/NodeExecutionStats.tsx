import { Box, IconButton, Tooltip, Typography } from "@mui/material";
import InfoOutlinedIcon from "@mui/icons-material/InfoOutlined";
import { formatNumber } from "./Common";
import type { Worker } from "../lib/rateStatus";

export default function NodeExecutionStats({ worker }: { worker: Worker }) {
  const current =
    worker.executionTaskStats?.scope === "CURRENT_EXECUTION_SUBTASKS"
      ? worker.executionTaskStats
      : null;
  const legacy = worker.taskStats;
  const active =
    current &&
    typeof current.runningTasks === "number" &&
    typeof current.listDoneTasks === "number"
      ? current.runningTasks + current.listDoneTasks
      : undefined;
  const metrics = current
    ? [
        { label: "进行中子任务", value: active },
        { label: "待执行子任务", value: current.pendingTasks },
        { label: "已完成子任务", value: current.completedTasks },
      ]
    : [
        { label: "执行中记录", value: legacy?.runningTasks },
        { label: "待执行记录", value: legacy?.pendingTasks },
        { label: "已完成记录", value: legacy?.completedTasks },
      ];
  const detail = current
    ? `按本节点当前保留的执行子任务计数，不含主任务及扫描调度记录；移入回收站后不再计数。进行中包含扫描完成后仍在传输的子任务，已完成不是主任务累计完成数。已停止 ${formatNumber(current.stoppedTasks)} · 部分失败 ${formatNumber(current.partialFailedTasks)} · 失败 ${formatNumber(current.failedTasks)}${current.unknownTasks > 0 ? ` · 状态未知 ${formatNumber(current.unknownTasks)}` : ""}。`
    : "该节点仅返回旧版任务记录数量，不能据此判断主任务完成数量。升级节点服务后可查看子任务统计。";
  return (
    <Box sx={{ px: 2.5, pb: 2.5 }}>
      <Box sx={{ display: "flex", alignItems: "center", gap: 0.5, mb: 1.5 }}>
        <Typography variant="subtitle2">
          {current ? "子任务执行统计" : "节点记录（旧版统计）"}
        </Typography>
        <Tooltip title={detail} describeChild arrow>
          <IconButton
            size="small"
            aria-label={current ? "查看子任务统计口径" : "查看旧版统计口径"}
          >
            <InfoOutlinedIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      </Box>
      <Box
        component="dl"
        sx={{
          m: 0,
          display: "grid",
          gridTemplateColumns: "repeat(3, minmax(0, 1fr))",
          gap: 2,
        }}
      >
        {metrics.map(({ label, value }) => (
          <Box key={label}>
            <Typography component="dd" variant="h5" sx={{ m: 0 }}>
              {typeof value === "number" && Number.isFinite(value)
                ? formatNumber(value)
                : "—"}
            </Typography>
            <Typography component="dt" variant="body2" color="text.secondary">
              {label}
            </Typography>
          </Box>
        ))}
      </Box>
      {current ? (
        <Typography variant="body2" color="text.secondary" sx={{ mt: 1.5 }}>
          当前保留 {formatNumber(current.totalTasks)} 个子任务
        </Typography>
      ) : (
        <Typography
          variant="caption"
          color="text.secondary"
          component="p"
          sx={{ mt: 1.5, mb: 0 }}
        >
          旧版记录，非主任务完成数
        </Typography>
      )}
    </Box>
  );
}
