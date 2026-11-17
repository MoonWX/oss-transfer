import { radii } from "../theme";
import { useEffect, useRef, useState } from "react";
import {
  Alert,
  Box,
  IconButton,
  LinearProgress,
  Stack,
  Typography,
} from "@mui/material";
import { RefreshRounded } from "@mui/icons-material";
import { useApi } from "../lib/api";
import { t, useI18n } from "../lib/i18n";
import { formatBytes } from "./Common";

type FileProgress = {
  childTaskId: string;
  itemId: string;
  sourceKey: string;
  size: number;
  // With basis TARGET: bytes the destination has confirmed plus the bytes
  // the current attempt of each request has handed to the network, below
  // size until every byte is confirmed. Without a basis (older Agents):
  // bytes read from the source.
  transferredBytes: number;
  stage: "TRANSFERRING" | "FINALIZING";
  readBytes?: number;
  phase?: string;
  basis?: string;
};
type Progress = {
  parentTaskId: number;
  generation: number;
  childNumber: number;
  supported: boolean;
  partial: boolean;
  observedAt: number;
  items: FileProgress[];
};
type Props = {
  taskId: number;
  generation?: number;
  childNumber: number;
  status: string;
  refreshMs: number;
  enabled?: boolean;
};

// What a file's requests wait for while none of them is sending.
function phaseLabel(phase?: string) {
  switch (phase) {
    case "RETRY_WAIT":
      return t("等待重试");
    case "WAITING_REQUEST_LIMIT":
      return t("等待写入请求限速");
    case "WAITING_UPLOAD_BUFFER":
      return t("等待上传缓冲");
  }
  return undefined;
}

