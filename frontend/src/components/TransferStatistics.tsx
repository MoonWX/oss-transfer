import { Box, Typography } from "@mui/material";
import { t, useI18n } from "../lib/i18n";
import { transferredBytes } from "../lib/taskProgress";
import { formatBytes, formatNumber } from "./Common";

// Shared by parent and child details. "已迁移" bytes are the data actually
// moved, including partly copied objects; the count is completed objects.
export default function TransferStatistics({
  task,
}: {
  task: Record<string, any>;
}) {
  useI18n();
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
        ],
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
  );
}
