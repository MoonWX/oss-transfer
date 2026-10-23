import { t, useI18n, formatDate } from "../lib/i18n";
import { useNodeLabels } from "../lib/workerStatus";
import { taskNodeLabels, type NodeDisplay } from "../lib/nodeRegistry";
import { useSearchParams } from "react-router-dom";
import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import {
  Alert,
  Box,
  Button,
  Checkbox,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Divider,
  LinearProgress,
  Paper,
  Stack,
  Tab,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TablePagination,
  TableRow,
  Tabs,
  TextField,
  Typography,
} from "@mui/material";
import {
  ArrowForwardRounded,
  CompareArrowsRounded,
  DeleteForeverRounded,
  RefreshRounded,
  TravelExploreRounded,
} from "@mui/icons-material";
import { api, ApiError, useApi, useNotify } from "../lib/api";
import type { NodeRegistry } from "../lib/nodeRegistry";
import {
  EmptyState,
  PageHeader,
  StatusChip,
  formatBytes,
  formatNumber,
} from "../components/Common";
type ObjectMetadata = {
  exists?: boolean | null;
  queryStatus?: "FOUND" | "NOT_FOUND" | "UNAVAILABLE";
  statusCode?: number;
  errorCode?: string;
  error?: string;
  contentLength?: number;
  contentType?: string;
  lastModified?: number | string;
  etag?: string;
  userMetadata?: Record<string, string>;
  storageClass?: string;
  contentEncoding?: string;
  contentDisposition?: string;
  cacheControl?: string;
  expirationTime?: number | string;
  httpExpiresDate?: number | string;
};
type Comparison = {
  partial?: boolean;
  sourceMetadata: ObjectMetadata;
  destMetadata: ObjectMetadata;
  verification?: {
    sizeMatch?: boolean | null;
    message?: string;
  };
  sourceBucket?: string;
  sourceKey?: string;
  destBucket?: string;
  destKey?: string;
};
type DeletedTask = {
  taskIndex: number;
  sourceBucket?: string;
  destBucket?: string;
  direction?: string;
  status?: string;
  createTime?: number;
  putObjectCount?: number;
  putObjectSize?: number;
  workerNode?: string;
  localPath?: string;
  executionNodeIp?: string;
  participatingWorkerNodes?: string[];
  sourceEndpoint?: {
    type?: string;
    localPath?: string;
    bucket?: string;
    s3Alias?: string;
    nodeBindings?: {
      localPath?: string;
    }[];
  };
  destinationEndpoint?: DeletedTask["sourceEndpoint"];
};
function deletedEndpoint(task: DeletedTask, source: boolean) {
  const value = source ? task.sourceEndpoint : task.destinationEndpoint;
  const local =
    value?.type === "LOCAL" ||
    (source && task.direction === "LOCAL_TO_OSS") ||
    (!source && task.direction === "OSS_TO_LOCAL");
  if (local)
    return (
      value?.localPath ||
      value?.nodeBindings?.[0]?.localPath ||
      task.localPath ||
      t("本地文件系统")
    );
  return (
    [
      value?.s3Alias,
      value?.bucket || (source ? task.sourceBucket : task.destBucket),
    ]
      .filter(Boolean)
      .join("/") || "—"
  );
}
type ObjectQuery = {
  sourceBucket: string;
  sourceKey: string;
  destBucket: string;
  destKey: string;
};
const metadataFields: {
  key: keyof ObjectMetadata;
  label: string;
  format?: "bytes" | "date";
}[] = [
  { key: "contentLength", label: "对象大小", format: "bytes" },
  { key: "etag", label: "ETag" },
  { key: "contentType", label: "Content-Type" },
  { key: "lastModified", label: "最后修改时间", format: "date" },
  { key: "storageClass", label: "存储类型" },
  { key: "contentEncoding", label: "Content-Encoding" },
  { key: "contentDisposition", label: "Content-Disposition" },
  { key: "cacheControl", label: "Cache-Control" },
  { key: "expirationTime", label: "生命周期过期时间", format: "date" },
  { key: "httpExpiresDate", label: "HTTP 过期时间", format: "date" },
];
const errorText = (error: unknown) =>
  error instanceof Error ? error.message : String(error);
const displayValue = (value: unknown, format?: "bytes" | "date") =>
  value == null || value === ""
    ? "—"
    : format === "bytes"
      ? t("{value1}（{value2} 字节）", {
          value1: formatBytes(Number(value)),
          value2: formatNumber(Number(value)),
        })
      : format === "date"
        ? formatDate(value)
        : String(value);
