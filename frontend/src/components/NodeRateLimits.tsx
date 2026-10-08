import { Link as RouterLink } from "react-router-dom";
import {
  Alert,
  Box,
  Button,
  Chip,
  LinearProgress,
  Stack,
  Typography,
} from "@mui/material";
import { formatNumber } from "./Common";

import type {
  RateType,
  Rate,
  Adjustment,
  RateLimitSnapshot,
} from "../lib/rateStatus";
export type { RateType, Rate, Adjustment } from "../lib/rateStatus";
export const rateTypes: RateType[] = ["list", "put", "head", "bandwidth"];
export const rateNames: Record<RateType, string> = {
  list: "LIST 请求",
  put: "PUT 请求",
  head: "HEAD 请求",
  bandwidth: "传输带宽",
};
export const rateText = (
  value: number | null | undefined,
  bandwidth = false,
) =>
  value == null
    ? "—"
    : value <= 0
      ? "不限速"
      : bandwidth
        ? `${(value / 1_000_000).toLocaleString(undefined, { maximumFractionDigits: 2 })} Mbps`
        : `${formatNumber(value)} 次/秒`;
const currentText = (value: number | null | undefined, bandwidth: boolean) =>
  value == null
    ? "—"
    : bandwidth
      ? `${(value / 1_000_000).toLocaleString(undefined, { maximumFractionDigits: 2 })} Mbps`
      : `${formatNumber(value)} 次/秒`;
export const rateTarget = (rate: Rate | undefined, type: RateType) =>
  type === "bandwidth" ? rate?.manualLimitBps : rate?.manualLimitQps;
export const adjustmentPhase = (event: Adjustment) => {
  if (event.manual || event.reasonCode === "MANUAL_SET") return "手动设置";
  switch (event.reasonCode) {
    case "OSS_503":
      return "自动降速";
    case "RECOVERY_STEP":
      return "逐步恢复";
    case "RECOVERED":
      return "已恢复目标";
    default:
      return "自动调整";
  }
};
export const adjustmentReason = (event: Adjustment) => {
  switch (event.reasonCode) {
    case "MANUAL_SET":
      return "用户修改上限，保存后立即生效。";
    case "OSS_503":
      return event.consecutive503Count != null &&
        event.consecutive503Threshold != null
        ? `OSS 返回 HTTP 503；计数 ${formatNumber(event.consecutive503Count)}，阈值 ${formatNumber(event.consecutive503Threshold)}。`
        : "OSS 返回 HTTP 503，触发节点降速。";
    case "RECOVERY_STEP":
      return "定时恢复，提高运行上限；仍低于目标。";
    case "RECOVERED":
      return "定时恢复，运行上限已达到目标。";
    case "AUTO_ADJUST":
      return "自动调整；未记录更具体的原因。";
    default:
      return "历史记录未保存调整原因。";
  }
};
export const adjustmentDetailsPath = (workerIp: string, eventId: string) =>
  `/nodes/adjustments?${new URLSearchParams({ workerIp, eventId }).toString()}`;
const percentText = (value: number) =>
  value.toLocaleString(undefined, { maximumFractionDigits: 1 });

