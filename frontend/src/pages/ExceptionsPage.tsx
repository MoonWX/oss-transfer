import { useWorkerStatus } from "../lib/workerStatus";
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
} from "react";
import { useSearchParams } from "react-router-dom";
import {
  Alert,
  Box,
  Button,
  Chip,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Divider,
  LinearProgress,
  MenuItem,
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
  DeleteOutlineRounded,
  DownloadRounded,
  FilterAltRounded,
  RefreshRounded,
  ReplayRounded,
} from "@mui/icons-material";
import { api, ApiError, download, useApi, useNotify } from "../lib/api";
import {
  EmptyState,
  PageHeader,
  formatBytes,
  formatNumber,
} from "../components/Common";

type Failure = {
  taskIndex?: number;
  parentTaskId?: number;
  workerNode?: string;
  executionNodeIp?: string;
  sourceBucket?: string;
  destBucket?: string;
  key?: string;
  destinationKey?: string;
  localPath?: string;
  errorMessage?: string;
  errorCode?: string;
  objectSize?: number;
  stage?: string;
  retryCount?: number;
  timestamp?: number;
  timestampFormatted?: string;
  direction?: string;
  tmpFilePath?: string;
  storageId?: string;
  [key: string]: unknown;
};
type Filters = {
  task: string;
  source: string;
  destination: string;
  node: string;
  direction: string;
  endpoint: string;
  limit: string;
};
type Result = {
  items: Failure[];
  total: number;
  partial?: boolean;
  unreachableWorkers?: string[];
  failedWorkers?: string[];
  workerErrors?: Record<string, string>;
  respondingNodes?: string[];
};
const initialFilters: Filters = {
  task: "",
  source: "",
  destination: "",
  node: "",
  direction: "",
  endpoint: "",
  limit: "1000",
};
const modes = ["failed", "fallback", "local"] as const;
const labels = ["失败对象", "降级迁移记录", "本地迁移失败"];
const errorText = (error: unknown) =>
  error instanceof Error ? error.message : String(error);
const failureTime = (item: Failure) =>
  item.timestampFormatted ||
  (item.timestamp ? new Date(item.timestamp).toLocaleString() : "—");

