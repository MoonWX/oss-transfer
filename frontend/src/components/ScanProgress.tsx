import { Box, Typography } from "@mui/material";
import { t, useI18n } from "../lib/i18n";
import { scanActivityLines, type ScanActivity } from "../lib/scanActivity";

// What the scanning node reports about a running scan, shown while a running
// task's scan is incomplete and Master has heard from its scanner. Nothing is
// shown without a report: an absent report must not read as a scan that has
// done nothing.
export default function ScanProgress({
  activity,
  compact = false,
}: {
  activity?: ScanActivity | null;
  compact?: boolean;
}) {
  useI18n();
  const lines = scanActivityLines(activity);
  if (!lines.length) return null;
  return (
    <Box sx={{ mb: compact ? 1 : 3 }} data-testid="scan-progress">
      <Typography
        variant={compact ? "caption" : "h6"}
        component="div"
        color={compact ? "text.secondary" : undefined}
        sx={{ mb: compact ? 0.25 : 1, fontWeight: compact ? 500 : undefined }}
      >
        {t("扫描进度")}
      </Typography>
      {lines.map((line) => (
        <Typography
          key={line}
          variant={compact ? "caption" : "body2"}
          color="text.secondary"
          component="div"
        >
          {line}
        </Typography>
      ))}
    </Box>
  );
}
