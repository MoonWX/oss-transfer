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
  retryResultLabel,
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
  Autocomplete,
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
  PlayArrowRounded,
  RefreshRounded,
  ReplayRounded,
} from "@mui/icons-material";
import { api, ApiError, download, getToken, useNotify } from "../lib/api";
import FailedTaskRetryDialog from "../components/FailedTaskRetryDialog";
import ListPagination, {
  rememberPageSize,
  savedPageSize,
} from "../components/ListPagination";
import {
  canRetrySnapshot,
  useTaskRecovery,
  useVisibleRecoveryTasks,
} from "../lib/taskRecovery";
import {
  EmptyState,
  PageHeader,
  StatusChip,
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
  retryStatus?: string;
  retryUpdatedAt?: number;
  retryError?: string;
  retryGeneration?: number;
  retryExecutionNodeIp?: string;
  retryExecutionNodeDisplay?: NodeDisplay;
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
  clearableCount?: number;
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
type ExceptionTask = {
  taskIndex: number;
  sourceBucket?: string;
  destBucket?: string;
  sourceEndpoint?: { localPath?: string };
  destinationEndpoint?: { localPath?: string };
};
type ClearScope = { mode: Mode; filters: Filters; count: number };

const exceptionFilters = (filters: Filters) => ({
  ...(filters.task.trim() ? { taskIndex: Number(filters.task) } : {}),
  ...(filters.node ? { executionNodeIp: filters.node } : {}),
  ...(filters.source.trim() ? { sourceBucket: filters.source.trim() } : {}),
  ...(filters.destination.trim()
    ? { destBucket: filters.destination.trim() }
    : {}),
  ...(filters.direction ? { direction: filters.direction } : {}),
  ...(filters.endpoint.trim() ? { endpoint: filters.endpoint.trim() } : {}),
});

type QueryState = {
  key: string;
  result: Result | null;
  error: string | null;
  loading: boolean;
};

const normalizeTaskFilter = (value: string) => {
  const task = value.trim().replace(/^#/, "");
  if (!task) return value.trim() ? null : "";
  return /^\d+$/.test(task) &&
    Number.isSafeInteger(Number(task)) &&
    Number(task) > 0
    ? String(Number(task))
    : null;
};

const errorText = (error: unknown) =>
  error instanceof Error ? error.message : String(error);
const sameFailure = (a: Failure, b: Failure) =>
  (a.parentTaskId ?? a.taskIndex) === (b.parentTaskId ?? b.taskIndex) &&
  a.childTaskId === b.childTaskId &&
  a.key === b.key;
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
    task: normalizeTaskFilter(params.get("taskId") || "") ?? "",
  };
  const nodes = useWorkerStatus();
  const nodeLabel = useNodeLabels();
  const [tab, setTab] = useState(params.get("type") === "local" ? 2 : 0);
  const [draft, setDraft] = useState<Filters>(linkedFilters);
  const [filters, setFilters] = useState<Filters>(linkedFilters);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(() =>
    savedPageSize("oss-transfer-exceptions-page-size", 25),
  );
  const [queries, setQueries] = useState<Partial<Record<Mode, QueryState>>>({});
  const [busy, setBusy] = useState(false);
  const retryInFlight = useRef(false);
  const recovery = useTaskRecovery();
  const [retryAcceptedTaskId, setRetryAcceptedTaskId] = useState<number | null>(
    null,
  );
  const [resumeAccepted, setResumeAccepted] = useState(false);
  const [retryError, setRetryError] = useState<string | null>(null);
  const [clearScope, setClearScope] = useState<ClearScope | null>(null);
  const clearInFlight = useRef(false);
  const [clearError, setClearError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Failure | null>(null);
  const [bulkOpen, setBulkOpen] = useState(false);
  const [catalogOpen, setCatalogOpen] = useState(false);
  const [catalogRevision, setCatalogRevision] = useState(0);
  const [catalog, setCatalog] = useState<{
    key: string;
    items: ExceptionTask[];
  } | null>(null);
  const [catalogLoading, setCatalogLoading] = useState(false);
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
      task: normalizeTaskFilter(params.get("taskId") || "") ?? "",
    };
    setTab(params.get("type") === "local" ? 2 : 0);
    setDraft(nextFilters);
    setFilters(nextFilters);
    setPage(0);
    setSelected(null);
  }, [params]);
  const mode = modes[tab];
  const catalogKey = JSON.stringify([mode, draft.task.trim(), catalogRevision]);
  const taskOptions = catalog?.key === catalogKey ? catalog.items : [];
  const taskSearch = draft.task.trim().replace(/^#/, "");
  const validTaskSearch =
    !draft.task.trim() ||
    (taskSearch !== "" && normalizeTaskFilter(taskSearch) !== null);
  useEffect(() => {
    if (!catalogOpen || !validTaskSearch) {
      setCatalogLoading(false);
      return;
    }
    const controller = new AbortController();
    const token = getToken();
    setCatalogLoading(true);
    const timer = window.setTimeout(async () => {
      try {
        const params = new URLSearchParams({
          type: mode,
          search: taskSearch,
          limit: "100",
        });
        const response = await api<{
          data: { items: ExceptionTask[]; hasMore: boolean };
        }>(`/api/master/exceptions/tasks?${params}`, {
          signal: controller.signal,
          timeoutMs: 10000,
        });
        if (!controller.signal.aborted && token === getToken())
          setCatalog({ key: catalogKey, items: response.data.items });
      } catch (cause) {
        if (!controller.signal.aborted && token === getToken()) {
          setCatalog({ key: catalogKey, items: [] });
          notify(errorText(cause), "warning");
        }
      } finally {
        if (!controller.signal.aborted) setCatalogLoading(false);
      }
    }, 200);
    return () => {
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [catalogOpen, catalogKey, mode, taskSearch, validTaskSearch, notify]);
  useEffect(() => setClearError(null), [mode]);
  const isLocal = mode === "local";
  const localPage = isLocal ? page : 0;
  const localSize = isLocal ? pageSize : 25;
  const queryKey = JSON.stringify([mode, filters, localPage, localSize]);
  const query = queries[mode]?.key === queryKey ? queries[mode] : undefined;
  // A new tab/filter is loading from its very first render. Cached rows belong
  // to one exact query and can remain visible while that query is refreshed.
  const result = query?.result ?? null;
  const clearableCount = Math.max(0, result?.clearableCount ?? 0);
  const loading = query?.loading ?? true;
  const error = query?.error ?? null;
  const recordNodeLabel: NodeLabels = (id, display) =>
    nodeLabel(id, mergeNodeDisplay(result?.nodeDisplays?.[id ?? ""], display));

  const load = useCallback(
    (force = false, quiet = false): Promise<void> => {
      const token = getToken();
      const pending = inFlight.current.get(queryKey);
      if (pending && !force) {
        latestQuery.current[mode] = { key: queryKey, id: pending.id };
        if (!quiet)
          setQueries((previous) => ({
            ...previous,
            [mode]: {
              key: queryKey,
              result:
                previous[mode]?.key === queryKey
                  ? previous[mode]!.result
                  : null,
              loading: true,
              error: null,
            },
          }));
        return pending.promise;
      }
      const id = ++loadId.current;
      latestQuery.current[mode] = { key: queryKey, id };
      if (!quiet)
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
                { timeoutMs: 10000 },
              )
            ).data;
          } else {
            const body = {
              ...exceptionFilters(filters),
              limit: Number(filters.limit) || 1000,
            };
            const raw = await api<Record<string, unknown>>(
              `/api/master/${mode}`,
              {
                method: "POST",
                body,
                timeoutMs: 10000,
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
              clearableCount:
                typeof raw.clearableCount === "number"
                  ? raw.clearableCount
                  : undefined,
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
          if (
            mounted.current &&
            getToken() === token &&
            latestQuery.current[mode]?.id === id
          )
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
          if (
            mounted.current &&
            getToken() === token &&
            latestQuery.current[mode]?.id === id
          )
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
  const actionScope = JSON.stringify([queryKey, page, pageSize]);
  const latestActionScope = useRef(actionScope);
  latestActionScope.current = actionScope;
  useEffect(() => {
    setRetryAcceptedTaskId(null);
    setResumeAccepted(false);
    setRetryError(null);
  }, [actionScope]);
  // Only infer a task when the complete result belongs to it. A page with
  // one parent does not mean the other pages belong to that parent as well.
  const resultTaskIds = [
    ...new Set(
      (result?.items ?? []).flatMap((item) => {
        const id = item.parentTaskId ?? item.taskIndex;
        return Number.isSafeInteger(id) && Number(id) > 0 ? [Number(id)] : [];
      }),
    ),
  ];
  const actionTaskId =
    mode === "fallback"
      ? undefined
      : Number(filters.task) > 0
        ? Number(filters.task)
        : result &&
            !result.partial &&
            result.total <= result.items.length &&
            resultTaskIds.length === 1
          ? resultTaskIds[0]
          : undefined;
  const applyTaskFilter = (value: string | null) => {
    const task = normalizeTaskFilter(value ?? "");
    if (task === null) {
      notify(tr("任务 ID 必须是正整数。"), "error");
      return;
    }
    setDraft((previous) => ({ ...previous, task }));
    setFilters((previous) => ({ ...previous, task }));
    setPage(0);
    setSelected(null);
  };
  const visibleTasks = useVisibleRecoveryTasks(
    !result || actionTaskId === undefined ? [] : [actionTaskId],
    queryKey,
    result,
  );
  const hasPendingResults = visible.some(
    (item) => item.retryStatus === "PENDING",
  );
  useEffect(() => {
    if (mode === "fallback" || !result) return;
    // Refresh object results independently of the parent status: the last ledger
    // update can arrive after the parent has already left RUNNING.
    const refresh = () => {
      if (document.visibilityState === "visible") void load(false, true);
    };
    const timer = window.setInterval(refresh, hasPendingResults ? 3000 : 10000);
    document.addEventListener("visibilitychange", refresh);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", refresh);
    };
  }, [mode, load, !!result, hasPendingResults]);
  const selectedRecord = selected
    ? (result?.items.find((item) => sameFailure(item, selected)) ?? selected)
    : null;
  const updateDraft = (field: keyof Filters, value: string) =>
    setDraft((previous) => ({ ...previous, [field]: value }));
  const applyFilters = (event: FormEvent) => {
    event.preventDefault();
    const task = normalizeTaskFilter(draft.task);
    if (task === null) {
      notify(tr("任务 ID 必须是正整数。"), "error");
      return;
    }
    setPage(0);
    setDraft((previous) => ({ ...previous, task }));
    setFilters({ ...draft, task });
  };
  async function clearRecords() {
    if (!clearScope || clearScope.mode === "fallback" || clearInFlight.current)
      return;
    clearInFlight.current = true;
    setBusy(true);
    setClearError(null);
    try {
      const value = await api("/api/master/failed/clear", {
        method: "POST",
        body: {
          type: clearScope.mode,
          ...exceptionFilters(clearScope.filters),
        },
      });
      if (
        value.partial ||
        value.failedWorkers?.length ||
        value.unreachableWorkers?.length
      ) {
        const message =
          value.message || tr("部分节点未能清除记录，请刷新后重试。");
        setClearError(message);
        notify(message, "warning");
      } else
        notify(
          tr("已清理 {count} 条已处理记录。", {
            count: formatNumber(value.clearedCount ?? 0),
          }),
          "success",
        );
      setClearScope(null);
      setPage(0);
      setCatalog(null);
      setCatalogRevision((value) => value + 1);
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
        setClearScope(null);
        setPage(0);
        setCatalog(null);
        setCatalogRevision((value) => value + 1);
        await load(true);
      }
    } finally {
      clearInFlight.current = false;
      setBusy(false);
    }
  }
  async function retry(taskId: number) {
    const displayed =
      taskId === undefined ? undefined : visibleTasks.tasks[taskId];
    if (
      taskId === undefined ||
      taskId !== actionTaskId ||
      !canRetrySnapshot(displayed ?? undefined) ||
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
    const scope = actionScope;
    const stillHere = () =>
      mounted.current && latestActionScope.current === scope;
    try {
      const current = await api<{
        data: { status: string; generation?: number };
      }>(`/api/master/tasks/${taskId}`, {
        timeoutMs: 10000,
      });
      if (!recovery.isCurrent()) return;
      if (!stillHere()) {
        recovery.release(taskId);
        return;
      }
      const generation = displayed!.generation!;
      if (
        current.data.generation !== generation ||
        !canRetrySnapshot(current.data)
      ) {
        visibleTasks.update(taskId, current.data);
        if (
          ["RUNNING", "RESUMING", "LIST_DONE", "COMPLETED"].includes(
            current.data.status,
          )
        )
          recovery.accept(taskId, current.data.generation);
        else recovery.release(taskId);
        notify(tr("任务状态已更新，请查看任务详情。"), "info");
        void load(true);
        return;
      }
      submitted = true;
      const value = await api<{
        accepted: number;
        ignored: number;
        failed: number;
        partial?: boolean;
        unreachableWorkers?: string[];
        details?: Array<{
          taskId?: number;
          generation?: number;
          status?: string;
          accepted?: boolean;
          ignored?: boolean;
          success?: boolean;
          message?: string;
          error?: string;
        }>;
      }>(
        current.data.status === "STOPPED"
          ? "/api/master/resume"
          : "/api/master/retry",
        {
          method: "POST",
          timeoutMs: 30000,
          body: {
            taskIds: [taskId],
            expectedGenerations: { [taskId]: generation },
          },
        },
      );
      if (!recovery.isCurrent()) return;
      const detail = value.details?.find((row) => row.taskId === taskId);
      if (value.accepted > 0 || value.ignored > 0) {
        recovery.accept(taskId, detail?.generation);
        visibleTasks.update(taskId, {
          status: detail?.status ?? "RESUMING",
          generation: detail?.generation,
        });
        if (value.accepted > 0 && stillHere()) {
          setRetryAcceptedTaskId(taskId);
          setResumeAccepted(current.data.status === "STOPPED");
        }
      } else recovery.release(taskId);
      if (!stillHere()) return;
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
        current.data.status === "STOPPED"
          ? "继续传输已受理 {value1}，忽略 {value2}，失败 {value3}{value4}"
          : "重试已受理 {value1}，忽略 {value2}，失败 {value3}{value4}",
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
      if (!stillHere()) return;
      const message = errorText(cause);
      setRetryError(message);
      notify(message, "error");
    } finally {
      retryInFlight.current = false;
      setBusy(false);
    }
  }
  const retryPhase = (id: number) => {
    const phase = recovery.states[id]?.phase;
    if (phase) return phase;
    const snapshot = visibleTasks.tasks[id];
    if (snapshot === undefined) return "loading";
    if (snapshot === null) return "unavailable";
    if (snapshot.status === "COMPLETED") return "completed";
    return canRetrySnapshot(snapshot) ? undefined : "accepted";
  };
  const actionSnapshot =
    actionTaskId === undefined ? undefined : visibleTasks.tasks[actionTaskId];
  const actionPhase =
    actionTaskId === undefined ? undefined : retryPhase(actionTaskId);
  const actionLabel =
    actionPhase === "submitting"
      ? tr("提交中…")
      : actionPhase === "completed"
        ? tr("任务已完成")
        : actionPhase === "loading"
          ? tr("检查状态…")
          : actionPhase === "unavailable"
            ? tr("状态暂不可用")
            : actionPhase
              ? tr("处理中…")
              : actionSnapshot?.status === "STOPPED"
                ? tr("继续传输")
                : actionSnapshot?.status === "PARTIAL_FAILED"
                  ? tr("重试失败项")
                  : tr("重试任务");
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
        description={tr("查看异常及重试结果，导出记录。")}
        action={
          <Stack direction="row" spacing={1}>
            {mode !== "fallback" && (
              <Button
                variant="contained"
                startIcon={<ReplayRounded />}
                onClick={() => setBulkOpen(true)}
                disabled={busy}
              >
                {tr("批量重试")}
              </Button>
            )}
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
          </Stack>
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
          {tr(
            resumeAccepted
              ? "任务 #{taskId} 的继续传输请求已受理。"
              : "任务 #{taskId} 的重试已受理。",
            { taskId: retryAcceptedTaskId },
          )}
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
            <Autocomplete
              freeSolo
              forcePopupIcon
              openOnFocus
              slotProps={{
                paper: {
                  elevation: 3,
                  sx: {
                    mt: 0.5,
                    border: 1,
                    borderColor: "divider",
                    borderRadius: "10px",
                  },
                },
                listbox: { sx: { maxHeight: 320 } },
              }}
              options={[
                "",
                ...taskOptions.map((task) => String(task.taskIndex)),
              ]}
              filterOptions={(options) => options}
              value={filters.task || null}
              inputValue={draft.task}
              loading={catalogLoading}
              loadingText={tr("正在读取任务")}
              onOpen={() => setCatalogOpen(true)}
              onClose={() => setCatalogOpen(false)}
              onInputChange={(_, value, reason) => {
                if (reason === "input") updateDraft("task", value);
              }}
              onChange={(_, value) => applyTaskFilter(value)}
              getOptionLabel={(value) => value}
              renderOption={(props, id) => {
                const { key, ...optionProps } = props;
                const task = taskOptions.find(
                  (task) => String(task.taskIndex) === id,
                );
                const source =
                  task?.sourceEndpoint?.localPath || task?.sourceBucket;
                const destination =
                  task?.destinationEndpoint?.localPath || task?.destBucket;
                const endpoints = `${source || "—"} → ${destination || "—"}`;
                return (
                  <li key={key} {...optionProps}>
                    <Box sx={{ minWidth: 0, width: "100%", py: 0.25 }}>
                      <Typography variant="body2">
                        {id ? tr("任务 #{id}", { id }) : tr("全部任务")}
                      </Typography>
                      {(source || destination) && (
                        <Typography
                          variant="caption"
                          color="text.secondary"
                          noWrap
                          title={endpoints}
                          sx={{ display: "block" }}
                        >
                          {endpoints}
                        </Typography>
                      )}
                    </Box>
                  </li>
                );
              }}
              renderInput={(params) => (
                <TextField
                  {...params}
                  label={tr("所属任务")}
                  placeholder={tr("搜索或输入任务 ID")}
                  size="small"
                />
              )}
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
            {isLocal && (
              <Button
                size="small"
                variant="outlined"
                startIcon={<DownloadRounded />}
                onClick={() => void report()}
                disabled={busy}
              >
                {tr("完整迁移报告")}
              </Button>
            )}
            {mode !== "fallback" && (
              <Button
                size="small"
                color="error"
                startIcon={<DeleteOutlineRounded />}
                onClick={() =>
                  setClearScope({
                    mode,
                    filters: { ...filters },
                    count: clearableCount,
                  })
                }
                disabled={busy || loading || clearableCount === 0}
              >
                {tr("清理已处理记录")}
              </Button>
            )}
          </Stack>
        </Stack>
        {actionTaskId !== undefined && (
          <Stack
            role="region"
            aria-label={tr("任务操作")}
            direction="row"
            alignItems="center"
            spacing={1.5}
            flexWrap="wrap"
            useFlexGap
            sx={{ px: 2.5, pb: 2.5 }}
          >
            <Typography variant="body2" fontWeight={600}>
              {tr("任务 #{id}", { id: actionTaskId })}
            </Typography>
            {actionSnapshot && <StatusChip status={actionSnapshot.status} />}
            <Button
              size="small"
              variant="contained"
              startIcon={
                actionPhase === "completed" ? (
                  <CheckCircleOutlineRounded />
                ) : actionPhase && actionPhase !== "unavailable" ? (
                  <CircularProgress size={16} color="inherit" />
                ) : actionSnapshot?.status === "STOPPED" ? (
                  <PlayArrowRounded />
                ) : (
                  <ReplayRounded />
                )
              }
              onClick={() => void retry(actionTaskId)}
              disabled={busy || !!actionPhase}
            >
              {actionLabel}
            </Button>
            <Button
              size="small"
              component={RouterLink}
              to={`/tasks?taskId=${actionTaskId}`}
            >
              {tr("查看任务")}
            </Button>
          </Stack>
        )}
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
              <Table
                size="small"
                sx={{
                  tableLayout: "fixed",
                  minWidth: mode === "fallback" ? 860 : 1060,
                }}
              >
                <TableHead>
                  <TableRow>
                    <TableCell sx={{ width: "26%" }}>
                      {tr("对象 / 路径")}
                    </TableCell>
                    <TableCell sx={{ width: "17%" }}>
                      {tr("任务与节点")}
                    </TableCell>
                    <TableCell>{tr("原因")}</TableCell>
                    {mode !== "fallback" && (
                      <TableCell sx={{ width: "20%" }}>
                        {tr("重试结果")}
                      </TableCell>
                    )}
                    <TableCell
                      align="right"
                      sx={{
                        width: 96,
                        position: "sticky",
                        right: 0,
                        zIndex: 1,
                        bgcolor: "background.paper",
                      }}
                    >
                      {tr("操作")}
                    </TableCell>
                  </TableRow>
                </TableHead>
                <TableBody>
                  {visible.map((item, index) => (
                    <TableRow
                      key={`${item.workerNode}-${item.taskIndex}-${item.key}-${index}`}
                      hover
                    >
                      <TableCell sx={{ minWidth: 220 }}>
                        <Typography
                          variant="body2"
                          title={item.key || item.localPath || "—"}
                          sx={{
                            overflowWrap: "anywhere",
                            fontFamily: "monospace",
                            display: "-webkit-box",
                            WebkitLineClamp: 2,
                            WebkitBoxOrient: "vertical",
                            overflow: "hidden",
                          }}
                        >
                          {item.key || item.localPath || "—"}
                        </Typography>
                        {item.destinationKey != null &&
                          item.destinationKey !== item.key && (
                            <Typography
                              variant="body2"
                              title={item.destinationKey}
                              sx={{
                                fontFamily: "monospace",
                                overflowWrap: "anywhere",
                                whiteSpace: "pre-wrap",
                                display: "-webkit-box",
                                WebkitLineClamp: 2,
                                WebkitBoxOrient: "vertical",
                                overflow: "hidden",
                              }}
                            >
                              {tr("目标：")}
                              {item.destinationKey}
                            </Typography>
                          )}
                        <Typography
                          variant="caption"
                          color="text.secondary"
                          title={`${item.sourceBucket || "—"} → ${item.destBucket || item.localPath || "—"}`}
                          sx={{
                            overflowWrap: "anywhere",
                            display: "-webkit-box",
                            WebkitLineClamp: 2,
                            WebkitBoxOrient: "vertical",
                            overflow: "hidden",
                          }}
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
                      <TableCell>
                        <Typography variant="body2">
                          #{item.parentTaskId ?? item.taskIndex ?? "—"}
                        </Typography>
                        <Typography
                          variant="caption"
                          color="text.secondary"
                          title={
                            item.workerNode || item.executionNodeIp
                              ? taskNodeLabels(item, recordNodeLabel)
                              : tr("调度阶段")
                          }
                          sx={{
                            fontFamily: "monospace",
                            overflowWrap: "anywhere",
                            display: "-webkit-box",
                            WebkitLineClamp: 2,
                            WebkitBoxOrient: "vertical",
                            overflow: "hidden",
                          }}
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
                          title={
                            item.errorMessage ||
                            item.errorCode ||
                            tr("未提供原因")
                          }
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
                        <Typography
                          variant="caption"
                          color="text.secondary"
                          display="block"
                          sx={{ mt: 0.5 }}
                        >
                          {failureTime(item)}
                        </Typography>
                      </TableCell>
                      {mode !== "fallback" && (
                        <TableCell sx={{ minWidth: 140, maxWidth: 280 }}>
                          <Chip
                            size="small"
                            variant="outlined"
                            color={
                              item.retryStatus === "INTERRUPTED"
                                ? "warning"
                                : item.retryStatus === "RECOVERED"
                                  ? "success"
                                  : item.retryStatus === "FAILED"
                                    ? "error"
                                    : item.retryStatus === "PENDING"
                                      ? "info"
                                      : "default"
                            }
                            label={tr(retryResultLabel(item.retryStatus))}
                          />
                          {!!item.retryCount && (
                            <Typography
                              variant="caption"
                              color="text.secondary"
                              display="block"
                              sx={{ mt: 0.5 }}
                            >
                              {tr("重试 {count} 次", {
                                count: formatNumber(item.retryCount),
                              })}
                            </Typography>
                          )}
                          {item.retryExecutionNodeIp &&
                            item.retryExecutionNodeIp !==
                              (item.executionNodeIp || item.workerNode) && (
                              <Typography
                                variant="caption"
                                color="text.secondary"
                                display="block"
                                sx={{ overflowWrap: "anywhere" }}
                              >
                                {tr("重试节点")}：
                                {recordNodeLabel(
                                  item.retryExecutionNodeIp,
                                  item.retryExecutionNodeDisplay,
                                )}
                              </Typography>
                            )}
                          {!!item.retryUpdatedAt && (
                            <Typography
                              variant="caption"
                              color="text.secondary"
                              display="block"
                            >
                              {new Date(item.retryUpdatedAt).toLocaleString(
                                getLocale(),
                              )}
                            </Typography>
                          )}
                          {item.retryStatus === "FAILED" && item.retryError && (
                            <Typography
                              variant="caption"
                              color="error.main"
                              display="block"
                              title={item.retryError}
                              sx={{
                                overflowWrap: "anywhere",
                                display: "-webkit-box",
                                WebkitLineClamp: 3,
                                WebkitBoxOrient: "vertical",
                                overflow: "hidden",
                              }}
                            >
                              {item.retryError}
                            </Typography>
                          )}
                        </TableCell>
                      )}
                      <TableCell
                        align="right"
                        sx={{
                          position: "sticky",
                          right: 0,
                          bgcolor: "background.paper",
                        }}
                      >
                        <Button size="small" onClick={() => setSelected(item)}>
                          {tr("详情")}
                        </Button>
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
        <Box sx={{ visibility: result ? "visible" : "hidden" }}>
          <ListPagination
            count={isLocal ? (result?.total ?? 0) : (result?.items.length ?? 0)}
            page={page}
            pageSize={pageSize}
            onPageChange={setPage}
            onPageSizeChange={(size) => {
              rememberPageSize("oss-transfer-exceptions-page-size", size);
              setPage(Math.floor((page * pageSize) / size));
              setPageSize(size);
            }}
          />
        </Box>
      </Paper>
      {isLocal && (
        <Typography variant="caption" color="text.secondary">
          {tr(
            "完整迁移报告包含所有本地迁移任务与失败记录，ZIP 中包含不可达节点信息；报告不受当前筛选条件影响。",
          )}
        </Typography>
      )}
      <FailedTaskRetryDialog
        open={bulkOpen}
        onClose={() => setBulkOpen(false)}
        onSubmitted={() => void load(true)}
      />
      <Dialog
        open={clearScope !== null}
        onClose={() => !busy && setClearScope(null)}
        fullWidth
        maxWidth="sm"
      >
        <DialogTitle>{tr("清理已处理记录？")}</DialogTitle>
        <DialogContent>
          <Typography>
            {tr(
              "清理当前筛选范围内 {count} 条已恢复或已跳过的记录。任务和未解决的失败记录会保留。",
              { count: formatNumber(clearScope?.count ?? 0) },
            )}
          </Typography>
          {clearScope?.filters.task && (
            <Typography sx={{ mt: 2 }}>
              {tr("任务 #{id}", { id: clearScope.filters.task })}
            </Typography>
          )}
        </DialogContent>
        <DialogActions>
          <Button disabled={busy} onClick={() => setClearScope(null)}>
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
            {tr("确认清理")}
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
            {selectedRecord &&
              exceptionDetails(selectedRecord, recordNodeLabel).map(
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
                      {key === "retryStatus"
                        ? tr(retryResultLabel(String(value)))
                        : typeof value === "boolean"
                          ? tr(value ? "是" : "否")
                          : date
                            ? new Date(String(value)).toLocaleString(
                                getLocale(),
                              )
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