export default function ToolsPage() {
  useI18n();
  const nodeLabel = useNodeLabels();
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") === "recycle" ? 1 : 0;
  const notify = useNotify();
  const [query, setQuery] = useState<ObjectQuery>({
    sourceBucket: params.get("sourceBucket") || "",
    sourceKey: params.get("sourceKey") || "",
    destBucket: params.get("destBucket") || "",
    destKey: params.get("destKey") || "",
  });
  const [comparison, setComparison] = useState<Comparison | null>(null);
  const [comparing, setComparing] = useState(false);
  const [compareError, setCompareError] = useState<string | null>(null);
  const [onlyDifferences, setOnlyDifferences] = useState(false);
  const linkedParams = useRef(params);
  useEffect(() => {
    const previous = linkedParams.current;
    linkedParams.current = params;
    const fields = ["sourceBucket", "sourceKey", "destBucket", "destKey"];
    if (fields.every((field) => previous.get(field) === params.get(field)))
      return;
    setQuery({
      sourceBucket: params.get("sourceBucket") || "",
      sourceKey: params.get("sourceKey") || "",
      destBucket: params.get("destBucket") || "",
      destKey: params.get("destKey") || "",
    });
    setComparison(null);
    setCompareError(null);
  }, [params]);
  const recycle = useApi<{
    deletedTasks: Record<string, DeletedTask[] | number>;
    totalCount: number;
    complete?: boolean;
    partial?: boolean;
    failedWorkers?: string[];
    unreachableWorkers?: string[];
    queriedWorkers?: string[];
    truncated?: boolean;
    nextBeforeId?: number;
    nodeDisplays?: Record<string, NodeDisplay>;
  }>(tab === 1 ? "/api/master/deleted-tasks" : null);
  const [earlierRows, setEarlierRows] = useState<
    (DeletedTask & {
      workerNode: string;
    })[]
  >([]);
  const [earlierCursor, setEarlierCursor] = useState<number | null>();
  const [earlierLoading, setEarlierLoading] = useState(false);
  const [earlierError, setEarlierError] = useState<string | null>(null);
  const earlierRequest = useRef<AbortController | null>(null);
  useEffect(() => () => earlierRequest.current?.abort(), []);
  const nextEarlierCursor =
    earlierCursor === undefined ? recycle.data?.nextBeforeId : earlierCursor;
  async function loadEarlierRecords() {
    if (!nextEarlierCursor || earlierRequest.current) return;
    const controller = new AbortController();
    earlierRequest.current = controller;
    setEarlierLoading(true);
    setEarlierError(null);
    try {
      const value = await api<{
        deletedTasks: Record<string, DeletedTask[] | number>;
        nextBeforeId?: number;
      }>(`/api/master/deleted-tasks?beforeId=${nextEarlierCursor}&limit=1000`, {
        signal: controller.signal,
      });
      const items = Object.entries(value.deletedTasks).flatMap(
        ([worker, tasks]) =>
          Array.isArray(tasks)
            ? tasks.map((task) => ({ ...task, workerNode: worker }))
            : [],
      );
      setEarlierRows((previous) => [...previous, ...items]);
      setEarlierCursor(value.nextBeforeId ?? null);
    } catch (cause) {
      if (!controller.signal.aborted) setEarlierError(errorText(cause));
    } finally {
      earlierRequest.current = null;
      if (!controller.signal.aborted) setEarlierLoading(false);
    }
  }
  // Recycle completeness belongs to this snapshot, not the live status poll.
  const nodes = useApi<{
    data: NodeRegistry;
  }>(tab === 1 ? "/api/master/nodes" : null);
  const hasMembershipSnapshot = Array.isArray(recycle.data?.queriedWorkers);
  const nodeDiscoveryFailed = !hasMembershipSnapshot && !!nodes.error;
  const membershipKnown = hasMembershipSnapshot || !!nodes.data?.data?.nodes;
  const [selectedIds, setSelectedIds] = useState<number[]>([]);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(25);
  const rows = useMemo(
    () =>
      [
        ...new Map(
          [
            ...earlierRows,
            ...Object.entries(recycle.data?.deletedTasks ?? {}).flatMap(
              ([worker, tasks]) =>
                Array.isArray(tasks)
                  ? tasks.map((task) => ({ ...task, workerNode: worker }))
                  : [],
            ),
          ].map((task) => [`${task.taskIndex}:${task.workerNode}`, task]),
        ).values(),
      ].sort(
        (left, right) =>
          right.taskIndex - left.taskIndex ||
          left.workerNode.localeCompare(right.workerNode),
      ),
    [recycle.data, earlierRows],
  );
  useEffect(() => {
    const lastPage = Math.max(0, Math.ceil(rows.length / pageSize) - 1);
    if (page > lastPage) setPage(lastPage);
  }, [rows.length, page, pageSize]);
  const uniqueIds = [...new Set(rows.map((task) => task.taskIndex))];
  const selectableIds = uniqueIds.slice(0, 128);
  const activeSelection = selectedIds.filter((id) => uniqueIds.includes(id));
  const missingNodes = [
    ...new Set([
      ...(
        recycle.data?.queriedWorkers ??
        nodes.data?.data?.nodes.map((node) => node.address) ??
        []
      ).filter((ip) => !Array.isArray(recycle.data?.deletedTasks?.[ip])),
      ...(recycle.data?.failedWorkers || []),
      ...(recycle.data?.unreachableWorkers || []),
    ]),
  ];
  const userMetadataKeys = [
    ...new Set([
      ...Object.keys(comparison?.sourceMetadata.userMetadata ?? {}),
      ...Object.keys(comparison?.destMetadata.userMetadata ?? {}),
    ]),
  ].sort();
  const same = (a: unknown, b: unknown) => (a ?? null) === (b ?? null);
  const comparable =
    comparison?.sourceMetadata.exists === true &&
    comparison?.destMetadata.exists === true &&
    comparison.sourceMetadata.queryStatus !== "UNAVAILABLE" &&
    comparison.destMetadata.queryStatus !== "UNAVAILABLE";
  const differences = comparison
    ? metadataFields.filter(
        (field) =>
          !same(
            comparison.sourceMetadata[field.key],
            comparison.destMetadata[field.key],
          ),
      ).length +
      userMetadataKeys.filter(
        (key) =>
          !same(
            comparison.sourceMetadata.userMetadata?.[key],
            comparison.destMetadata.userMetadata?.[key],
          ),
      ).length
    : 0;
  const visibleFields = metadataFields.filter(
    (field) =>
      !comparable ||
      !onlyDifferences ||
      !same(
        comparison?.sourceMetadata[field.key],
        comparison?.destMetadata[field.key],
      ),
  );
  const visibleUserKeys = userMetadataKeys.filter(
    (key) =>
      !comparable ||
      !onlyDifferences ||
      !same(
        comparison?.sourceMetadata.userMetadata?.[key],
        comparison?.destMetadata.userMetadata?.[key],
      ),
  );
  async function compare(event: FormEvent) {
    event.preventDefault();
    setComparing(true);
    setCompareError(null);
    setComparison(null);
    setOnlyDifferences(false);
    try {
      const values = {
        ...query,
        sourceBucket: query.sourceBucket.trim(),
        destBucket: query.destBucket.trim(),
      };
      if (
        !values.sourceBucket ||
        !values.destBucket ||
        !values.sourceKey ||
        !values.destKey
      )
        throw new Error(t("请填写源、目标对象的 Bucket 和 Key。"));
      setComparison(
        await api<Comparison>(
          `/api/master/object-metadata?${new URLSearchParams(values)}`,
        ),
      );
    } catch (error) {
      if (
        error instanceof ApiError &&
        error.response?.sourceMetadata &&
        error.response?.destMetadata
      )
        setComparison(error.response as Comparison);
      else setCompareError(errorText(error));
    } finally {
      setComparing(false);
    }
  }
  async function deleteTasks() {
    setDeleting(true);
    setDeleteError(null);
    try {
      const result = await api("/api/master/permanent-delete", {
        method: "POST",
        body: { taskIds: activeSelection },
      });
      setConfirmDelete(false);
      const detail = result.data || result;
      const failedWorkers = [
        ...new Set([
          ...(detail.failedWorkers || []),
          ...(detail.unreachableWorkers || []),
        ]),
      ];
      const partial =
        detail.partial || Number(detail.failed) > 0 || failedWorkers.length > 0;
      const details = Array.isArray(detail.details) ? detail.details : [];
      const removedIds = details
        .filter((item: any) => item.success)
        .map((item: any) => item.taskId);
      setEarlierRows((previous) =>
        previous.filter((task) => !removedIds.includes(task.taskIndex)),
      );
      const retained = [
        ...new Set(
          details.flatMap((item: any) => item.retainedAgentHistory ?? []),
        ),
      ];
      if (!partial) setSelectedIds([]);
      else
        setDeleteError(
          t("{value1}{value2}", {
            value1: detail.message || t("部分记录未删除，请刷新后重试。"),
            value2: failedWorkers.length
              ? t("（未完成节点：{value1}）", {
                  value1: failedWorkers
                    .map((id) =>
                      nodeLabel(String(id), detail?.nodeDisplays?.[String(id)]),
                    )
                    .join(", "),
                })
              : "",
          }),
        );
      setPage(0);
      notify(
        partial
          ? t("部分记录未删除{value1}。", {
              value1: failedWorkers.length
                ? t("，未响应节点：{value1}", {
                    value1: failedWorkers
                      .map((id) =>
                        nodeLabel(
                          String(id),
                          detail?.nodeDisplays?.[String(id)],
                        ),
                      )
                      .join(", "),
                  })
                : t("，请刷新后重试"),
            })
          : retained.length
            ? t("任务记录已删除；已移除节点的本地历史保留：{value1}。", {
                value1: retained
                  .map((id) =>
                    nodeLabel(String(id), detail?.nodeDisplays?.[String(id)]),
                  )
                  .join(", "),
              })
            : t("永久删除请求已提交，正在刷新回收站。"),
        partial ? "warning" : "info",
      );
      await Promise.all([recycle.refresh(), nodes.refresh()]);
    } catch (error) {
      const response = error instanceof ApiError ? error.response : undefined;
      const detail = response?.data || response;
      const failedWorkers: string[] = [
        ...new Set<string>([
          ...(detail?.failedWorkers || []),
          ...(detail?.unreachableWorkers || []),
        ]),
      ];
      const partial =
        detail?.partial ||
        Number(detail?.failed) > 0 ||
        failedWorkers.length > 0;
      const message = t("{value1}{value2}", {
        value1: errorText(error),
        value2: failedWorkers.length
          ? t("（未完成节点：{value1}）", {
              value1: failedWorkers
                .map((id) =>
                  nodeLabel(String(id), detail?.nodeDisplays?.[String(id)]),
                )
                .join(", "),
            })
          : "",
      });
      setDeleteError(message);
      notify(message, partial ? "warning" : "error");
      if (partial) {
        setConfirmDelete(false);
        setPage(0);
        // Keep the selection; activeSelection excludes IDs removed by the fresh response.
        await Promise.all([recycle.refresh(), nodes.refresh()]);
      }
    } finally {
      setDeleting(false);
    }
  }
  return (
    <Stack spacing={3}>
      <PageHeader
        title={t("运维工具")}
        description={t("比对对象元数据，管理回收站任务记录。")}
      />
      <Tabs
        value={tab}
        onChange={(_, value) => {
          const next = new URLSearchParams(params);
          value === 1 ? next.set("tab", "recycle") : next.delete("tab");
          setParams(next, { replace: true });
        }}
        aria-label={t("运维工具")}
        sx={{ borderBottom: 1, borderColor: "divider" }}
      >
        <Tab
          icon={<CompareArrowsRounded fontSize="small" />}
          iconPosition="start"
          label={t("对象比对")}
        />
        <Tab
          icon={<DeleteForeverRounded fontSize="small" />}
          iconPosition="start"
          label={t("任务回收站")}
        />
      </Tabs>
      {tab === 0 && (
        <>
          <Paper
            component="form"
            variant="outlined"
            onSubmit={compare}
            sx={{ p: { xs: 2, md: 3 } }}
          >
            <Stack spacing={3}>
              <Box
                sx={{
                  display: "grid",
                  gridTemplateColumns: { xs: "1fr", md: "1fr 36px 1fr" },
                  gap: 2.5,
                  alignItems: "center",
                }}
              >
                <Stack spacing={2}>
                  <Typography variant="h6">{t("源对象")}</Typography>
                  <TextField
                    required
                    fullWidth
                    label={t("源 Bucket")}
                    placeholder={t("连接名称/bucket-name")}
                    value={query.sourceBucket}
                    onChange={(event) =>
                      setQuery((value) => ({
                        ...value,
                        sourceBucket: event.target.value,
                      }))
                    }
                    helperText={t(
                      "使用已配置的连接名称，例如 storage/my-bucket。",
                    )}
                  />
                  <TextField
                    required
                    fullWidth
                    label={t("源对象 Key")}
                    placeholder="path/to/object"
                    value={query.sourceKey}
                    onChange={(event) =>
                      setQuery((value) => ({
                        ...value,
                        sourceKey: event.target.value,
                      }))
                    }
                  />
                </Stack>
                <ArrowForwardRounded
                  color="disabled"
                  sx={{ display: { xs: "none", md: "block" } }}
                />
                <Stack spacing={2}>
                  <Typography variant="h6">{t("目标对象")}</Typography>
                  <TextField
                    required
                    fullWidth
                    label={t("目标 Bucket")}
                    placeholder={t("连接名称/bucket-name")}
                    value={query.destBucket}
                    onChange={(event) =>
                      setQuery((value) => ({
                        ...value,
                        destBucket: event.target.value,
                      }))
                    }
                    helperText={t(
                      "填写连接名称和存储桶，例如 storage/my-bucket。",
                    )}
                  />
                  <TextField
                    required
                    fullWidth
                    label={t("目标对象 Key")}
                    placeholder="path/to/object"
                    value={query.destKey}
                    onChange={(event) =>
                      setQuery((value) => ({
                        ...value,
                        destKey: event.target.value,
                      }))
                    }
                  />
                </Stack>
              </Box>
              <Stack
                direction={{ xs: "column", sm: "row" }}
                spacing={1.5}
                justifyContent="space-between"
                alignItems={{ sm: "center" }}
              >
                <Button
                  onClick={() =>
                    setQuery((value) => ({
                      ...value,
                      destKey: value.sourceKey,
                    }))
                  }
                  disabled={!query.sourceKey || comparing}
                >
                  {t("目标使用相同 Key")}
                </Button>
                <Button
                  type="submit"
                  variant="contained"
                  disabled={comparing}
                  startIcon={
                    comparing ? (
                      <CircularProgress size={18} color="inherit" />
                    ) : (
                      <TravelExploreRounded />
                    )
                  }
                >
                  {comparing ? t("正在读取元数据…") : t("开始比对")}
                </Button>
              </Stack>
            </Stack>
          </Paper>
          {compareError && <Alert severity="error">{compareError}</Alert>}
          {!comparison && !comparing && !compareError && (
            <EmptyState
              title={t("对象元数据比对")}
              description={t(
                "比较对象大小、ETag、存储类型和自定义元数据，不修改对象。",
              )}
            />
          )}
          {comparison && (
            <Stack spacing={2.5}>
              <Box
                sx={{
                  display: "grid",
                  gridTemplateColumns: {
                    xs: "1fr",
                    md: "repeat(2, minmax(0, 1fr))",
                  },
                  gap: 2,
                }}
              >
                {[
                  {
                    title: t("源对象"),
                    meta: comparison.sourceMetadata,
                    bucket: comparison.sourceBucket,
                    key: comparison.sourceKey,
                  },
                  {
                    title: t("目标对象"),
                    meta: comparison.destMetadata,
                    bucket: comparison.destBucket,
                    key: comparison.destKey,
                  },
                ].map((item) => (
                  <Paper key={item.title} variant="outlined" sx={{ p: 2.5 }}>
                    <Stack
                      direction="row"
                      justifyContent="space-between"
                      alignItems="center"
                      sx={{ mb: 1.5 }}
                    >
                      <Typography variant="subtitle1">{item.title}</Typography>
                      <Chip
                        size="small"
                        variant="outlined"
                        color={
                          item.meta.exists === true
                            ? "success"
                            : item.meta.exists === false
                              ? "default"
                              : "warning"
                        }
                        label={
                          item.meta.queryStatus === "UNAVAILABLE"
                            ? t("查询失败，存在状态未知")
                            : item.meta.exists === true
                              ? t("读取成功")
                              : item.meta.exists === false
                                ? t("对象不存在")
                                : t("存在状态未知")
                        }
                      />
                    </Stack>
                    <Typography
                      variant="body2"
                      sx={{ fontFamily: "monospace", overflowWrap: "anywhere" }}
                    >
                      {item.bucket}/{item.key}
                    </Typography>
                    {item.meta.error && (
                      <Alert severity="error" sx={{ mt: 2 }}>
                        {item.meta.error}
                      </Alert>
                    )}
                    {(item.meta.statusCode || item.meta.errorCode) && (
                      <Typography
                        variant="body2"
                        color="text.secondary"
                        sx={{ mt: 1 }}
                      >
                        {[
                          item.meta.statusCode &&
                            `HTTP ${item.meta.statusCode}`,
                          item.meta.errorCode,
                        ]
                          .filter(Boolean)
                          .join(" · ")}
                      </Typography>
                    )}
                  </Paper>
                ))}
              </Box>
              <Alert
                severity={
                  comparable && comparison.verification?.sizeMatch === true
                    ? "success"
                    : "warning"
                }
              >
                {comparable && comparison.verification?.sizeMatch === true
                  ? t("两个对象的大小一致。")
                  : comparable && comparison.verification?.sizeMatch === false
                    ? t("两个对象的大小不一致，请检查迁移结果。")
                    : comparison.verification?.message ||
                      t("元数据不完整，无法完成比对。")}{" "}
                {t("对象大小和 ETag 的比较不等同于完整内容校验。")}
              </Alert>
              <Paper variant="outlined" sx={{ overflow: "hidden" }}>
                <Stack
                  direction={{ xs: "column", sm: "row" }}
                  spacing={1}
                  alignItems={{ sm: "center" }}
                  justifyContent="space-between"
                  sx={{ p: 2.5 }}
                >
                  <Box>
                    <Typography variant="h6">{t("元数据差异")}</Typography>
                    <Typography variant="body2" color="text.secondary">
                      {comparable
                        ? t("{value1} 个字段不同", {
                            value1: differences,
                          })
                        : t("元数据不完整，无法统计差异")}
                    </Typography>
                  </Box>
                  <Button
                    size="small"
                    variant={onlyDifferences ? "contained" : "outlined"}
                    disabled={!comparable}
                    onClick={() => setOnlyDifferences((value) => !value)}
                  >
                    {onlyDifferences ? t("显示全部字段") : t("仅看差异")}
                  </Button>
                </Stack>
                <TableContainer>
                  <Table sx={{ minWidth: 560 }}>
                    <TableHead>
                      <TableRow>
                        <TableCell sx={{ width: "24%" }}>{t("字段")}</TableCell>
                        <TableCell sx={{ width: "38%" }}>
                          {t("源对象")}
                        </TableCell>
                        <TableCell sx={{ width: "38%" }}>
                          {t("目标对象")}
                        </TableCell>
                      </TableRow>
                    </TableHead>
                    <TableBody>
                      {visibleFields.map((field) => {
                        const different =
                          comparable &&
                          !same(
                            comparison.sourceMetadata[field.key],
                            comparison.destMetadata[field.key],
                          );
                        return (
                          <TableRow
                            key={field.key}
                            sx={{
                              bgcolor: different
                                ? "action.selected"
                                : undefined,
                            }}
                          >
                            <TableCell>
                              <Typography variant="body2">
                                {t(field.label)}
                              </Typography>
                              {different && (
                                <Typography
                                  variant="caption"
                                  color="warning.main"
                                >
                                  {t("存在差异")}
                                </Typography>
                              )}
                            </TableCell>
                            <TableCell sx={{ overflowWrap: "anywhere" }}>
                              {displayValue(
                                comparison.sourceMetadata[field.key],
                                field.format,
                              )}
                            </TableCell>
                            <TableCell sx={{ overflowWrap: "anywhere" }}>
                              {displayValue(
                                comparison.destMetadata[field.key],
                                field.format,
                              )}
                            </TableCell>
                          </TableRow>
                        );
                      })}
                      {visibleUserKeys.length > 0 && (
                        <TableRow>
                          <TableCell colSpan={3}>
                            <Typography variant="subtitle2">
                              {t("自定义元数据")}
                            </Typography>
                          </TableCell>
                        </TableRow>
                      )}
                      {visibleUserKeys.map((key) => (
                        <TableRow
                          key={`user-${key}`}
                          sx={{
                            bgcolor:
                              comparable &&
                              !same(
                                comparison.sourceMetadata.userMetadata?.[key],
                                comparison.destMetadata.userMetadata?.[key],
                              )
                                ? "action.selected"
                                : undefined,
                          }}
                        >
                          <TableCell
                            sx={{
                              overflowWrap: "anywhere",
                              fontFamily: "monospace",
                            }}
                          >
                            {key}
                          </TableCell>
                          <TableCell sx={{ overflowWrap: "anywhere" }}>
                            {displayValue(
                              comparison.sourceMetadata.userMetadata?.[key],
                            )}
                          </TableCell>
                          <TableCell sx={{ overflowWrap: "anywhere" }}>
                            {displayValue(
                              comparison.destMetadata.userMetadata?.[key],
                            )}
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </TableContainer>
                {comparable && onlyDifferences && differences === 0 && (
                  <EmptyState
                    title={t("没有元数据差异")}
                    description={t("本次返回的元数据字段一致。")}
                  />
                )}
              </Paper>
            </Stack>
          )}
        </>
      )}
      {tab === 1 && (
        <>
          <Alert severity="info">
            {t(
              "回收站保留已标记删除的任务。永久删除仅清理任务记录，无法恢复；不删除源端或目标端的数据对象。",
            )}
          </Alert>
          {recycle.error && <Alert severity="error">{recycle.error}</Alert>}
          {deleteError && <Alert severity="warning">{deleteError}</Alert>}
          {nodeDiscoveryFailed && (
            <Alert severity="warning">
              {t("节点清单读取失败，无法确认回收站是否覆盖全部节点。")}
            </Alert>
          )}
          {recycle.data && missingNodes.length > 0 && (
            <Alert severity="warning">
              {t("以下节点未返回回收站数据，当前列表可能不完整：{nodes}", {
                nodes: missingNodes
                  .map((id) => nodeLabel(id, recycle.data?.nodeDisplays?.[id]))
                  .join(", "),
              })}
            </Alert>
          )}
          {(recycle.data?.partial || recycle.data?.complete === false) &&
            !recycle.data?.truncated &&
            missingNodes.length === 0 && (
              <Alert severity="warning">
                {t("部分回收站记录未返回，当前列表可能不完整。")}
              </Alert>
            )}
          {nextEarlierCursor && (
            <Alert
              severity="info"
              action={
                <Button
                  disabled={earlierLoading}
                  onClick={() => void loadEarlierRecords()}
                >
                  {earlierLoading ? t("加载中") : t("加载更早记录")}
                </Button>
              }
            >
              {t("已加载 {count} 条记录。", {
                count: formatNumber(rows.length),
              })}
            </Alert>
          )}
          {earlierError && <Alert severity="error">{earlierError}</Alert>}
          <Paper variant="outlined" sx={{ overflow: "hidden" }}>
            <Stack
              direction={{ xs: "column", sm: "row" }}
              spacing={2}
              justifyContent="space-between"
              alignItems={{ sm: "center" }}
              sx={{ p: 2.5 }}
            >
              <Box>
                <Typography variant="h6">{t("已删除任务")}</Typography>
                <Typography variant="body2" color="text.secondary">
                  {recycle.data
                    ? t("{value1} 条节点任务记录{value2}", {
                        value1: formatNumber(rows.length),
                        value2: activeSelection.length
                          ? t(" · 已选择 {value1} 个任务 ID（最多 128 个）", {
                              value1: activeSelection.length,
                            })
                          : "",
                      })
                    : t("正在读取回收站")}
                </Typography>
              </Box>
              <Stack direction="row" spacing={1}>
                <Button
                  startIcon={<RefreshRounded />}
                  disabled={recycle.loading}
                  onClick={() => {
                    earlierRequest.current?.abort();
                    earlierRequest.current = null;
                    setEarlierLoading(false);
                    setEarlierRows([]);
                    setEarlierCursor(undefined);
                    setEarlierError(null);
                    void recycle.refresh();
                    void nodes.refresh();
                  }}
                >
                  {t("刷新")}
                </Button>
                <Button
                  variant="outlined"
                  color="error"
                  startIcon={<DeleteForeverRounded />}
                  disabled={
                    !activeSelection.length || deleting || recycle.loading
                  }
                  onClick={() => setConfirmDelete(true)}
                >
                  {t("永久删除")}
                </Button>
              </Stack>
            </Stack>
            <Box sx={{ height: 4 }}>
              <LinearProgress
                aria-label={t("刷新回收站")}
                sx={{ visibility: recycle.loading ? "visible" : "hidden" }}
              />
            </Box>
            {rows.length > 0 ? (
              <>
                <TableContainer>
                  <Table size="small" sx={{ minWidth: 700 }}>
                    <TableHead>
                      <TableRow>
                        <TableCell padding="checkbox">
                          <Checkbox
                            inputProps={{
                              "aria-label":
                                uniqueIds.length > 128
                                  ? t("选择前 128 个已删除任务")
                                  : t("选择全部已删除任务"),
                            }}
                            checked={
                              selectableIds.length > 0 &&
                              selectableIds.every((id) =>
                                activeSelection.includes(id),
                              )
                            }
                            indeterminate={
                              activeSelection.length > 0 &&
                              !selectableIds.every((id) =>
                                activeSelection.includes(id),
                              )
                            }
                            onChange={(event) =>
                              setSelectedIds(
                                event.target.checked ? selectableIds : [],
                              )
                            }
                          />
                        </TableCell>
                        <TableCell>{t("任务")}</TableCell>
                        <TableCell>{t("迁移路径")}</TableCell>
                        <TableCell>{t("节点")}</TableCell>
                        <TableCell>{t("最终状态")}</TableCell>
                        <TableCell align="right">{t("已传输")}</TableCell>
                      </TableRow>
                    </TableHead>
                    <TableBody>
                      {rows
                        .slice(page * pageSize, page * pageSize + pageSize)
                        .map((task) => (
                          <TableRow
                            hover
                            selected={activeSelection.includes(task.taskIndex)}
                            key={`${task.workerNode}-${task.taskIndex}`}
                          >
                            <TableCell padding="checkbox">
                              <Checkbox
                                inputProps={{
                                  "aria-label": t("选择任务 {value1}", {
                                    value1: task.taskIndex,
                                  }),
                                }}
                                checked={activeSelection.includes(
                                  task.taskIndex,
                                )}
                                disabled={
                                  activeSelection.length >= 128 &&
                                  !activeSelection.includes(task.taskIndex)
                                }
                                onChange={(event) =>
                                  setSelectedIds((ids) =>
                                    event.target.checked
                                      ? [
                                          ...new Set([...ids, task.taskIndex]),
                                        ].slice(0, 128)
                                      : ids.filter(
                                          (id) => id !== task.taskIndex,
                                        ),
                                  )
                                }
                              />
                            </TableCell>
                            <TableCell>
                              <Typography variant="body2">
                                #{task.taskIndex}
                              </Typography>
                              <Typography
                                variant="caption"
                                color="text.secondary"
                              >
                                {task.createTime
                                  ? formatDate(task.createTime, {
                                      year: "numeric",
                                      month: "numeric",
                                      day: "numeric",
                                    })
                                  : "—"}
                              </Typography>
                            </TableCell>
                            <TableCell sx={{ maxWidth: 330 }}>
                              <Typography
                                variant="body2"
                                sx={{ overflowWrap: "anywhere" }}
                              >
                                {deletedEndpoint(task, true)}
                              </Typography>
                              <Typography
                                variant="caption"
                                color="text.secondary"
                                sx={{ overflowWrap: "anywhere" }}
                              >
                                → {deletedEndpoint(task, false)}
                              </Typography>
                            </TableCell>
                            <TableCell
                              sx={{
                                fontFamily: "monospace",
                                whiteSpace: "nowrap",
                              }}
                            >
                              {taskNodeLabels(task, nodeLabel)}
                            </TableCell>
                            <TableCell>
                              <StatusChip status={task.status || "UNKNOWN"} />
                            </TableCell>
                            <TableCell
                              align="right"
                              sx={{ whiteSpace: "nowrap" }}
                            >
                              <Typography variant="body2">
                                {task.putObjectCount === undefined
                                  ? "—"
                                  : t("{value1} 个对象", {
                                      value1: formatNumber(task.putObjectCount),
                                    })}
                              </Typography>
                              <Typography
                                variant="caption"
                                color="text.secondary"
                              >
                                {task.putObjectSize === undefined
                                  ? "—"
                                  : formatBytes(task.putObjectSize)}
                              </Typography>
                            </TableCell>
                          </TableRow>
                        ))}
                    </TableBody>
                  </Table>
                </TableContainer>
                <TablePagination
                  component="div"
                  count={rows.length}
                  page={page}
                  onPageChange={(_, next) => setPage(next)}
                  rowsPerPage={pageSize}
                  onRowsPerPageChange={(event) => {
                    setPageSize(Number(event.target.value));
                    setPage(0);
                  }}
                  rowsPerPageOptions={[25, 50, 100]}
                  labelRowsPerPage={t("每页")}
                  labelDisplayedRows={({ from, to, count }) =>
                    `${from}–${to} / ${count}`
                  }
                  getItemAriaLabel={(type) =>
                    type === "next"
                      ? t("下一页")
                      : type === "previous"
                        ? t("上一页")
                        : type === "first"
                          ? t("第一页")
                          : t("最后一页")
                  }
                />
              </>
            ) : (
              !recycle.error &&
              !!recycle.data &&
              !recycle.data.partial &&
              recycle.data.complete !== false &&
              !nodeDiscoveryFailed &&
              membershipKnown &&
              missingNodes.length === 0 && (
                <EmptyState
                  title={t("暂无已删除任务")}
                  description={t("从任务列表删除的记录会移入回收站。")}
                />
              )
            )}
          </Paper>
          <Dialog
            open={confirmDelete}
            onClose={() => !deleting && setConfirmDelete(false)}
            maxWidth="sm"
            fullWidth
          >
            <DialogTitle>
              {t("永久删除 {count} 个任务？", {
                count: activeSelection.length,
              })}
            </DialogTitle>
            <DialogContent>
              <Alert severity="warning" sx={{ mb: 2 }}>
                {t(
                  "此操作无法撤销，将清理所有执行节点上同 ID 的已删除任务记录。",
                )}
              </Alert>
              <Typography variant="body2" sx={{ overflowWrap: "anywhere" }}>
                {t("任务 ID：{ids}", {
                  ids: activeSelection.map((id) => `#${id}`).join(", "),
                })}
              </Typography>
              <Divider sx={{ my: 2 }} />
              <Typography variant="body2" color="text.secondary">
                {t("源端和目标端的数据对象不受影响。")}
              </Typography>
            </DialogContent>
            <DialogActions>
              <Button
                disabled={deleting}
                onClick={() => setConfirmDelete(false)}
              >
                {t("取消")}
              </Button>
              <Button
                variant="contained"
                color="error"
                disabled={deleting}
                startIcon={
                  deleting ? (
                    <CircularProgress size={16} color="inherit" />
                  ) : (
                    <DeleteForeverRounded />
                  )
                }
                onClick={() => void deleteTasks()}
              >
                {t("确认永久删除")}
              </Button>
            </DialogActions>
          </Dialog>
        </>
      )}
    </Stack>
  );
}
