import { t as tr, useI18n, getLocale } from "../lib/i18n";
import type { NodeDisplay } from "../lib/nodeRegistry";
import { useNodeLabels } from "../lib/workerStatus";
import { useEffect, useRef, useState } from "react";
import { Link as RouterLink, useSearchParams } from "react-router-dom";
import { ArrowBackRounded } from "@mui/icons-material";
import {
  Alert,
  Box,
  Button,
  Chip,
  Paper,
  Stack,
  Typography,
} from "@mui/material";
import { api, useApi } from "../lib/api";
import {
  eventChange,
  type Adjustment,
  type TriggerRequest,
} from "../lib/rateStatus";
import { useRefreshPreference } from "../lib/refreshPreference";
import { adjustmentPhase } from "../components/NodeRateLimits";
import { ErrorAlert, LoadingState, PageHeader } from "../components/Common";
import RefreshControls from "../components/RefreshControls";

type Detail = NodeDisplay & {
  workerIp: string;
  event: Adjustment;
  incidentId?: string | null;
  triggerEvent?: Adjustment | null;
  events: Adjustment[];
  nextCursor?: string | number | null;
  done: boolean;
  processRestarted?: boolean;
  incidentActive?: boolean;
  historyComplete?: boolean;
};
const dateTime = (value?: number | null) =>
  value ? new Date(value).toLocaleString(getLocale()) : tr("未记录");

function Field({ label, value }: { label: string; value: string | number }) {
  useI18n();
  return (
    <Box sx={{ minWidth: 0 }}>
      <Typography component="dt" variant="body2" color="text.secondary">
        {label}
      </Typography>
      <Typography
        component="dd"
        sx={{ m: 0, mt: 0.5, overflowWrap: "anywhere" }}
      >
        {value}
      </Typography>
    </Box>
  );
}