export default function LiveFileProgress({
  taskId,
  generation,
  childNumber,
  status,
  refreshMs,
  enabled = true,
}: Props) {
  useI18n();
  const active = enabled && ["RUNNING", "STOPPING"].includes(status);
  const params = new URLSearchParams({ childNumber: String(childNumber) });
  if (generation != null) params.set("generation", String(generation));
  const path = active ? `/api/master/tasks/${taskId}/progress?${params}` : null;
  const request = useApi<{ data: Progress }>(path, refreshMs);
  const [now, setNow] = useState(Date.now);
  const latest = useRef<{ path: string | null; data?: Progress }>({ path });
  if (latest.current.path !== path) latest.current = { path };
  const incoming = request.data?.data;
  const matches =
    incoming?.parentTaskId === taskId &&
    incoming.childNumber === childNumber &&
    (generation == null || incoming.generation === generation) &&
    Number.isFinite(incoming.observedAt) &&
    Array.isArray(incoming.items);
  if (
    matches &&
    (!latest.current.data ||
      incoming.observedAt >= latest.current.data.observedAt)
  )
    latest.current.data = incoming;
  const data = latest.current.data;
  useEffect(() => {
    if (!active) return;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [active]);
  if (!active) return null;

  const stale =
    !!request.error ||
    (!!incoming && !matches) ||
    (!!incoming && !!data && incoming.observedAt < data.observedAt) ||
    !!data?.partial ||
    (!!data && now - data.observedAt > Math.max(15_000, refreshMs * 2));
  const items = (data?.items || []).filter(
    (item) =>
      Number.isFinite(item.size) &&
      item.size >= 0 &&
      Number.isFinite(item.transferredBytes) &&
      item.transferredBytes >= 0 &&
      ["TRANSFERRING", "FINALIZING"].includes(item.stage),
  );
  if (data?.supported && !stale && !items.length) return null;
  // One child runs on one node, so its files share a basis.
  const counted = items.some((item) => item.basis === "TARGET");
  const older = items.some((item) => !item.basis);

  return (
    <Box component="section" aria-label={t("文件进度")} sx={{ mb: 3 }}>
      <Stack
        direction="row"
        alignItems="center"
        justifyContent="space-between"
        sx={{ mb: 1 }}
      >
        <Typography variant="subtitle2">{t("文件进度")}</Typography>
        {(stale || refreshMs === 0) && (
          <IconButton
            size="small"
            aria-label={t("刷新文件进度")}
            onClick={() => void request.refresh()}
          >
            <RefreshRounded fontSize="small" />
          </IconButton>
        )}
      </Stack>
      {data?.supported === false ? (
        <Typography variant="caption" color="text.secondary">
          {t("更新执行节点后可查看文件进度")}
        </Typography>
      ) : (
        <>
          {stale && (
            <Alert severity="warning" sx={{ mb: 1, py: 0 }}>
              {t("进度暂未更新")}
            </Alert>
          )}
          {(counted || older) && (
            <Typography
              variant="caption"
              color="text.secondary"
              data-file-progress-basis
              sx={{ display: "block", mb: 0.5 }}
            >
              {counted
                ? t(
                    "已传输为目标端已确认的字节，加上各请求本次尝试已交给网络的字节；请求失败时，该次尝试的字节会撤回。",
                  )
                : t(
                    "该执行节点版本较旧，进度按读取源端计算，写入目标端可能仍在进行",
                  )}
            </Typography>
          )}
          {!data && !stale && (
            <LinearProgress aria-label={t("正在获取文件进度")} />
          )}
          <Stack
            sx={{
              maxHeight: 264,
              overflowY: "auto",
              scrollbarGutter: "stable",
            }}
          >
            {items.map((item) => {
              const transferred = Math.min(item.size, item.transferredBytes);
              const finalizing = item.stage === "FINALIZING";
              // An empty file has no bytes to count, so it shows no
              // percentage until its write is confirmed.
              const percent =
                item.size > 0
                  ? (transferred / item.size) * 100
                  : finalizing
                    ? 100
                    : 0;
              const displayPercent = Math.min(
                finalizing ? 100 : 99.9,
                Math.floor(percent * 10) / 10,
              );
              // Bytes read from the source run ahead of the destination:
              // the lighter buffer segment of the bar.
              const read =
                Number.isFinite(item.readBytes) && (item.readBytes || 0) > 0
                  ? Math.min(item.size, item.readBytes || 0)
                  : null;
              const buffer =
                read != null && item.size > 0
                  ? (Math.max(read, transferred) / item.size) * 100
                  : null;
              const label = finalizing
                ? item.basis === "TARGET"
                  ? t("确认中")
                  : t("源端已读完")
                : (phaseLabel(item.phase) ??
                  (item.size > 0 ? `${displayPercent}%` : t("传输中")));
              const hint = finalizing
                ? item.basis === "TARGET"
                  ? t(
                      "数据已全部被目标端接收，正在完成分片上传，或把文件同步到磁盘并发布",
                    )
                  : !item.basis
                    ? t(
                        "该执行节点版本较旧，进度按读取源端计算，写入目标端可能仍在进行",
                      )
                    : undefined
                : undefined;
              return (
                <Box
                  key={`${item.childTaskId}:${item.itemId}`}
                  data-file-progress={item.itemId}
                  sx={{
                    py: 1,
                    borderBottom: 1,
                    borderColor: "divider",
                    "&:last-child": { borderBottom: 0 },
                  }}
                >
                  <Stack
                    direction="row"
                    alignItems="center"
                    gap={1}
                    sx={{ mb: 0.5 }}
                  >
                    <Typography
                      variant="body2"
                      noWrap
                      title={item.sourceKey}
                      sx={{ minWidth: 0, flex: 1 }}
                    >
                      {item.sourceKey}
                    </Typography>
                    <Typography
                      variant="caption"
                      color="text.secondary"
                      title={hint}
                      data-file-stage
                      sx={{ flexShrink: 0 }}
                    >
                      {label}
                    </Typography>
                  </Stack>
                  <Typography
                    variant="caption"
                    color="text.secondary"
                    sx={{ display: "block", mb: 0.75 }}
                  >
                    {formatBytes(transferred)} / {formatBytes(item.size)}
                  </Typography>
                  <LinearProgress
                    variant={buffer == null ? "determinate" : "buffer"}
                    value={percent}
                    valueBuffer={buffer ?? undefined}
                    title={
                      read == null
                        ? undefined
                        : t("已从源端读取 {bytes}", {
                            bytes: formatBytes(read),
                          })
                    }
                    aria-label={t("文件进度：{key}", { key: item.sourceKey })}
                    aria-valuetext={label}
                    color={stale ? "inherit" : "primary"}
                    sx={{
                      height: 3,
                      borderRadius: radii.pill,
                      bgcolor: "action.hover",
                      "& .MuiLinearProgress-dashed": { display: "none" },
                    }}
                  />
                </Box>
              );
            })}
          </Stack>
        </>
      )}
    </Box>
  );
}
