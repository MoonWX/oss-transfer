import { Box, Typography } from "@mui/material";
import { t, useI18n } from "../lib/i18n";
import { taskRequestRateText, transferredBytes } from "../lib/taskProgress";
import { formatBytes, formatNumber } from "./Common";

// Shared by parent and child details. "已迁移" bytes include fresh in-flight
// bytes from the live sample; the object count stays completed objects only.
export default function TransferStatistics({
  task,
}: {
  task: Record<string, any>;
}) {
  useI18n();
  const inFlight = Number(task.inFlightBytes || 0);
  const requests = taskRequestRateText(task.requestRates);
  return (
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
        [
          t("已迁移"),
          task.putObjectCount,
          task.putObjectSize == null && task.transferredBytes == null
            ? null
            : transferredBytes(task),
          inFlight > 0
            ? t("含传输中 {value1}", { value1: formatBytes(inFlight) })
            : null,
        ],
        [t("已跳过"), task.skippedObjectCount, task.skippedObjectSize],
        [t("失败对象"), task.failedObjectCount, task.failedObjectSize],
      ].map(([label, count, bytes, hint]) => (
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
          {hint && (
            <Typography
              variant="caption"
              color="text.secondary"
              data-testid="transfer-in-flight"
            >
              {hint}
            </Typography>
          )}
        </Box>
      ))}
      {requests && (
        <Box sx={{ gridColumn: "1 / -1" }} data-testid="task-request-rates">
          <Typography variant="caption" color="text.secondary">
            {t("请求速率")}
          </Typography>
          <Typography sx={{ fontWeight: 500 }}>{requests.value}</Typography>
          {requests.detail.map((line) => (
            <Typography
              key={line}
              variant="caption"
              color="text.secondary"
              display="block"
            >
              {line}
            </Typography>
          ))}
        </Box>
      )}
    </Box>
  );
}