function exportRows(rows: Failure[], name: string) {
  const fields = [
    "workerNode",
    "taskIndex",
    "parentTaskId",
    "direction",
    "sourceBucket",
    "destBucket",
    "key",
    "destinationKey",
    "localPath",
    "stage",
    "errorCode",
    "errorMessage",
    "objectSize",
    "retryCount",
    "timestampFormatted",
    "tmpFilePath",
    "storageId",
  ];
  const cell = (value: unknown) => {
    let text =
      value == null
        ? ""
        : typeof value === "object"
          ? JSON.stringify(value)
          : String(value);
    if (/^[=+\-@\t\r]/.test(text)) text = `'${text}`;
    return `"${text.replaceAll('"', '""')}"`;
  };
  const csv =
    "\uFEFF" +
    [
      fields.map(cell).join(","),
      ...rows.map((row) => fields.map((field) => cell(row[field])).join(",")),
    ].join("\r\n");
  const url = URL.createObjectURL(
    new Blob([csv], { type: "text/csv;charset=utf-8;" }),
  );
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = name;
  anchor.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

export default function ExceptionsPage() {
  const notify = useNotify();
  const [params] = useSearchParams();
  const linkedFilters = {
    ...initialFilters,
    task: /^\d+$/.test(params.get("taskId") || "") ? params.get("taskId")! : "",
  };
  const nodes = useWorkerStatus();
  const [tab, setTab] = useState(params.get("type") === "local" ? 2 : 0);
  const [draft, setDraft] = useState<Filters>(linkedFilters);
  const [filters, setFilters] = useState<Filters>(linkedFilters);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(25);
  const [result, setResult] = useState<Result | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirmClear, setConfirmClear] = useState(false);
  const [clearError, setClearError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Failure | null>(null);
  const loadId = useRef(0);
  const linkedParams = useRef(params);
  useEffect(() => {
    const previous = linkedParams.current;
    linkedParams.current = params;
    if (
      previous.get("taskId") === params.get("taskId") &&
      previous.get("type") === params.get("type")
    )
      return;
    const nextFilters = {
      ...initialFilters,
      task: /^\d+$/.test(params.get("taskId") || "")
        ? params.get("taskId")!
        : "",
    };
    setTab(params.get("type") === "local" ? 2 : 0);
    setDraft(nextFilters);
    setFilters(nextFilters);
    setPage(0);
    setResult(null);
    setSelected(null);
  }, [params]);
  const mode = modes[tab];
  useEffect(() => setClearError(null), [mode]);
  const isLocal = mode === "local";
  const localPage = isLocal ? page : 0;
  const localSize = isLocal ? pageSize : 25;

  const load = useCallback(async () => {
    const id = ++loadId.current;
    setLoading(true);
    setError(null);
    try {
      let next: Result;
      if (mode === "local") {
        const params = new URLSearchParams({
          offset: String(localPage * localSize),
          limit: String(localSize),
        });
        if (filters.task.trim()) params.set("taskId", filters.task.trim());
        if (filters.node) params.set("executionNodeIp", filters.node);
        if (filters.direction) params.set("direction", filters.direction);
        if (filters.endpoint.trim())
          params.set("endpoint", filters.endpoint.trim());
        next = (
          await api<{ data: Result }>(
            `/api/master/local-transfers/failures?${params}`,
          )
        ).data;
      } else {
        const body: Record<string, unknown> = {
          limit: Number(filters.limit) || 1000,
        };
        if (filters.task.trim()) body.taskIndex = Number(filters.task);
        if (filters.source.trim()) body.sourceBucket = filters.source.trim();
        if (filters.destination.trim())
          body.destBucket = filters.destination.trim();
        const raw = await api<Record<string, unknown>>(`/api/master/${mode}`, {
          method: "POST",
          body,
        });
        const grouped = (raw[
          mode === "failed" ? "failedKeys" : "fallbackObjects"
        ] ?? {}) as Record<string, Failure[] | number>;
        const respondingNodes = Object.entries(grouped)
          .filter(([, value]) => Array.isArray(value))
          .map(([worker]) => worker);
        const items = Object.entries(grouped).flatMap(([worker, value]) =>
          Array.isArray(value) && (!filters.node || worker === filters.node)
            ? value.map((row) => ({ ...row, workerNode: worker }))
            : [],
        );
        const total = Object.entries(grouped).reduce(
          (sum, [key, value]) =>
            key.endsWith("Count") &&
            typeof value === "number" &&
            (!filters.node || key === `${filters.node}Count`)
              ? sum + value
              : sum,
          0,
        );
        next = {
          items,
          total,
          respondingNodes,
          partial: raw.partial === true,
          unreachableWorkers: Array.isArray(raw.unreachableWorkers)
            ? (raw.unreachableWorkers as string[])
            : [],
          failedWorkers: Array.isArray(raw.failedWorkers)
            ? (raw.failedWorkers as string[])
            : [],
          workerErrors: raw.workerErrors as Record<string, string> | undefined,
        };
      }
      if (id === loadId.current) setResult(next);
    } catch (cause) {
      if (id === loadId.current) {
        setError(errorText(cause));
        setResult(null);
      }
    } finally {
      if (id === loadId.current) setLoading(false);
    }
  }, [mode, filters, localPage, localSize]);
  useEffect(() => {
    void load();
    return () => {
      loadId.current++;
    };
  }, [load]);
  useEffect(() => {
    if (!result) return;
    const count = isLocal ? result.total : result.items.length;
    const lastPage = Math.max(0, Math.ceil(count / pageSize) - 1);
    if (page > lastPage) setPage(lastPage);
  }, [result, isLocal, page, pageSize]);
  const expectedNodes = Object.keys(nodes.data?.workers ?? {}).filter(
    (node) => !filters.node || node === filters.node,
  );
  const missingNodes = [
    ...new Set([
      ...(!isLocal && result
        ? expectedNodes.filter(
            (node) => !result.respondingNodes?.includes(node),
          )
        : []),
      ...(result?.unreachableWorkers ?? []),
      ...(result?.failedWorkers ?? []),
    ]),
  ];
  const visible = result?.items
    ? isLocal
      ? result.items
      : result.items.slice(page * pageSize, page * pageSize + pageSize)
    : [];
  const updateDraft = (field: keyof Filters, value: string) =>
    setDraft((previous) => ({ ...previous, [field]: value }));
  const applyFilters = (event: FormEvent) => {
    event.preventDefault();
    if (
      draft.task &&
      (!Number.isInteger(Number(draft.task)) || Number(draft.task) < 0)
    ) {
      notify("任务 ID 必须是非负整数。", "error");
      return;
    }
    setPage(0);
    setFilters({ ...draft });
  };
  async function clearRecords() {
    setBusy(true);
    setClearError(null);
    try {
      const value = await api(`/api/master/${mode}/clear`, { method: "POST" });
      if (
        value.partial ||
        value.failedWorkers?.length ||
        value.unreachableWorkers?.length
      ) {
        const message = value.message || "部分节点未能清除记录，请刷新后重试。";
        setClearError(message);
        notify(message, "warning");
      } else notify("清除请求已提交，正在刷新各节点记录。", "info");
      setConfirmClear(false);
      setPage(0);
      await load();
    } catch (cause) {
      const value = cause instanceof ApiError ? cause.response : undefined;
      const failedNodes = [
        ...new Set([
          ...(value?.failedWorkers ?? []),
          ...(value?.unreachableWorkers ?? []),
        ]),
      ];
      const message = `${errorText(cause)}${failedNodes.length ? `（未完成节点：${failedNodes.join("、")}）` : ""}`;
      setClearError(message);
      notify(message, value?.partial ? "warning" : "error");
      if (value?.partial) {
        setConfirmClear(false);
        setPage(0);
        await load();
      }
    } finally {
      setBusy(false);
    }
  }
  async function retry(item: Failure) {
    const taskId = item.parentTaskId ?? item.taskIndex;
    if (taskId === undefined) return;
    setBusy(true);
    try {
      const response = await api<{
        data: {
          accepted: number;
          ignored: number;
          failed: number;
          partial?: boolean;
          unreachableWorkers?: string[];
        };
      }>("/api/master/local-transfers/retry-failures", {
        method: "POST",
        body: { taskIds: [taskId] },
      });
      const value = response.data;
      notify(
        `重试已受理 ${value.accepted}，忽略 ${value.ignored}，失败 ${value.failed}${value.partial ? `；不可达节点：${value.unreachableWorkers?.join("、") || "未知"}` : ""}`,
        value.partial || value.failed > 0 ? "warning" : "success",
      );
      await load();
    } catch (cause) {
      notify(errorText(cause), "error");
    } finally {
      setBusy(false);
    }
  }
  async function report() {
    setBusy(true);
    try {
      await download(
        "/api/master/local-transfers/report",
        "local-transfer-report.zip",
      );
    } catch (cause) {
      notify(errorText(cause), "error");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Stack spacing={3}>
      <PageHeader
        title="异常记录"
        description="查看失败对象和降级记录，导出异常明细。"
        action={
          <Button
            startIcon={<RefreshRounded />}
            onClick={() => {
              void load();
              void nodes.refresh();
            }}
            disabled={loading}
          >
            刷新
          </Button>
        }
      />
      <Tabs
        value={tab}
        onChange={(_, value) => {
          setTab(value);
          setPage(0);
          setResult(null);
          setDraft(initialFilters);
          setFilters(initialFilters);
        }}
        variant="scrollable"
        scrollButtons="auto"
        aria-label="异常记录类型"
        sx={{ borderBottom: 1, borderColor: "divider" }}
      >
        {labels.map((label) => (
          <Tab key={label} label={label} />
        ))}
      </Tabs>
      <Paper
        variant="outlined"
        component="form"
        onSubmit={applyFilters}
        sx={{ p: 2.5 }}
      >
        <Stack spacing={2}>
          <Box
            sx={{
              display: "grid",
              gridTemplateColumns: {
                xs: "1fr",
                sm: "repeat(2, minmax(0, 1fr))",
                lg: "repeat(4, minmax(0, 1fr))",
              },
              gap: 2,
            }}
          >
            <TextField
              label="任务 ID"
              size="small"
              type="number"
              value={draft.task}
              onChange={(event) => updateDraft("task", event.target.value)}
              slotProps={{ htmlInput: { min: 0, step: 1 } }}
            />
            <TextField
              select
              label="执行节点"
              size="small"
              value={draft.node}
              onChange={(event) => updateDraft("node", event.target.value)}
            >
              <MenuItem value="">全部节点</MenuItem>
              {Object.keys(nodes.data?.workers ?? {}).map((ip) => (
                <MenuItem key={ip} value={ip}>
                  {ip}
                </MenuItem>
              ))}
            </TextField>
            {isLocal ? (
              <>
                <TextField
                  select
                  label="迁移方向"
                  size="small"
                  value={draft.direction}
                  onChange={(event) =>
                    updateDraft("direction", event.target.value)
                  }
                >
                  <MenuItem value="">全部方向</MenuItem>
                  <MenuItem value="OSS_TO_LOCAL">OSS → 本地</MenuItem>
                  <MenuItem value="LOCAL_TO_OSS">本地 → OSS</MenuItem>
                </TextField>
                <TextField
                  label="端点 / 路径"
                  size="small"
                  value={draft.endpoint}
                  onChange={(event) =>
                    updateDraft("endpoint", event.target.value)
                  }
                  sx={{ gridColumn: { sm: "span 2" } }}
                />
              </>
            ) : (
              <>
                <TextField
                  label="源 Bucket"
                  size="small"
                  value={draft.source}
                  onChange={(event) =>
                    updateDraft("source", event.target.value)
                  }
                />
                <TextField
                  label="目标 Bucket"
                  size="small"
                  value={draft.destination}
                  onChange={(event) =>
                    updateDraft("destination", event.target.value)
                  }
                />
                <TextField
                  select
                  label="每节点读取上限"
                  size="small"
                  value={draft.limit}
                  onChange={(event) => updateDraft("limit", event.target.value)}
                >
                  {["100", "1000", "5000", "10000"].map((value) => (
                    <MenuItem key={value} value={value}>
                      {formatNumber(Number(value))} 条
                    </MenuItem>
                  ))}
                </TextField>
              </>
            )}
          </Box>
          <Stack direction="row" spacing={1}>
            <Button
              type="submit"
              variant="contained"
              startIcon={<FilterAltRounded />}
              disabled={loading}
            >
              应用筛选
            </Button>
            <Button
              onClick={() => {
                setDraft(initialFilters);
                setFilters({ ...initialFilters });
                setPage(0);
              }}
              disabled={loading}
            >
              重置
            </Button>
          </Stack>
        </Stack>
      </Paper>
      {error && <Alert severity="error">{error}</Alert>}
      {nodes.error && (
        <Alert severity="warning">
          节点清单读取失败，无法确认结果是否覆盖全部节点。
        </Alert>
      )}
      {clearError && <Alert severity="warning">{clearError}</Alert>}
      {(result?.partial || missingNodes.length > 0) && (
        <Alert severity="warning">
          部分节点未返回数据，当前结果不完整。
          {missingNodes.length > 0 && `不可达节点：${missingNodes.join("、")}`}
          {Object.entries(result?.workerErrors ?? {}).map(([node, message]) => (
            <Typography key={node} variant="body2">
              {node}：{message}
            </Typography>
          ))}
        </Alert>
      )}
      {!isLocal && result && result.total > result.items.length && (
        <Alert severity="info">
          共查询到 {formatNumber(result.total)} 条记录，当前加载{" "}
          {formatNumber(result.items.length)}{" "}
          条。可提高每节点读取上限或缩小筛选范围。
        </Alert>
      )}
      {mode === "fallback" && (
        <Alert severity="info">
          降级记录表示对象曾切换到备用传输方式。请查看对应任务状态确认结果。
        </Alert>
      )}
      <Paper variant="outlined" sx={{ overflow: "hidden" }}>
        <Stack
          direction={{ xs: "column", sm: "row" }}
          spacing={2}
          justifyContent="space-between"
          alignItems={{ sm: "center" }}
          sx={{ p: 2.5 }}
        >
          <Box>
            <Typography variant="h6">{labels[tab]}</Typography>
            <Typography variant="body2" color="text.secondary">
              {result
                ? `${formatNumber(result.total)} 条记录${!isLocal ? ` · 已加载 ${formatNumber(result.items.length)} 条` : ""}`
                : error
                  ? "读取失败"
                  : "正在读取记录"}
            </Typography>
          </Box>
          <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
            <Button
              size="small"
              startIcon={<DownloadRounded />}
              disabled={!result?.items.length || loading}
              onClick={() =>
                exportRows(result?.items ?? [], `${mode}-records.csv`)
              }
            >
              {isLocal ? "导出当前页" : "导出已加载记录"}
            </Button>
            {isLocal ? (
              <Button
                size="small"
                variant="outlined"
                startIcon={<DownloadRounded />}
                onClick={() => void report()}
                disabled={busy}
              >
                完整迁移报告
              </Button>
            ) : (
              <Button
                size="small"
                color="error"
                startIcon={<DeleteOutlineRounded />}
                onClick={() => setConfirmClear(true)}
                disabled={busy || loading}
              >
                清除全部记录
              </Button>
            )}
          </Stack>
        </Stack>
        {loading && <LinearProgress />}
        {visible.length > 0 ? (
          <TableContainer>
            <Table size="small" sx={{ minWidth: 780 }}>
              <TableHead>
                <TableRow>
                  <TableCell>对象 / 路径</TableCell>
                  <TableCell>任务与节点</TableCell>
                  <TableCell>原因</TableCell>
                  <TableCell>记录时间</TableCell>
                  <TableCell align="right">操作</TableCell>
                </TableRow>
              </TableHead>
              <TableBody>
                {visible.map((item, index) => (
                  <TableRow
                    key={`${item.workerNode}-${item.taskIndex}-${item.key}-${index}`}
                    hover
                  >
                    <TableCell sx={{ maxWidth: 310 }}>
                      <Typography
                        variant="body2"
                        sx={{
                          overflowWrap: "anywhere",
                          fontFamily: "monospace",
                        }}
                      >
                        {item.key || item.localPath || "—"}
                      </Typography>
                      {item.destinationKey != null &&
                        item.destinationKey !== item.key && (
                          <Typography
                            variant="body2"
                            sx={{
                              fontFamily: "monospace",
                              overflowWrap: "anywhere",
                              whiteSpace: "pre-wrap",
                            }}
                          >
                            目标：{item.destinationKey}
                          </Typography>
                        )}
                      <Typography
                        variant="caption"
                        color="text.secondary"
                        sx={{ overflowWrap: "anywhere" }}
                      >
                        {item.sourceBucket || "—"} →{" "}
                        {item.destBucket || item.localPath || "—"}
                      </Typography>
                      {item.objectSize !== undefined && (
                        <Typography
                          variant="caption"
                          color="text.secondary"
                          display="block"
                        >
                          {formatBytes(item.objectSize)}
                        </Typography>
                      )}
                    </TableCell>
                    <TableCell sx={{ whiteSpace: "nowrap" }}>
                      <Typography variant="body2">
                        #{item.parentTaskId ?? item.taskIndex ?? "—"}
                      </Typography>
                      <Typography
                        variant="caption"
                        color="text.secondary"
                        sx={{ fontFamily: "monospace" }}
                      >
                        {item.workerNode || item.executionNodeIp || "调度阶段"}
                      </Typography>
                      {item.stage && (
                        <Chip
                          size="small"
                          variant="outlined"
                          label={item.stage}
                          sx={{
                            display: "flex",
                            width: "fit-content",
                            mt: 0.5,
                          }}
                        />
                      )}
                    </TableCell>
                    <TableCell sx={{ maxWidth: 300 }}>
                      <Typography
                        variant="body2"
                        color="error.main"
                        sx={{
                          overflowWrap: "anywhere",
                          display: "-webkit-box",
                          WebkitLineClamp: 3,
                          WebkitBoxOrient: "vertical",
                          overflow: "hidden",
                        }}
                      >
                        {item.errorMessage || item.errorCode || "未提供原因"}
                      </Typography>
                      {item.errorCode && (
                        <Typography variant="caption" color="text.secondary">
                          {item.errorCode}
                        </Typography>
                      )}
                    </TableCell>
                    <TableCell sx={{ whiteSpace: "nowrap" }}>
                      <Typography variant="body2">
                        {failureTime(item)}
                      </Typography>
                    </TableCell>
                    <TableCell align="right">
                      <Button size="small" onClick={() => setSelected(item)}>
                        详情
                      </Button>
                      {isLocal &&
                        (item.parentTaskId !== undefined ||
                          item.taskIndex !== undefined) && (
                          <Button
                            size="small"
                            startIcon={<ReplayRounded />}
                            onClick={() => void retry(item)}
                            disabled={busy}
                          >
                            重试任务
                          </Button>
                        )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableContainer>
        ) : (
          !loading &&
          !error && (
            <EmptyState
              title={
                result && result.total > 0 ? "当前页无记录" : "暂无匹配记录"
              }
              description={
                result && result.total > 0
                  ? "每个节点最多返回 1,000 条本地失败记录。请按任务或节点缩小筛选范围。"
                  : "请调整筛选条件，或在任务执行后刷新。"
              }
            />
          )
        )}
        <TablePagination
          component="div"
          count={isLocal ? (result?.total ?? 0) : (result?.items.length ?? 0)}
          page={page}
          onPageChange={(_, value) => setPage(value)}
          rowsPerPage={pageSize}
          onRowsPerPageChange={(event) => {
            setPageSize(Number(event.target.value));
            setPage(0);
          }}
          rowsPerPageOptions={[25, 50, 100]}
          labelRowsPerPage="每页"
          labelDisplayedRows={({ from, to, count }) =>
            `${from}–${to} / ${count}`
          }
          getItemAriaLabel={(type) =>
            type === "next"
              ? "下一页"
              : type === "previous"
                ? "上一页"
                : type === "first"
                  ? "第一页"
                  : "最后一页"
          }
        />
      </Paper>
      {isLocal && (
        <Typography variant="caption" color="text.secondary">
          完整迁移报告包含所有本地迁移任务与失败记录，ZIP
          中包含不可达节点信息；报告不受当前筛选条件影响。
        </Typography>
      )}
      <Dialog
        open={confirmClear}
        onClose={() => !busy && setConfirmClear(false)}
        fullWidth
        maxWidth="sm"
      >
        <DialogTitle>
          清除全部{mode === "fallback" ? "降级" : "失败"}记录？
        </DialogTitle>
        <DialogContent>
          <Alert severity="warning" sx={{ mb: 2 }}>
            清除所有执行节点的记录，不受当前筛选条件限制。
          </Alert>
          <Typography>
            记录清除后无法恢复。
            {mode === "failed" && "清除范围也包括本地迁移产生的失败对象。"}
            仅删除异常记录，不会重试迁移任务。请先导出需保留的记录。
          </Typography>
        </DialogContent>
        <DialogActions>
          <Button disabled={busy} onClick={() => setConfirmClear(false)}>
            取消
          </Button>
          <Button
            variant="contained"
            color="error"
            disabled={busy}
            onClick={() => void clearRecords()}
            startIcon={
              busy ? <CircularProgress size={16} color="inherit" /> : undefined
            }
          >
            确认清除全部
          </Button>
        </DialogActions>
      </Dialog>
      <Dialog
        open={selected !== null}
        onClose={() => setSelected(null)}
        fullWidth
        maxWidth="md"
      >
        <DialogTitle>异常详情</DialogTitle>
        <DialogContent>
          <Stack spacing={2}>
            {selected &&
              Object.entries(selected)
                .filter(
                  ([, value]) =>
                    value !== null && value !== undefined && value !== "",
                )
                .map(([key, value]) => (
                  <Box key={key}>
                    <Typography variant="caption" color="text.secondary">
                      {(
                        {
                          taskIndex:
                            selected.parentTaskId != null
                              ? "子任务 ID"
                              : "任务 ID",
                          parentTaskId: "任务 ID",
                          workerNode: "执行节点",
                          executionNodeIp: "执行节点",
                          sourceBucket: "源 Bucket",
                          destBucket: "目标 Bucket",
                          key: "源对象 Key",
                          destinationKey: "目标对象 Key",
                          localPath: "本地路径",
                          errorMessage: "错误信息",
                          errorCode: "错误代码",
                          objectSize: "对象大小（字节）",
                          stage: "失败阶段",
                          retryCount: "重试次数",
                          timestamp: "时间戳",
                          timestampFormatted: "记录时间",
                          tmpFilePath: "临时文件路径",
                          storageId: "存储 ID",
                          direction: "迁移方向",
                        } as Record<string, string>
                      )[key] || key}
                    </Typography>
                    <Typography
                      variant="body2"
                      component="pre"
                      sx={{
                        m: 0,
                        whiteSpace: "pre-wrap",
                        overflowWrap: "anywhere",
                        fontFamily: "monospace",
                      }}
                    >
                      {typeof value === "object"
                        ? JSON.stringify(value, null, 2)
                        : String(value)}
                    </Typography>
                    <Divider sx={{ mt: 1.5 }} />
                  </Box>
                ))}
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setSelected(null)}>关闭</Button>
        </DialogActions>
      </Dialog>
    </Stack>
  );
}
