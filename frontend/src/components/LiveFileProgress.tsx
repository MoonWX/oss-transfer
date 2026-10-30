import { useEffect, useId, useRef, useState } from "react";
import {
  Alert,
  Box,
  Button,
  ButtonBase,
  Collapse,
  IconButton,
  LinearProgress,
  Stack,
  Typography,
} from "@mui/material";
import {
  ArrowForwardRounded,
  ExpandMoreRounded,
  RefreshRounded,
} from "@mui/icons-material";
import { useApi } from "../lib/api";
import { t, useI18n } from "../lib/i18n";
import { formatBytes, formatNumber } from "./Common";

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
  const contentId = useId();
  const [expandedPath, setExpandedPath] = useState<string | null>(null);
  const expanded = path != null && expandedPath === path;
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
    <Box
      component="section"
      aria-label={t("正在传输")}
      sx={{ mb: 3, border: 1, borderColor: "divider", borderRadius: 1 }}
    >
      <Stack direction="row" alignItems="center" justifyContent="space-between">
        <ButtonBase
          aria-label={t("文件传输详情")}
          disabled={data?.supported === false}
          aria-expanded={data?.supported === false ? undefined : expanded}
          aria-controls={data?.supported === false ? undefined : contentId}
          onClick={() => setExpandedPath(expanded ? null : path)}
          sx={{
            flex: 1,
            minWidth: 0,
            minHeight: 44,
            px: 1.5,
            gap: 1,
            justifyContent: "flex-start",
            borderRadius: 1,
            "&:hover, &.Mui-focusVisible": { bgcolor: "action.hover" },
          }}
        >
          <Typography component="span" variant="body2" sx={{ fontWeight: 600 }}>
            {t("正在传输")}
          </Typography>
          {data?.supported && (
            <Typography
              component="span"
              variant="caption"
              color="text.secondary"
            >
              {t("{count} 个文件", {
                count: formatNumber(Math.max(data.total || 0, items.length)),
              })}
            </Typography>
          )}
          {data?.supported !== false && (
            <ExpandMoreRounded
              fontSize="small"
              sx={{
                ml: "auto",
                color: "text.secondary",
                transform: expanded ? "rotate(180deg)" : "none",
              }}
            />
          )}
        </ButtonBase>
        {(stale || refreshMs === 0) && (
          <IconButton
            size="small"
            aria-label={t("刷新文件进度")}
            onClick={() => void request.refresh()}
            sx={{ mr: 0.5 }}
          >
            <RefreshRounded fontSize="small" />
          </IconButton>
        )}
      </Stack>
      {data?.supported === false ? (
        <Typography
          variant="caption"
          color="text.secondary"
          sx={{ display: "block", px: 1.5, pb: 1 }}
        >
          {t("更新执行节点后可查看文件进度")}
        </Typography>
      ) : (
        <>
          {stale && (
            <Alert severity="warning" sx={{ mx: 1.5, mb: 1, py: 0 }}>
              {t("进度暂未更新")}
            </Alert>
          )}
          {!data && !stale && (
            <LinearProgress aria-label={t("正在获取文件进度")} />
          )}
          <Collapse in={expanded} unmountOnExit>
            <Box id={contentId} sx={{ borderTop: 1, borderColor: "divider" }}>
              <Stack
                sx={{
                  maxHeight: 264,
                  overflowY: "auto",
                  scrollbarGutter: "stable",
                }}
              >
                {items.map((item) => {
                  const transferred = Math.min(
                    item.size,
                    item.transferredBytes,
                  );
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
                      sx={{
                        px: 1.5,
                        py: 1.25,
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
                          sx={{ flexShrink: 0 }}
                        >
                          {finalizing ? t("确认中") : `${displayPercent}%`}
                        </Typography>
                      </Stack>
                      <Stack
                        direction="row"
                        alignItems="center"
                        justifyContent="space-between"
                        gap={1}
                        sx={{ mb: 0.75 }}
                      >
                        {!childNumber && (
                          <Typography
                            variant="caption"
                            color="text.secondary"
                            noWrap
                            title={item.executionNodeLabel}
                            sx={{
                              minWidth: 0,
                              display: "flex",
                              alignItems: "center",
                              gap: 0.75,
                            }}
                          >
                            {onSelectChild ? (
                              <Button
                                size="small"
                                onClick={() => onSelectChild(item.childTaskId)}
                                aria-label={t("子任务 #{id}", {
                                  id: item.childTaskId,
                                })}
                                sx={{
                                  minWidth: 0,
                                  p: 0,
                                  flexShrink: 0,
                                  fontSize: "inherit",
                                  lineHeight: "inherit",
                                }}
                              >
                                #{item.childTaskId}
                              </Button>
                            ) : (
                              <Box component="span" sx={{ flexShrink: 0 }}>
                                #{item.childTaskId}
                              </Box>
                            )}
                            <Box
                              component="span"
                              sx={{
                                overflow: "hidden",
                                textOverflow: "ellipsis",
                              }}
                            >
                              {item.executionNodeLabel}
                            </Box>
                          </Typography>
                        )}
                        <Typography
                          variant="caption"
                          color="text.secondary"
                          sx={{ flexShrink: 0, whiteSpace: "nowrap" }}
                        >
                          {formatBytes(transferred)} / {formatBytes(item.size)}
                        </Typography>
                      </Stack>
                      <LinearProgress
                        variant="determinate"
                        value={percent}
                        aria-label={t("文件进度：{key}", {
                          key: item.sourceKey,
                        })}
                        aria-valuetext={
                          finalizing ? t("确认中") : `${displayPercent}%`
                        }
                        color={stale ? "inherit" : "primary"}
                        sx={{
                          height: 3,
                          borderRadius: 1,
                          bgcolor: "action.hover",
                        }}
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
                  sx={{ mx: 1.5, my: 1 }}
                >
                  {t("在子任务中查看其他文件")}
                </Button>
              )}
            </Box>
          </Collapse>
        </>
      )}
    </Box>
  );
}
