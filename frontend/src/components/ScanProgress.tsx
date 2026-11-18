import { Box, LinearProgress, Stack, Tooltip, Typography } from "@mui/material";
import { t, useI18n } from "../lib/i18n";
import { scanActivityView, type ScanActivity } from "../lib/scanActivity";
import { radii } from "../theme";

const numbers = { fontVariantNumeric: "tabular-nums" } as const;

// What the scanning node reports about a running scan, shown while a running
// task's scan is incomplete and Master has heard from its scanner. Nothing is
// shown without a report: an absent report must not read as a scan that has
// done nothing. The counters use the layout of the transfer statistics above
// it; the directory being worked through is the one moving part.
export default function ScanProgress({
  activity,
  compact = false,
}: {
  activity?: ScanActivity | null;
  compact?: boolean;
}) {
  useI18n();
  const view = scanActivityView(activity);
  if (!view) return null;
  const title = (
    <Typography
      variant={compact ? "caption" : "h6"}
      component="div"
      color={compact ? "text.secondary" : undefined}
      sx={{ fontWeight: compact ? 500 : undefined }}
    >
      {t("扫描进度")}
    </Typography>
  );
  if (!view.supported)
    return (
      <Box sx={{ mb: compact ? 1 : 3 }} data-testid="scan-progress">
        <Box sx={{ mb: compact ? 0.25 : 1 }}>{title}</Box>
        <Typography
          variant={compact ? "caption" : "body2"}
          color="text.secondary"
          component="div"
        >
          {view.message}
        </Typography>
      </Box>
    );
  const valueColor = view.stale ? "text.secondary" : "text.primary";
  const directory = view.directory;
  return (
    <Box sx={{ mb: compact ? 1 : 3 }} data-testid="scan-progress">
      <Stack
        direction="row"
        justifyContent="space-between"
        alignItems="baseline"
        gap={2}
        sx={{ mb: compact ? 0.5 : 1.5 }}
      >
        {title}
        {view.asOf && (
          <Typography
            variant="caption"
            color="warning.main"
            data-testid="scan-progress-as-of"
          >
            {view.asOf}
          </Typography>
        )}
      </Stack>
      <Box
        sx={{
          display: "grid",
          gridTemplateColumns: "repeat(2, minmax(0, 1fr))",
          gap: compact ? 1.5 : 2.5,
        }}
      >
        {[
          [t("已读取名称"), view.namesRead],
          [t("已检查条目"), view.examined],
        ].map(([label, value]) => (
          <Box key={label}>
            <Typography
              variant="caption"
              color="text.secondary"
              component="div"
            >
              {label}
            </Typography>
            <Typography
              variant={compact ? "body2" : "body1"}
              color={valueColor}
              sx={{ fontWeight: 500, ...numbers }}
            >
              {value}
            </Typography>
          </Box>
        ))}
      </Box>
      {directory && (
        <Box sx={{ mt: compact ? 1 : 2 }} data-testid="scan-progress-directory">
          <Stack
            direction="row"
            justifyContent="space-between"
            alignItems="baseline"
            gap={2}
          >
            <Typography variant="caption" color="text.secondary">
              {directory.label}
            </Typography>
            <Typography
              variant="caption"
              color="text.secondary"
              sx={{ whiteSpace: "nowrap", ...numbers }}
            >
              {directory.detail}
            </Typography>
          </Stack>
          <Tooltip title={directory.path}>
            <Typography
              variant={compact ? "caption" : "body2"}
              color={valueColor}
              component="div"
              noWrap
              sx={{ mt: 0.25, mb: 1 }}
            >
              {directory.path}
            </Typography>
          </Tooltip>
          <LinearProgress
            variant={
              directory.percent == null ? "indeterminate" : "determinate"
            }
            value={directory.percent ?? 0}
            color={view.stale ? "inherit" : "primary"}
            sx={{
              height: 4,
              borderRadius: radii.pill,
              bgcolor: "action.hover",
              color: view.stale ? "text.disabled" : undefined,
            }}
          />
          {directory.note && (
            <Typography
              variant="caption"
              color="text.secondary"
              component="div"
              sx={{ mt: 1 }}
            >
              {directory.note}
            </Typography>
          )}
        </Box>
      )}
      {view.alerts.map((alert) => (
        <Stack
          key={alert.text}
          direction="row"
          alignItems="center"
          gap={0.75}
          sx={{ mt: 1 }}
        >
          <Box
            sx={{
              width: 6,
              height: 6,
              borderRadius: "50%",
              flex: "none",
              bgcolor: alert.warning ? "warning.main" : "text.disabled",
            }}
          />
          <Typography
            variant={compact ? "caption" : "body2"}
            color={alert.warning ? "warning.main" : "text.secondary"}
          >
            {alert.text}
          </Typography>
        </Stack>
      ))}
      {[...view.notes, view.footnote].map((note) => (
        <Typography
          key={note}
          variant="caption"
          color="text.secondary"
          component="div"
          sx={{ mt: 1 }}
        >
          {note}
        </Typography>
      ))}
    </Box>
  );
}
