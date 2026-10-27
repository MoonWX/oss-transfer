import { useEffect, useRef, useState } from "react";
import {
  Alert,
  Box,
  Button,
  IconButton,
  LinearProgress,
  Stack,
  Typography,
} from "@mui/material";
import { ArrowForwardRounded, RefreshRounded } from "@mui/icons-material";
import { useApi } from "../lib/api";
import { t, useI18n } from "../lib/i18n";
import { formatBytes } from "./Common";

type FileProgress = {
  childTaskId: string;
  subtaskNumber: number;
  executionNodeLabel: string;
  itemId: string;
  sourceKey: string;
  destinationKey: string;
  size: number;
  transferredBytes: number;
  stage: "TRANSFERRING" | "FINALIZING";
};
type Progress = {
  parentTaskId: number;
  generation: number;
  childNumber?: number;
  supported: boolean;
  partial: boolean;
  observedAt: number;
  items: FileProgress[];
  total: number;
  hasMore: boolean;
};
type Props = {
  taskId: number;
  generation?: number;
  childNumber?: number;
  status: string;
  refreshMs: number;
  enabled?: boolean;
  onSelectChild?: (id: string) => void;
  onShowChildren?: () => void;
};

export default function LiveFileProgress({
  taskId,
  generation,
  childNumber,
  status,
  refreshMs,
  enabled = true,
  onSelectChild,
  onShowChildren,
}: Props) {
  useI18n();
  const active = enabled && ["RUNNING", "STOPPING"].includes(status);
  const params = new URLSearchParams();
  if (childNumber != null) params.set("childNumber", String(childNumber));
  if (generation != null) params.set("generation", String(generation));
  const query = params.toString();
  const path = active
    ? `/api/master/tasks/${taskId}/progress${query ? `?${query}` : ""}`
    : null;
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

  return (
    <Box component="section" aria-label={t("正在传输")} sx={{ mb: 3 }}>
      <Stack
        direction="row"
        alignItems="center"
        justifyContent="space-between"
        sx={{ mb: 1.5 }}
      >
        <Typography variant="subtitle1" sx={{ fontWeight: 600 }}>
          {t("正在传输")}
        </Typography>
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
        <Typography variant="body2" color="text.secondary">
          {t("更新执行节点后可查看文件进度")}
        </Typography>
      ) : (
        <>
          {stale && (
            <Alert severity="warning" sx={{ mb: items.length ? 1.5 : 0 }}>
              {t("进度暂未更新")}
            </Alert>
          )}
          {!data && !stale && (
            <LinearProgress aria-label={t("正在获取文件进度")} />
          )}
          <Stack gap={2}>
            {items.map((item) => {
              const transferred = Math.min(item.size, item.transferredBytes);
              const finalizing =
                item.stage === "FINALIZING" || transferred === item.size;
              const percent =
                item.size > 0 ? (transferred / item.size) * 100 : 100;
              const displayPercent = Math.min(
                percent < 100 ? 99.9 : 100,
                Math.floor(percent * 10) / 10,
              );
              return (
                <Box
                  key={`${item.childTaskId}:${item.itemId}`}
                  data-file-progress={item.itemId}
                >
                  <Typography
                    variant="body2"
                    sx={{ overflowWrap: "anywhere", mb: 0.5 }}
                  >
                    {item.sourceKey}
                  </Typography>
                  {!childNumber && (
                    <Typography
                      variant="caption"
                      color="text.secondary"
                      sx={{ display: "block", mb: 0.5 }}
                    >
                      {onSelectChild ? (
                        <Button
                          size="small"
                          onClick={() => onSelectChild(item.childTaskId)}
                          sx={{ minWidth: 0, p: 0, mr: 1, fontSize: "inherit" }}
                        >
                          {t("子任务 #{id}", { id: item.childTaskId })}
                        </Button>
                      ) : (
                        <>{t("子任务 #{id}", { id: item.childTaskId })} · </>
                      )}
                      {item.executionNodeLabel}
                    </Typography>
                  )}
                  <Stack
                    direction="row"
                    justifyContent="space-between"
                    gap={1}
                    sx={{ mb: 0.75 }}
                  >
                    <Typography variant="caption" color="text.secondary">
                      {formatBytes(transferred)} / {formatBytes(item.size)}
                    </Typography>
                    <Typography
                      variant="caption"
                      color="text.secondary"
                      sx={{ whiteSpace: "nowrap" }}
                    >
                      {finalizing ? t("确认中") : `${displayPercent}%`}
                    </Typography>
                  </Stack>
                  <LinearProgress
                    variant="determinate"
                    value={percent}
                    aria-label={t("文件进度：{key}", { key: item.sourceKey })}
                    aria-valuetext={
                      finalizing ? t("确认中") : `${displayPercent}%`
                    }
                    color={stale ? "inherit" : "primary"}
                    sx={{ height: 5, borderRadius: 1 }}
                  />
                </Box>
              );
            })}
          </Stack>
          {data?.hasMore && (
            <Button
              size="small"
              endIcon={<ArrowForwardRounded />}
              onClick={onShowChildren}
              disabled={!onShowChildren}
              sx={{ mt: 1.5 }}
            >
              {t("在子任务中查看其他文件")}
            </Button>
          )}
        </>
      )}
    </Box>
  );
}
