import { t, useI18n, getLocale } from "../lib/i18n";
import type { ReactNode } from "react";
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Stack,
  Typography,
} from "@mui/material";
import InboxOutlined from "@mui/icons-material/InboxOutlined";
import { radii } from "../theme";

export const formatNumber = (n: unknown) =>
  Number(n || 0).toLocaleString(getLocale());
export function formatBytes(value: unknown) {
  const n = Number(value || 0);
  if (!Number.isFinite(n) || n <= 0) return "0 B";
  const index = Math.min(Math.floor(Math.log(n) / Math.log(1024)), 5);
  return `${(n / 1024 ** index).toLocaleString(getLocale(), { maximumFractionDigits: index ? 1 : 0 })} ${["B", "KiB", "MiB", "GiB", "TiB", "PiB"][index]}`;
}
export const statusLabels: Record<string, string> = {
  get PENDING() {
    return t("排队中");
  },
  get WAITING() {
    return t("排队中");
  },
  get RUNNING() {
    return t("执行中");
  },
  get STOPPING() {
    return t("停止中");
  },
  get RESUMING() {
    return t("恢复中");
  },
  get RECOVERABLE() {
    return t("等待恢复");
  },
  get LIST_DONE() {
    return t("执行中");
  },
  get COMPLETED() {
    return t("已完成");
  },
  get PARTIAL_FAILED() {
    return t("部分失败");
  },
  get FAILED() {
    return t("失败");
  },
  get STOPPED() {
    return t("已停止");
  },
};
export function StatusChip({ status }: { status?: string }) {
  useI18n();
  const colors: Record<
    string,
    "default" | "info" | "success" | "warning" | "error"
  > = {
    RUNNING: "info",
    STOPPING: "warning",
    RESUMING: "info",
    RECOVERABLE: "warning",
    LIST_DONE: "info",
    COMPLETED: "success",
    PARTIAL_FAILED: "warning",
    FAILED: "error",
    STOPPED: "default",
  };
  return (
    <Chip
      label={statusLabels[status || ""] || status || t("未知")}
      size="small"
      color={colors[status || ""] || "default"}
      variant="outlined"
      sx={{ fontWeight: 500, fontSize: 12, height: 26 }}
    />
  );
}
export function PageHeader({
  title,
  description,
  action,
}: {
  title: string;
  description?: string;
  action?: ReactNode;
}) {
  return (
    <Stack
      direction={{ xs: "column", sm: "row" }}
      justifyContent="space-between"
      alignItems={{ xs: "stretch", sm: "center" }}
      gap={2.5}
      sx={{ mb: 3.5 }}
    >
      <Box>
        <Typography variant="h4" component="h1">
          {title}
        </Typography>
        {description && (
          <Typography color="text.secondary" sx={{ mt: 1, fontSize: 14 }}>
            {description}
          </Typography>
        )}
      </Box>
      {action}
    </Stack>
  );
}
export function EmptyState({
  title,
  description,
  action,
}: {
  title: string;
  description?: string;
  action?: ReactNode;
}) {
  return (
    <Stack
      alignItems="center"
      spacing={1.5}
      sx={{ py: 7, px: 3, textAlign: "center" }}
    >
      <Box
        sx={{
          color: "primary.main",
          bgcolor: "action.selected",
          borderRadius: radii.surface,
          p: 2,
          mb: 1,
        }}
      >
        <InboxOutlined sx={{ fontSize: 36 }} />
      </Box>
      <Typography variant="h6">{title}</Typography>
      {description && (
        <Typography
          variant="body2"
          color="text.secondary"
          sx={{ maxWidth: 430, lineHeight: 1.8 }}
        >
          {description}
        </Typography>
      )}
      {action && <Box sx={{ pt: 1 }}>{action}</Box>}
    </Stack>
  );
}
export function ErrorAlert({
  error,
  onRetry,
}: {
  error: string | null;
  onRetry?: () => void;
}) {
  useI18n();
  return error ? (
    <Alert
      severity="error"
      sx={{ mb: 2 }}
      action={
        onRetry ? (
          <Button color="inherit" size="small" onClick={onRetry}>
            {t("重试")}
          </Button>
        ) : undefined
      }
    >
      {error}
    </Alert>
  ) : null;
}
export function LoadingState() {
  useI18n();
  return (
    <Stack
      direction="row"
      spacing={2}
      alignItems="center"
      justifyContent="center"
      sx={{ p: 8 }}
    >
      <CircularProgress size={24} />
      <Typography color="text.secondary">{t("正在加载…")}</Typography>
    </Stack>
  );
}
