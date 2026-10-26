import { t as tr, useI18n, getLocale } from "../lib/i18n";
import { useNodeLabels, useWorkerStatus } from "../lib/workerStatus";
import {
  taskNodeLabels,
  mergeNodeDisplay,
  type NodeDisplay,
  type NodeLabels,
} from "../lib/nodeRegistry";
import {
  exceptionCsv,
  exceptionDetails,
  missingExceptionNodes,
} from "../lib/exceptionRecords";
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
} from "react";
import { Link as RouterLink, useSearchParams } from "react-router-dom";
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
  Skeleton,
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
  CheckCircleOutlineRounded,
  DeleteOutlineRounded,
  DownloadRounded,
  FilterAltRounded,
  RefreshRounded,
  ReplayRounded,
} from "@mui/icons-material";
import { api, ApiError, download, useApi, useNotify } from "../lib/api";
import { useTaskRecovery } from "../lib/taskRecovery";
import {
  EmptyState,
  PageHeader,
  formatBytes,
  formatNumber,
} from "../components/Common";

type Failure = {
  taskIndex?: number;
  parentTaskId?: number;
  childTaskId?: string;
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
  complete?: boolean;
  queriedWorkers?: string[];
  unreachableWorkers?: string[];
  failedWorkers?: string[];
  workerErrors?: Record<string, string>;
  respondingNodes?: string[];
  nodeDisplays?: Record<string, NodeDisplay>;
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
type Mode = (typeof modes)[number];
type QueryState = {
  key: string;
  result: Result | null;
  error: string | null;
  loading: boolean;
};

const errorText = (error: unknown) =>
  error instanceof Error ? error.message : String(error);
const failureTime = (item: Failure) =>
  item.timestamp
    ? new Date(item.timestamp).toLocaleString(getLocale())
    : item.timestampFormatted || "—";

function exportRows(rows: Failure[], name: string, resolve: NodeLabels) {
  const csv = exceptionCsv(rows, resolve);
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
  const labels = [tr("失败对象"), tr("降级迁移记录"), tr("本地迁移失败")];

  useI18n();
  const notify = useNotify();
  const [params] = useSearchParams();
  const linkedFilters = {
    ...initialFilters,
    task: /^\d+$/.test(params.get("taskId") || "") ? params.get("taskId")! : "",
  };
  const nodes = useWorkerStatus();
  const nodeLabel = useNodeLabels();
  const [tab, setTab] = useState(params.get("type") === "local" ? 2 : 0);
  const [draft, setDraft] = useState<Filters>(linkedFilters);
  const [filters, setFilters] = useState<Filters>(linkedFilters);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(25);
  const [queries, setQueries] = useState<Partial<Record<Mode, QueryState>>>({});
  const [busy, setBusy] = useState(false);
  const retryInFlight = useRef(false);
  const recovery = useTaskRecovery();
  const [retryAcceptedTaskId, setRetryAcceptedTaskId] = useState<number | null>(
    null,
  );
  const [retryError, setRetryError] = useState<string | null>(null);
  const [confirmClear, setConfirmClear] = useState(false);
  const [clearError, setClearError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Failure | null>(null);
  const loadId = useRef(0);
  const mounted = useRef(false);
  const inFlight = useRef(
    new Map<string, { id: number; promise: Promise<void> }>(),
  );
  const latestQuery = useRef<
    Partial<Record<Mode, { key: string; id: number }>>
  >({});
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
    setSelected(null);
  }, [params]);
  const mode = modes[tab];
  useEffect(() => setClearError(null), [mode]);
  const isLocal = mode === "local";
  const localPage = isLocal ? page : 0;
  const localSize = isLocal ? pageSize : 25;
  const queryKey = JSON.stringify([mode, filters, localPage, localSize]);
  const query = queries[mode]?.key === queryKey ? queries[mode] : undefined;
  // A new tab/filter is loading from its very first render. Cached rows belong
  // to one exact query and can remain visible while that query is refreshed.
  const result = query?.result ?? null;
  const loading = query?.loading ?? true;
  const error = query?.error ?? null;
  const recordNodeLabel: NodeLabels = (id, display) =>
    nodeLabel(id, mergeNodeDisplay(result?.nodeDisplays?.[id ?? ""], display));

  const load = useCallback(
    (force = false): Promise<void> => {
      const pending = inFlight.current.get(queryKey);
      if (pending && !force) {
        latestQuery.current[mode] = { key: queryKey, id: pending.id };
        setQueries((previous) => ({
          ...previous,
          [mode]: {
            key: queryKey,
            result:
              previous[mode]?.key === queryKey ? previous[mode]!.result : null,
            loading: true,
            error: null,
          },
        }));
        return pending.promise;
      }
      const id = ++loadId.current;
      latestQuery.current[mode] = { key: queryKey, id };
      setQueries((previous) => ({
        ...previous,
        [mode]: {
          key: queryKey,
          result:
            previous[mode]?.key === queryKey ? previous[mode]!.result : null,
          loading: true,
          error: null,
        },
      }));
      const request = (async () => {
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
            if (filters.node) body.executionNodeIp = filters.node;
            if (filters.source.trim())
              body.sourceBucket = filters.source.trim();
            if (filters.destination.trim())
              body.destBucket = filters.destination.trim();
            const raw = await api<Record<string, unknown>>(
              `/api/master/${mode}`,
              {
                method: "POST",
                body,
              },
            );
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
              queriedWorkers: Array.isArray(raw.queriedWorkers)
                ? (raw.queriedWorkers as string[])
                : undefined,
              complete:
                typeof raw.complete === "boolean" ? raw.complete : undefined,
              partial: raw.partial === true,
              unreachableWorkers: Array.isArray(raw.unreachableWorkers)
                ? (raw.unreachableWorkers as string[])
                : [],
              failedWorkers: Array.isArray(raw.failedWorkers)
                ? (raw.failedWorkers as string[])
                : [],
              workerErrors: raw.workerErrors as
                Record<string, string> | undefined,
              nodeDisplays: raw.nodeDisplays as
                Record<string, NodeDisplay> | undefined,
            };
          }
          if (mounted.current && latestQuery.current[mode]?.id === id)
            setQueries((previous) => ({
              ...previous,
              [mode]: {
                key: queryKey,
                result: next,
                error: null,
                loading: false,
              },
            }));
        } catch (cause) {
          if (mounted.current && latestQuery.current[mode]?.id === id)
            setQueries((previous) => ({
              ...previous,
              [mode]: {
                key: queryKey,
                result:
                  previous[mode]?.key === queryKey
                    ? previous[mode]!.result
                    : null,
                error: errorText(cause),
                loading: false,
              },
            }));
        }
      })();
      inFlight.current.set(queryKey, { id, promise: request });
      void request.finally(() => {
        if (inFlight.current.get(queryKey)?.promise === request)
          inFlight.current.delete(queryKey);
      });
      return request;
    },
    [mode, filters, localPage, localSize, queryKey],
  );
  useEffect(() => {
    mounted.current = true;
    void load();
    return () => {
      mounted.current = false;
    };
  }, [load]);
  useEffect(() => {
    if (!result) return;
    const count = isLocal ? result.total : result.items.length;
    const lastPage = Math.max(0, Math.ceil(count / pageSize) - 1);
    if (page > lastPage) setPage(lastPage);
  }, [result, isLocal, page, pageSize]);
  const hasMembershipSnapshot = Array.isArray(result?.queriedWorkers);
  const legacyNodes =
    !isLocal && !filters.task
      ? Object.keys(nodes.data?.workers ?? {}).filter(
          (node) => !filters.node || node === filters.node,
        )
      : [];
  const missingNodes = missingExceptionNodes(result, legacyNodes);
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
      notify(tr("任务 ID 必须是非负整数。"), "error");
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
        const message =
          value.message || tr("部分节点未能清除记录，请刷新后重试。");
        setClearError(message);
        notify(message, "warning");
      } else notify(tr("清除请求已提交，正在刷新各节点记录。"), "info");
      setConfirmClear(false);
      setPage(0);
      await load(true);
    } catch (cause) {
      const value = cause instanceof ApiError ? cause.response : undefined;
      const failedNodes = [
        ...new Set([
          ...(value?.failedWorkers ?? []),
          ...(value?.unreachableWorkers ?? []),
        ]),
      ];
      const message = `${errorText(cause)}${failedNodes.length ? tr("（未完成节点：{value1}）", { value1: failedNodes.map((id) => nodeLabel(id, value?.nodeDisplays?.[id])).join("、") }) : ""}`;
      setClearError(message);
      notify(message, value?.partial ? "warning" : "error");
      if (value?.partial) {
        setConfirmClear(false);
        setPage(0);
        await load(true);
      }
    } finally {
      setBusy(false);
    }
  }
  async function retry(item: Failure) {
    const taskId = item.parentTaskId ?? item.taskIndex;
    if (
      taskId === undefined ||
      busy ||
      retryInFlight.current ||
      !recovery.begin([taskId])
    )
      return;
    retryInFlight.current = true;
    setRetryAcceptedTaskId(null);
    setRetryError(null);
    setBusy(true);
    let submitted = false;
    try {
      const current = await api<{
        data: { status: string; generation?: number };
      }>(`/api/master/tasks/${taskId}`);
      if (!recovery.isCurrent()) return;
      const generation = current.data.generation;
      if (
        ["RUNNING", "RESUMING", "LIST_DONE", "COMPLETED"].includes(
          current.data.status,
        )
      ) {
        recovery.accept(taskId, generation);
        notify(tr("任务状态已更新，请查看任务详情。"), "info");
        void load(true);
        return;
      }
      submitted = true;
      const response = await api<{
        data: {
          accepted: number;
          ignored: number;
          failed: number;
          partial?: boolean;
          unreachableWorkers?: string[];
          details?: Array<{
            taskId?: number;
            generation?: number;
            accepted?: boolean;
            ignored?: boolean;
            success?: boolean;
            message?: string;
            error?: string;
          }>;
        };
      }>("/api/master/local-transfers/retry-failures", {
        method: "POST",
        body: {
          taskIds: [taskId],
          ...(Number.isSafeInteger(generation) && Number(generation) > 0
            ? { expectedGenerations: { [taskId]: generation } }
            : {}),
        },
      });
      const value = response.data;
      if (!recovery.isCurrent()) return;
      const detail = value.details?.find((row) => row.taskId === taskId);
      if (value.accepted > 0 || value.ignored > 0) {
        recovery.accept(taskId, detail?.generation);
        if (value.accepted > 0) setRetryAcceptedTaskId(taskId);
      } else recovery.release(taskId);
      const reasons = [
        ...new Set(
          (value.details ?? [])
            .filter((item) => item.success === false || item.error)
            .map((item) => item.message || item.error)
            .filter((message): message is string => !!message),
        ),
      ];
      const unavailable =
        value.unreachableWorkers?.map((id) => nodeLabel(id)) ?? [];
      const message = tr(
        "重试已受理 {value1}，忽略 {value2}，失败 {value3}{value4}",
        {
          value1: value.accepted,
          value2: value.ignored,
          value3: value.failed,
          value4: reasons.length
            ? `；${reasons.join("；")}`
            : unavailable.length
              ? tr("；不可达节点：{value1}", {
                  value1: unavailable.join("、"),
                })
              : "",
        },
      );
      const failed = value.partial || value.failed > 0;
      if (failed) setRetryError(message);
      notify(message, failed ? "warning" : "success");
      void load(true);
    } catch (cause) {
      if (!recovery.isCurrent()) return;
      if (
        submitted &&
        (!(cause instanceof ApiError) ||
          cause.status === 0 ||
          cause.status >= 500)
      )
        recovery.check(taskId);
      else recovery.release(taskId);
      const message = errorText(cause);
      setRetryError(message);
      notify(message, "error");
    } finally {
      retryInFlight.current = false;
      setBusy(false);
    }
  }
  const retryPhase = (item: Failure) =>
    recovery.states[item.parentTaskId ?? item.taskIndex ?? -1]?.phase;
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
        title={tr("异常记录")}
        description={tr("查看失败对象和降级记录，导出异常明细。")}
        action={
          <Button
            startIcon={<RefreshRounded />}
            onClick={() => {
              void load();
              void nodes.refresh();
            }}
            disabled={loading}
          >
            {tr("刷新")}
          </Button>
        }
      />
      {retryAcceptedTaskId !== null && (
        <Alert
          severity="info"
          onClose={() => setRetryAcceptedTaskId(null)}
          action={
            <Button
              component={RouterLink}
              to={`/tasks?taskId=${retryAcceptedTaskId}`}
              color="inherit"
              size="small"
            >
              {tr("查看任务")}
            </Button>
          }
        >
          {tr("任务 #{taskId} 的重试已受理。", { taskId: retryAcceptedTaskId })}
        </Alert>
      )}
      {retryError && (
        <Alert severity="warning" onClose={() => setRetryError(null)}>
          {retryError}
        </Alert>
      )}
      <Tabs
        value={tab}
        onChange={(_, value) => {
          setTab(value);
          setPage(0);
          setSelected(null);
          setClearError(null);
          setDraft(initialFilters);
          setFilters(initialFilters);
        }}
        variant="scrollable"
        scrollButtons="auto"
        aria-label={tr("异常记录类型")}
        sx={{ borderBottom: 1, borderColor: "divider" }}
      >
        {labels.map((label) => (
          <Tab key={label} label={label} disabled={busy} />
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
              label={tr("任务 ID")}
              size="small"
              type="number"
              value={draft.task}
              onChange={(event) => updateDraft("task", event.target.value)}
              slotProps={{ htmlInput: { min: 0, step: 1 } }}
            />
            <TextField
              select
              label={tr("执行节点")}
              size="small"
              value={draft.node}
              onChange={(event) => updateDraft("node", event.target.value)}
            >
              <MenuItem value="">{tr("全部节点")}</MenuItem>
              {[
                ...new Set([
                  ...Object.keys(nodes.data?.workers ?? {}),
                  ...Object.keys(result?.nodeDisplays ?? {}),
                  ...(result?.items ?? [])
                    .map((item) => item.workerNode || item.executionNodeIp)
                    .filter((id): id is string => !!id),
                  ...(draft.node ? [draft.node] : []),
                ]),
              ].map((ip) => (
                <MenuItem key={ip} value={ip}>
                  {nodeLabel(ip, result?.nodeDisplays?.[ip])}
                </MenuItem>
              ))}
            </TextField>
            {isLocal ? (
              <>
                <TextField
                  select
                  label={tr("迁移方向")}
                  size="small"
                  value={draft.direction}
                  onChange={(event) =>
                    updateDraft("direction", event.target.value)
                  }
                >
                  <MenuItem value="">{tr("全部方向")}</MenuItem>
                  <MenuItem value="OSS_TO_LOCAL">{tr("OSS → 本地")}</MenuItem>
                  <MenuItem value="LOCAL_TO_OSS">{tr("本地 → OSS")}</MenuItem>
                </TextField>
                <TextField
                  label={tr("端点 / 路径")}
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
                  label={tr("源 Bucket")}
                  size="small"
                  value={draft.source}
                  onChange={(event) =>
                    updateDraft("source", event.target.value)
                  }
                />
                <TextField
                  label={tr("目标 Bucket")}
                  size="small"
                  value={draft.destination}
                  onChange={(event) =>
                    updateDraft("destination", event.target.value)
                  }
                />
                <TextField
                  select
                  label={tr("每节点读取上限")}
                  size="small"
                  value={draft.limit}
                  onChange={(event) => updateDraft("limit", event.target.value)}
                >
                  {["100", "1000", "5000", "10000"].map((value) => (
                    <MenuItem key={value} value={value}>
                      {tr("{count} 条", { count: formatNumber(Number(value)) })}
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
              {tr("应用筛选")}
            </Button>
            <Button
              onClick={() => {
                setDraft(initialFilters);
                setFilters({ ...initialFilters });
                setPage(0);
              }}
              disabled={loading}
            >
              {tr("重置")}
            </Button>
          </Stack>
        </Stack>
      </Paper>
      {error && <Alert severity="error">{error}</Alert>}
      {nodes.error && !hasMembershipSnapshot && (
        <Alert severity="warning">
          {tr("节点清单读取失败，无法确认结果是否覆盖全部节点。")}
        </Alert>
      )}
      {clearError && <Alert severity="warning">{clearError}</Alert>}
      {(result?.partial ||
        result?.complete === false ||
        missingNodes.length > 0) && (
        <Alert severity="warning">
          {missingNodes.length > 0
            ? tr("部分节点未返回数据，当前结果不完整。不可达节点：{nodes}", {
                nodes: missingNodes
                  .map((id) => nodeLabel(id, result?.nodeDisplays?.[id]))
                  .join("、"),
              })
            : tr("部分节点未返回数据，当前结果不完整。")}
          {Object.entries(result?.workerErrors ?? {}).map(([node, message]) => (
            <Typography key={node} variant="body2">
              {nodeLabel(node, result?.nodeDisplays?.[node])}：{message}
            </Typography>
          ))}
        </Alert>
      )}
      {!isLocal && result && result.total > result.items.length && (
        <Alert severity="info">
          {tr(
            "共查询到 {total} 条记录，当前加载 {loaded} 条。可提高每节点读取上限或缩小筛选范围。",
            {
              total: formatNumber(result.total),
              loaded: formatNumber(result.items.length),
            },
          )}
        </Alert>
      )}
      {mode === "fallback" && (
        <Alert severity="info">
          {tr(
            "降级记录表示对象曾切换到备用传输方式。请查看对应任务状态确认结果。",
          )}
        </Alert>
      )}
      <Paper
        variant="outlined"
        role="region"
        aria-label={labels[tab]}
        aria-busy={loading}
        sx={{ overflow: "hidden" }}
      >
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
                ? tr("{value1} 条记录{value2}", {
                    value1: formatNumber(result.total),
                    value2: !isLocal
                      ? tr(" · 已加载 {value1} 条", {
                          value1: formatNumber(result.items.length),
                        })
                      : "",
                  })
                : error
                  ? tr("读取失败")
                  : tr("正在读取记录")}
            </Typography>
          </Box>
          <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
            <Button
              size="small"
              startIcon={<DownloadRounded />}
              disabled={!result?.items.length || loading}
              onClick={() =>
                exportRows(
                  result?.items ?? [],
                  `${mode}-records.csv`,
                  recordNodeLabel,
                )
              }
            >
              {isLocal ? tr("导出当前页") : tr("导出已加载记录")}
            </Button>
            {isLocal ? (
              <Button
                size="small"
                variant="outlined"
                startIcon={<DownloadRounded />}
                onClick={() => void report()}
                disabled={busy}
              >
                {tr("完整迁移报告")}
              </Button>
            ) : (
              <Button
                size="small"
                color="error"
                startIcon={<DeleteOutlineRounded />}
                onClick={() => setConfirmClear(true)}
                disabled={busy || loading}
              >
                {tr("清除全部记录")}
              </Button>
            )}
          </Stack>
        </Stack>
        <Box sx={{ height: 4 }}>
          {loading && <LinearProgress aria-label={tr("正在读取记录")} />}
        </Box>
        <Box sx={{ minHeight: 240 }}>
          {loading && !result ? (
            <Stack spacing={2} sx={{ p: 2.5 }} aria-hidden="true">
              {[0, 1, 2].map((row) => (
                <Skeleton
                  key={row}
                  variant="rounded"
                  height={48}
                  animation={false}
                />
              ))}
            </Stack>
          ) : visible.length > 0 ? (
            <TableContainer>
              <Table size="small" sx={{ minWidth: 780 }}>
                <TableHead>
                  <TableRow>
                    <TableCell>{tr("对象 / 路径")}</TableCell>
                    <TableCell>{tr("任务与节点")}</TableCell>
                    <TableCell>{tr("原因")}</TableCell>
                    <TableCell>{tr("记录时间")}</TableCell>
                    <TableCell align="right">{tr("操作")}</TableCell>
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
                              {tr("目标：")}
                              {item.destinationKey}
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
                          {item.workerNode || item.executionNodeIp
                            ? taskNodeLabels(item, recordNodeLabel)
                            : tr("调度阶段")}
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
                          {item.errorMessage ||
                            item.errorCode ||
                            tr("未提供原因")}
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
                          {tr("详情")}
                        </Button>
                        {isLocal &&
                          (item.parentTaskId !== undefined ||
                            item.taskIndex !== undefined) && (
                            <Button
                              size="small"
                              startIcon={
                                retryPhase(item) === "completed" ? (
                                  <CheckCircleOutlineRounded />
                                ) : retryPhase(item) ? (
                                  <CircularProgress size={16} color="inherit" />
                                ) : (
                                  <ReplayRounded />
                                )
                              }
                              onClick={() => void retry(item)}
                              disabled={busy || !!retryPhase(item)}
                            >
                              {retryPhase(item) === "submitting"
                                ? tr("提交中…")
                                : retryPhase(item) === "completed"
                                  ? tr("已完成")
                                  : retryPhase(item)
                                    ? tr("处理中…")
                                    : tr("重试任务")}
                            </Button>
                          )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableContainer>
          ) : (
            result &&
            !error && (
              <EmptyState
                title={
                  result && result.total > 0
                    ? tr("当前页无记录")
                    : tr("暂无匹配记录")
                }
                description={
                  result && result.total > 0
                    ? tr(
                        "每个节点最多返回 1,000 条本地失败记录。请按任务或节点缩小筛选范围。",
                      )
                    : tr("请调整筛选条件，或在任务执行后刷新。")
                }
              />
            )
          )}
        </Box>
        <TablePagination
          component="div"
          sx={{ visibility: result ? "visible" : "hidden" }}
          count={isLocal ? (result?.total ?? 0) : (result?.items.length ?? 0)}
          page={page}
          onPageChange={(_, value) => setPage(value)}
          rowsPerPage={pageSize}
          onRowsPerPageChange={(event) => {
            setPageSize(Number(event.target.value));
            setPage(0);
          }}
          rowsPerPageOptions={[25, 50, 100]}
          labelRowsPerPage={tr("每页")}
          labelDisplayedRows={({ from, to, count }) =>
            `${from}–${to} / ${count}`
          }
          getItemAriaLabel={(type) =>
            type === "next"
              ? tr("下一页")
              : type === "previous"
                ? tr("上一页")
                : type === "first"
                  ? tr("第一页")
                  : tr("最后一页")
          }
        />
      </Paper>
      {isLocal && (
        <Typography variant="caption" color="text.secondary">
          {tr(
            "完整迁移报告包含所有本地迁移任务与失败记录，ZIP 中包含不可达节点信息；报告不受当前筛选条件影响。",
          )}
        </Typography>
      )}
      <Dialog
        open={confirmClear}
        onClose={() => !busy && setConfirmClear(false)}
        fullWidth
        maxWidth="sm"
      >
        <DialogTitle>
          {mode === "fallback"
            ? tr("清除全部降级记录？")
            : tr("清除全部失败记录？")}
        </DialogTitle>
        <DialogContent>
          <Alert severity="warning" sx={{ mb: 2 }}>
            {tr("清除所有执行节点的记录，不受当前筛选条件限制。")}
          </Alert>
          <Typography>
            {mode === "failed"
              ? tr(
                  "记录清除后无法恢复。清除范围也包括本地迁移产生的失败对象。仅删除异常记录，不会重试迁移任务。请先导出需保留的记录。",
                )
              : tr(
                  "记录清除后无法恢复。仅删除异常记录，不会重试迁移任务。请先导出需保留的记录。",
                )}
          </Typography>
        </DialogContent>
        <DialogActions>
          <Button disabled={busy} onClick={() => setConfirmClear(false)}>
            {tr("取消")}
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
            {tr("确认清除全部")}
          </Button>
        </DialogActions>
      </Dialog>
      <Dialog
        open={selected !== null}
        onClose={() => setSelected(null)}
        fullWidth
        maxWidth="md"
      >
        <DialogTitle>{tr("异常详情")}</DialogTitle>
        <DialogContent>
          <Stack spacing={2}>
            {selected &&
              exceptionDetails(selected, recordNodeLabel).map(
                ({ key, label, value, date }) => (
                  <Box key={key}>
                    <Typography variant="caption" color="text.secondary">
                      {tr(label)}
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
                      {typeof value === "boolean"
                        ? tr(value ? "是" : "否")
                        : date
                          ? new Date(String(value)).toLocaleString(getLocale())
                          : String(value)}
                    </Typography>
                    <Divider sx={{ mt: 1.5 }} />
                  </Box>
                ),
              )}
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setSelected(null)}>{tr("关闭")}</Button>
        </DialogActions>
      </Dialog>
    </Stack>
  );
}