export default function RateAdjustmentPage() {
  useI18n();
  const [params] = useSearchParams();
  const workerIp = params.get("workerIp") ?? "";
  const eventId = params.get("eventId") ?? "";
  // Changing events resets paging and keeps prior request details out of the new page.
  return (
    <DetailContent
      key={`${workerIp}:${eventId}`}
      workerIp={workerIp}
      eventId={eventId}
    />
  );
}
function DetailContent({
  workerIp,
  eventId,
}: {
  workerIp: string;
  eventId: string;
}) {
  const fields: [keyof TriggerRequest, string][] = [
    ["operation", tr("操作")],
    ["direction", tr("迁移方向")],
    ["method", tr("HTTP 方法")],
    ["taskId", tr("任务 ID")],
    ["connectionAlias", tr("存储连接")],
    ["bucket", "Bucket"],
    ["key", tr("对象 Key")],
    ["sourceBucket", tr("源 Bucket")],
    ["destinationBucket", tr("目标 Bucket")],
    ["partNumber", tr("分片编号")],
    ["attempt", tr("尝试次数")],
    ["statusCode", tr("HTTP 状态码")],
    ["errorCode", tr("错误码")],
    ["requestId", tr("请求 ID")],
    ["extendedRequestId", tr("扩展请求 ID")],
    ["handlingStage", tr("处理阶段")],
    ["message", tr("错误信息")],
  ];

  useI18n();
  const { refreshMs, refreshSeconds } = useRefreshPreference();
  const nodeLabel = useNodeLabels();
  const path =
    workerIp && eventId
      ? `/api/master/rate-limit/adjustment?${new URLSearchParams({ workerIp, eventId })}`
      : null;
  const { data, error, loading, refresh } = useApi<Detail>(path, refreshMs);
  const [pages, setPages] = useState<
    { cursor: string | number; data: Detail }[]
  >([]);
  const pagesRef = useRef(pages);
  pagesRef.current = pages;
  const resetOnSuccess = useRef(false);
  const tailPoll = useRef<AbortController | null>(null);
  const [moreError, setMoreError] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const pending = useRef<AbortController | null>(null);
  useEffect(
    () => () => {
      pending.current?.abort();
      tailPoll.current?.abort();
    },
    [],
  );
  useEffect(() => {
    tailPoll.current?.abort();
    if (!data || !path) return;
    if (resetOnSuccess.current) {
      resetOnSuccess.current = false;
      setPages([]);
      return;
    }
    const lastPage = pagesRef.current.at(-1);
    if (!lastPage) return;
    // Completed intermediate pages are immutable; only the loaded tail can grow.
    const controller = new AbortController();
    tailPoll.current = controller;
    void api<Detail>(`${path}&cursor=${encodeURIComponent(lastPage.cursor)}`, {
      signal: controller.signal,
    })
      .then((next) => {
        if (controller.signal.aborted) return;
        setPages((previous) =>
          previous.map((page) =>
            page.cursor === lastPage.cursor ? { ...page, data: next } : page,
          ),
        );
        setMoreError(null);
      })
      .catch((e: Error) => {
        if (!controller.signal.aborted)
          setMoreError(tr("后续记录未更新：{value1}", { value1: e.message }));
      });
    return () => controller.abort();
  }, [data, path]);
  const refreshDetail = () => {
    pending.current?.abort();
    pending.current = null;
    setLoadingMore(false);
    tailPoll.current?.abort();
    resetOnSuccess.current = true;
    setMoreError(null);
    return refresh();
  };
  const tail = pages.at(-1)?.data ?? data;
  async function loadMore() {
    if (!path || tail?.nextCursor == null || loadingMore) return;
    const cursor = tail.nextCursor;
    const controller = new AbortController();
    pending.current = controller;
    setLoadingMore(true);
    setMoreError(null);
    try {
      const next = await api<Detail>(
        `${path}&cursor=${encodeURIComponent(tail.nextCursor)}`,
        { signal: controller.signal },
      );
      if (!controller.signal.aborted)
        setPages((previous) => [...previous, { cursor, data: next }]);
    } catch (e) {
      if (!controller.signal.aborted) setMoreError((e as Error).message);
    } finally {
      if (pending.current === controller) setLoadingMore(false);
    }
  }
  const events = [
    ...new Map(
      [
        ...(data?.events ?? []),
        ...pages.flatMap((page) => page.data.events),
      ].map((event) => [event.id, event]),
    ).values(),
  ];
  const trigger =
    data?.event.reasonCode === "OSS_503" ? data.event : data?.triggerEvent;
  const request =
    trigger?.triggerRequest ??
    (data?.event.reasonCode === "OSS_503" ? data.event.triggerRequest : null);
  const requestFields = fields.filter(
    ([key]) => request?.[key] != null && request[key] !== "",
  );
  const manual = data?.event.manual && !trigger;
  const currentUnknown =
    !!error || refreshSeconds === 0 || !!data?.processRestarted;
  return (
    <>
      <Button
        component={RouterLink}
        to="/nodes"
        startIcon={<ArrowBackRounded />}
        sx={{ mb: 2 }}
      >
        {tr("返回执行节点")}
      </Button>
      <PageHeader
        title={tr("调节详情")}
        description={nodeLabel(workerIp, data)}
        action={
          <RefreshControls
            onRefresh={refreshDetail}
            loading={loading}
            refreshLabel={tr("刷新调节详情")}
          />
        }
      />
      {!path ? (
        <Alert severity="error">{tr("缺少节点或事件 ID。")}</Alert>
      ) : (
        <>
          <ErrorAlert error={error} onRetry={refreshDetail} />
          {!data && loading && <LoadingState />}
          {data && (
            <Stack spacing={3}>
              {data.historyComplete === false && (
                <Alert severity="warning">
                  {tr("本轮部分记录未保存，以下详情不完整。")}
                </Alert>
              )}
              {data.processRestarted ? (
                <Alert severity="info">
                  {tr("节点已重启，以下为历史记录，无法据此判断当前恢复状态。")}
                </Alert>
              ) : refreshSeconds === 0 ? (
                <Alert severity="info">
                  {tr("自动刷新已暂停，以下为最近一次读取结果。")}
                </Alert>
              ) : data.incidentActive && !currentUnknown ? (
                <Alert severity="warning">
                  {tr(
                    "该节点曾收到 HTTP 503，已自动调整请求上限，最近读取时尚未恢复到目标。",
                  )}
                </Alert>
              ) : !manual &&
                data.incidentId &&
                data.incidentActive === false &&
                !currentUnknown ? (
                <Alert severity="success">{tr("本轮调节已结束。")}</Alert>
              ) : null}
              <Paper variant="outlined" sx={{ p: { xs: 2, sm: 3 } }}>
                <Typography variant="h6" component="h2" sx={{ mb: 2 }}>
                  {manual ? tr("手动调整") : tr("触发本次降速的请求")}
                </Typography>
                <Box
                  component="dl"
                  data-testid="rate-trigger-request"
                  sx={{
                    m: 0,
                    display: "grid",
                    gridTemplateColumns: {
                      xs: "1fr",
                      md: "repeat(2, minmax(0, 1fr))",
                    },
                    gap: 2.5,
                  }}
                >
                  <Field
                    label={manual ? tr("调整时间") : tr("触发时间")}
                    value={dateTime((trigger ?? data.event).timestamp)}
                  />
                  <Field
                    label={tr("事件 ID")}
                    value={(trigger ?? data.event).id ?? tr("未记录")}
                  />
                  {requestFields.map(([key, label]) => (
                    <Field key={key} label={label} value={request![key]!} />
                  ))}
                </Box>
                {request?.contextSource === "TRANSFER_CONTEXT" && (
                  <Typography color="text.secondary" sx={{ mt: 2 }}>
                    {tr("仅记录到迁移处理阶段，未确定具体 HTTP 请求。")}
                  </Typography>
                )}
                {!manual && !requestFields.length && (
                  <Typography color="text.secondary" sx={{ mt: 2 }}>
                    {tr("未记录触发请求详情。")}
                  </Typography>
                )}
              </Paper>
              {trigger && (
                <Paper variant="outlined" sx={{ p: { xs: 2, sm: 3 } }}>
                  <Typography variant="h6" component="h2" sx={{ mb: 2 }}>
                    {tr("触发条件")}
                  </Typography>
                  <Box
                    component="dl"
                    sx={{
                      m: 0,
                      display: "grid",
                      gridTemplateColumns: {
                        xs: "1fr",
                        sm: "repeat(2, minmax(0, 1fr))",
                      },
                      gap: 2.5,
                    }}
                  >
                    <Field
                      label={tr("节点 503 计数 / 阈值")}
                      value={`${trigger.consecutive503Count ?? tr("未记录")} / ${trigger.consecutive503Threshold ?? tr("未记录")}`}
                    />
                    {trigger.factor != null && (
                      <Field
                        label={tr("降速幅度")}
                        value={`${Number(((1 - trigger.factor) * 100).toFixed(2))}%`}
                      />
                    )}
                    {trigger.minQps != null && (
                      <Field
                        label={tr("降速下限")}
                        value={tr("{value1} 次/秒（目标更低时保持目标）", {
                          value1: trigger.minQps,
                        })}
                      />
                    )}
                    {trigger.intervalMs != null && (
                      <Field
                        label={tr("恢复间隔")}
                        value={tr("{value1} 秒", {
                          value1: trigger.intervalMs / 1000,
                        })}
                      />
                    )}
                    {trigger.increaseFactor != null && (
                      <Field
                        label={tr("每次恢复幅度")}
                        value={`${Number(((trigger.increaseFactor - 1) * 100).toFixed(2))}%`}
                      />
                    )}
                  </Box>
                </Paper>
              )}
              <Paper variant="outlined" sx={{ p: { xs: 2, sm: 3 } }}>
                <Typography variant="h6" component="h2" sx={{ mb: 2 }}>
                  {tr("调整记录")}
                </Typography>
                <Stack
                  component="ol"
                  data-testid="rate-adjustment-events"
                  spacing={0}
                  sx={{ p: 0, m: 0, listStyle: "none" }}
                >
                  {events.map((event) => (
                    <Box
                      component="li"
                      key={event.id}
                      sx={{
                        py: 2,
                        borderTop: 1,
                        borderColor: "divider",
                        minWidth: 0,
                      }}
                    >
                      <Stack
                        direction="row"
                        spacing={1.5}
                        useFlexGap
                        flexWrap="wrap"
                        alignItems="center"
                        sx={{ mb: 1 }}
                      >
                        <Chip
                          size="small"
                          label={adjustmentPhase(event)}
                          variant="outlined"
                          color={
                            event.reasonCode === "OSS_503"
                              ? "warning"
                              : event.reasonCode === "RECOVERED"
                                ? "success"
                                : "default"
                          }
                        />
                        <Typography variant="body2" color="text.secondary">
                          {dateTime(event.timestamp)}
                        </Typography>
                      </Stack>
                      <Typography sx={{ overflowWrap: "anywhere" }}>
                        {eventChange(event)}
                      </Typography>
                      {(event.reasonCode === "RECOVERY_STEP" ||
                        event.reasonCode === "RECOVERED") && (
                        <Typography
                          variant="body2"
                          color="text.secondary"
                          sx={{ mt: 0.5 }}
                        >
                          {tr("定时恢复")}
                        </Typography>
                      )}
                    </Box>
                  ))}
                </Stack>
                <ErrorAlert error={moreError} onRetry={refreshDetail} />
                {!tail?.done && tail?.nextCursor != null && (
                  <Button
                    onClick={loadMore}
                    disabled={loadingMore}
                    sx={{ mt: 2 }}
                  >
                    {loadingMore ? tr("正在加载…") : tr("加载更多")}
                  </Button>
                )}
              </Paper>
            </Stack>
          )}
        </>
      )}
    </>
  );
}