export default function NodeRateLimits({
  ip,
  rateLimit,
  refreshMs,
  stale,
  onEdit,
}: {
  ip: string;
  rateLimit?: RateLimitSnapshot;
  refreshMs: number;
  stale: boolean;
  onEdit: (ip: string, type: RateType, target?: number) => void;
}) {
  const uncertain = stale || refreshMs === 0;
  const activeAdjustments = [rateLimit?.put, rateLimit?.head].filter(
    (rate) => rate?.state === "REDUCED" || rate?.state === "RECOVERING",
  );
  const latestAdjustment = activeAdjustments
    .map((rate) => rate?.lastAdjustment)
    .filter((event): event is Adjustment => Boolean(event))
    .sort(
      (a, b) =>
        (b.timestamp ?? 0) - (a.timestamp ?? 0) ||
        (b.sequence ?? 0) - (a.sequence ?? 0),
    )[0];

  return (
    <Stack spacing={2}>
      <Typography variant="subtitle2">请求与带宽</Typography>
      {refreshMs === 0 && (
        <Alert severity="info">
          自动刷新已暂停。以下状态为最近一次读取结果。
        </Alert>
      )}
      {stale && (
        <Alert severity="warning">
          节点状态未更新。以下为上次读取值，当前限速与恢复进度待确认。
        </Alert>
      )}
      {!uncertain && activeAdjustments.length > 0 && (
        <Alert severity="warning" data-testid={`rate-notice-${ip}`}>
          <Stack spacing={0.5} alignItems="flex-start">
            <Typography variant="body2">
              节点可能遭遇限速，已自动调整运行上限。
            </Typography>
            {latestAdjustment?.id &&
              latestAdjustment.detailsAvailable !== false && (
                <Button
                  component={RouterLink}
                  to={adjustmentDetailsPath(ip, latestAdjustment.id)}
                  color="inherit"
                  size="small"
                  sx={{ minWidth: 0, p: 0 }}
                >
                  查看详情
                </Button>
              )}
          </Stack>
        </Alert>
      )}
      {rateTypes.map((type) => {
        const rate = rateLimit?.[type];
        const bandwidth = type === "bandwidth";
        const current = bandwidth ? rate?.currentBps : rate?.currentQps;
        const limit = bandwidth ? rate?.limitBps : rate?.limitQps;
        const target = rateTarget(rate, type);
        const reduced =
          rate?.state === "REDUCED" || rate?.state === "RECOVERING";
        const configuredTarget = bandwidth
          ? rate?.configuredLimitBps
          : rate?.configuredLimitQps;
        const usesConfiguredTarget =
          rate?.limitSource === "CONFIG" ||
          (target != null &&
            configuredTarget != null &&
            target === configuredTarget);
        const hasRecoveryStatus = type === "put" || type === "head";
        const recovered = !uncertain && rate?.state === "RECOVERED";
        const recovering = !uncertain && rate?.state === "RECOVERING";
        const stateLabel = uncertain
          ? "状态待确认"
          : reduced
            ? rate?.state === "RECOVERING"
              ? "恢复中"
              : "自动降速"
            : recovered
              ? "已恢复目标"
              : target !== undefined && target <= 0
                ? "不限速"
                : rate?.state === "NORMAL"
                  ? "按目标运行"
                  : undefined;
        const showProgress =
          hasRecoveryStatus &&
          !uncertain &&
          target !== undefined &&
          target > 0 &&
          limit !== undefined &&
          (reduced || recovered);
        const percentage =
          target && target > 0 && limit !== undefined
            ? Math.max(0, Math.min(100, (limit / target) * 100))
            : 0;
        return (
          <Box
            key={type}
            data-testid={`rate-${ip}-${type}`}
            sx={{
              border: 1,
              borderColor:
                reduced && !uncertain
                  ? recovering
                    ? "info.main"
                    : "warning.main"
                  : "divider",
              borderRadius: 2,
              p: 1.5,
              minWidth: 0,
            }}
          >
            <Stack
              direction="row"
              justifyContent="space-between"
              alignItems="center"
              spacing={1}
            >
              <Stack
                direction="row"
                spacing={1}
                useFlexGap
                flexWrap="wrap"
                alignItems="center"
                sx={{ minWidth: 0 }}
              >
                <Typography variant="subtitle2">{rateNames[type]}</Typography>
                {stateLabel && (
                  <Chip
                    label={stateLabel}
                    size="small"
                    variant="outlined"
                    color={
                      uncertain
                        ? "default"
                        : reduced
                          ? recovering
                            ? "info"
                            : "warning"
                          : recovered
                            ? "success"
                            : "default"
                    }
                  />
                )}
              </Stack>
              <Button
                size="small"
                onClick={() => onEdit(ip, type, target)}
                aria-label={`调整 ${ip} 的 ${rateNames[type]} 限速`}
                sx={{ flexShrink: 0 }}
              >
                调整
              </Button>
            </Stack>
            <Box
              sx={{
                display: "grid",
                gridTemplateColumns: {
                  xs: "1fr",
                  sm: "repeat(3, minmax(0, 1fr))",
                },
                gap: 1.25,
                mt: 1.5,
              }}
            >
              {[
                { label: "运行上限", value: rateText(limit, bandwidth) },
                {
                  label: usesConfiguredTarget
                    ? "配置目标"
                    : rate?.limitSource === "PERSISTED_MANUAL"
                      ? "已保存目标"
                      : "目标上限",
                  value: rateText(target, bandwidth),
                },
                { label: "实测速率", value: currentText(current, bandwidth) },
              ].map(({ label, value }) => (
                <Box key={label}>
                  <Typography variant="caption" color="text.secondary">
                    {label}
                  </Typography>
                  <Typography
                    variant="body2"
                    sx={{
                      fontVariantNumeric: "tabular-nums",
                      overflowWrap: "anywhere",
                    }}
                  >
                    {value}
                  </Typography>
                </Box>
              ))}
            </Box>
            {showProgress && (
              <Box sx={{ mt: 1.75 }}>
                <Stack
                  direction="row"
                  justifyContent="space-between"
                  alignItems="baseline"
                  spacing={1}
                  sx={{ mb: 0.75 }}
                >
                  <Typography variant="caption" color="text.secondary">
                    运行上限 / 目标上限
                  </Typography>
                  <Typography variant="caption">
                    {percentText(percentage)}%
                  </Typography>
                </Stack>
                <LinearProgress
                  variant="determinate"
                  value={percentage}
                  color={
                    recovered ? "success" : recovering ? "info" : "warning"
                  }
                  aria-label={`${rateNames[type]}运行上限占目标比例`}
                  aria-valuetext={`${percentText(percentage)}%，运行上限占目标上限比例`}
                  sx={{ height: 6, borderRadius: 6 }}
                />
              </Box>
            )}
          </Box>
        );
      })}
    </Stack>
  );
}
