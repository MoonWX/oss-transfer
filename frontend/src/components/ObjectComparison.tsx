import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type FormEvent,
  type RefObject,
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
  DialogContentText,
  DialogTitle,
  Divider,
  LinearProgress,
  Link,
  List,
  ListItemButton,
  MenuItem,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  TextField,
  Typography,
} from "@mui/material";
import {
  ChevronLeftRounded,
  ChevronRightRounded,
  DeleteOutlineRounded,
  FileDownloadOutlined,
  PlayArrowRounded,
  RefreshRounded,
  StopCircleOutlined,
} from "@mui/icons-material";
import { api, ApiError, useNotify } from "../lib/api";
import RuntimeMessageLine from "./RuntimeMessageLine";
import { ticketDownload } from "../lib/download";
import {
  formatDate,
  formatNumber as formatDecimal,
  getLocale,
  t,
  useI18n,
} from "../lib/i18n";
import { formatBytes, formatNumber } from "./Common";
import ListPagination, {
  listPageSizes,
  rememberPageSize,
  savedPageSize,
} from "./ListPagination";
import SelectField from "./SelectField";

type Endpoint = { kind?: string; ref?: string; bucket?: string };
type Row = {
  number: number;
  key: string;
  sourceKey: string;
  destinationKey: string;
  status: string;
  sourceSize: number;
  destinationSize: number;
  sourceSha256?: string;
  destinationSha256?: string;
  error?: string;
  reasons?: string[];
  errorDetail?: string;
};
type Progress = {
  id: string;
  status: string;
  mode: string;
  scope: string;
  source?: Endpoint;
  destination?: Endpoint;
  sourcePrefix?: string;
  destinationPrefix?: string;
  compared?: number;
  matched?: number;
  different?: number;
  etagMismatch?: number;
  missing?: number;
  extra?: number;
  changed?: number;
  errors?: number;
  bothMissing?: number;
  rows?: number;
  rowsTruncated?: boolean;
  resultLimit?: number;
  truncatedReason?: string;
  lastStoredKey?: string;
  bytesRead?: number;
  elapsedMillis?: number;
  // How often the comparison continued from its checkpoint, e.g. after the
  // node restarted.
  resumes?: number;
  resumable?: boolean;
  startedAt?: string;
  endedAt?: string;
  error?: string;
  errorDetail?: string;
};
type Page = Progress & { items?: Row[]; nextAfter?: number };
type HistoryItem = Progress & { nodeId?: string; nodeName?: string };
type History = {
  items?: HistoryItem[];
  complete?: boolean;
  unavailableNodes?: { nodeId: string; nodeName?: string; count?: number }[];
};
type RowsView = {
  key: string;
  after: number;
  items: Row[];
  nextAfter: number;
};

const ROWS_PER_PAGE = 100;
const PAGE_POLL_MS = 1500;
const HISTORY_POLL_MS = 5000;
// While a node is unreachable the history checks back at this slower pace.
const HISTORY_SLOW_POLL_MS = 15000;
const RETRY_MS = 5000;
// Retries for an unreachable node double up to this delay.
const MAX_RETRY_MS = 60000;
// A filtered page may come back empty after the Master's scan budget while
// stored rows remain. Continue this many times before asking the operator.
const AUTO_CONTINUE = 5;
const PAGE_SIZE_PREFERENCE = "oss-transfer-comparison-page-size";

const isActive = (status?: string) =>
  status === "RUNNING" || status === "CANCELING";
const isAbort = (error: unknown) =>
  error instanceof DOMException && error.name === "AbortError";
const errorMessage = (error: unknown) =>
  error instanceof Error ? error.message : String(error);
const comparisonPath = (id: string) =>
  `/api/master/comparisons/${encodeURIComponent(id)}`;

/**
 * The width of an element, measured before paint and kept current. The
 * sidebar is resizable, so the content width, not the viewport, decides
 * whether a table fits. Only the fitting layout is rendered.
 */
function useElementWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  const [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const element = ref.current;
    if (!element) return;
    const measure = () => setWidth(element.clientWidth);
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  return [ref, width] as const;
}

/**
 * Shows a table only where it fits without scrolling sideways; narrower
 * containers get stacked items. The table's real need depends on the
 * language, so an overflowing table records its width and steps aside.
 */
function useFittingTable(minimum: number) {
  const { locale } = useI18n();
  const [box, width] = useElementWidth<HTMLDivElement>();
  const table = useRef<HTMLDivElement>(null);
  const [needed, setNeeded] = useState({ locale, width: minimum });
  const fits = width >= (needed.locale === locale ? needed.width : minimum);
  useLayoutEffect(() => {
    const element = table.current;
    if (fits && element && element.scrollWidth > element.clientWidth + 1)
      setNeeded({ locale, width: element.scrollWidth });
  });
  return { box, table, fits };
}

// Below these widths each comparison or difference is a stacked item. The
// difference table keeps at least 140 px for each key column.
const HISTORY_TABLE_WIDTH = 840;
const ROWS_TABLE_WIDTH = 688;
const CONTENT_ROWS_TABLE_WIDTH = 816;

function stateLabel(status?: string) {
  switch (status) {
    case "RUNNING":
      return t("正在比对");
    case "CANCELING":
      return t("正在取消");
    case "CANCELED":
      return t("已取消");
    case "INTERRUPTED":
      return t("比对已中断");
    case "FAILED":
      return t("比对失败");
    case "COMPLETED":
      return t("比对完成");
    case "UNAVAILABLE":
      return t("节点不可用");
    default:
      return status || t("未知");
  }
}
const stateColors: Record<
  string,
  "default" | "info" | "success" | "warning" | "error"
> = {
  RUNNING: "info",
  CANCELING: "warning",
  INTERRUPTED: "warning",
  FAILED: "error",
  COMPLETED: "success",
};
function StateChip({ status }: { status?: string }) {
  return (
    <Chip
      label={stateLabel(status)}
      size="small"
      color={stateColors[status || ""] || "default"}
      variant="outlined"
      sx={{ fontWeight: 500, fontSize: 12, height: 26 }}
    />
  );
}

function rowStatusLabel(status: string) {
  switch (status) {
    case "MATCH":
      return t("一致");
    case "DIFFERENT":
      return t("不一致");
    case "ETAG_MISMATCH":
      return t("ETag 不同");
    case "MISSING_TARGET":
      return t("目标缺失");
    case "EXTRA_TARGET":
      return t("目标多余");
    case "CHANGED_DURING_COMPARE":
      return t("比对期间已变化");
    case "ERROR":
      return t("读取失败");
    case "BOTH_MISSING":
      return t("两端均不存在");
    default:
      return status;
  }
}

function reasonLabel(reason: string) {
  switch (reason) {
    case "size":
      return t("大小");
    case "contentType":
      return "Content-Type";
    case "contentEncoding":
      return "Content-Encoding";
    case "contentDisposition":
      return "Content-Disposition";
    case "cacheControl":
      return "Cache-Control";
    case "metadata":
      return t("自定义元数据");
    case "sha256":
      return "SHA-256";
    case "etag":
      return "ETag";
    default:
      return reason;
  }
}
function reasonsText(reasons?: string[]) {
  if (!reasons?.length) return "";
  const separator = getLocale() === "en-US" ? ", " : "、";
  return t("不同：{reasons}", {
    reasons: reasons.map(reasonLabel).join(separator),
  });
}

/** Status filters in display order; "" lists every stored difference. */
const filters: {
  status: string;
  label: () => string;
  count: (progress: Progress) => number | undefined;
}[] = [
  { status: "", label: () => t("全部差异"), count: (p) => p.rows },
  { status: "DIFFERENT", label: () => t("不一致"), count: (p) => p.different },
  {
    status: "ETAG_MISMATCH",
    label: () => t("ETag 不同"),
    count: (p) => p.etagMismatch,
  },
  {
    status: "MISSING_TARGET",
    label: () => t("目标缺失"),
    count: (p) => p.missing,
  },
  {
    status: "EXTRA_TARGET",
    label: () => t("目标多余"),
    count: (p) => p.extra,
  },
  {
    status: "CHANGED_DURING_COMPARE",
    label: () => t("比对期间已变化"),
    count: (p) => p.changed,
  },
  { status: "ERROR", label: () => t("读取失败"), count: (p) => p.errors },
  {
    status: "BOTH_MISSING",
    label: () => t("两端均不存在"),
    count: (p) => p.bothMissing,
  },
];

const location = (endpoint?: Endpoint, prefix?: string) =>
  `${[endpoint?.ref, endpoint?.bucket].filter(Boolean).join("/")}/${prefix || ""}`;
const routeText = (progress: Progress) =>
  `${location(progress.source, progress.sourcePrefix)} → ${location(
    progress.destination,
    progress.destinationPrefix,
  )}`;
const modeLabel = (mode?: string) =>
  mode === "content" ? "SHA-256" : t("元数据");
const differences = (progress: Progress) =>
  Math.max(0, (progress.compared ?? 0) - (progress.matched ?? 0));

/** Go writes an unset time as year 1; show only real timestamps. */
function timeText(value?: string) {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime()) || date.getUTCFullYear() < 1971) return "—";
  return formatDate(date);
}

function formatElapsed(milliseconds: number) {
  const total = Math.max(0, Math.floor(milliseconds / 1000));
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (days) return t("{days} 天 {hours} 小时", { days, hours });
  if (hours) return t("{hours} 小时 {minutes} 分", { hours, minutes });
  if (minutes) return t("{minutes} 分 {seconds} 秒", { minutes, seconds });
  return t("{seconds} 秒", { seconds });
}

function statsText(progress: Progress) {
  const count = formatNumber(progress.compared);
  const elapsedMillis = progress.elapsedMillis ?? 0;
  // Agents before results protocol 2 do not report the running time.
  if (elapsedMillis <= 0) return `${t("已检查")} ${count}`;
  const seconds = elapsedMillis / 1000;
  const perSecond = (progress.compared ?? 0) / seconds;
  const values = {
    count,
    // Content mode can take longer than a second per object.
    rate: formatDecimal(
      perSecond,
      perSecond > 0 && perSecond < 1
        ? { maximumSignificantDigits: 2 }
        : { maximumFractionDigits: perSecond < 10 ? 1 : 0 },
    ),
    elapsed: formatElapsed(elapsedMillis),
  };
  if (progress.mode !== "content")
    return t("已检查 {count}，{rate} 个/秒，已用 {elapsed}", values);
  // Content mode reads whole objects: the read rate shows progress while a
  // large object is still being checked, and is the only rate of one object.
  const read = {
    ...values,
    bytes: formatBytes(progress.bytesRead),
    byteRate: formatBytes((progress.bytesRead ?? 0) / seconds),
  };
  return progress.scope === "object"
    ? t("已用 {elapsed}，已读取 {bytes}（{byteRate}/秒）", read)
    : t(
        "已检查 {count}，{rate} 个/秒，已用 {elapsed}，已读取 {bytes}（{byteRate}/秒）",
        read,
      );
}

function truncationText(progress: Progress) {
  const key = progress.lastStoredKey || "—";
  switch (progress.truncatedReason || "LEGACY_LIMIT") {
    case "LEGACY_LIMIT":
      return t(
        "该节点版本最多保存前 {count} 条差异，其余差异只计数。升级该节点后重新比对可保存全部差异。",
        { count: formatNumber(progress.resultLimit || 10000) },
      );
    case "STORAGE_BUDGET":
      return t(
        "差异明细超过节点的比对结果存储预算，之后的差异只计数未保存。最后保存的 Key：{key}",
        { key },
      );
    case "LOW_DISK_SPACE":
      return t(
        "节点磁盘剩余空间不足，之后的差异只计数未保存。最后保存的 Key：{key}",
        { key },
      );
    default:
      return t("部分差异明细未保存，之后的差异只计数。最后保存的 Key：{key}", {
        key,
      });
  }
}

const sourceSize = (row: Row) =>
  row.status === "EXTRA_TARGET" || row.status === "BOTH_MISSING"
    ? "—"
    : formatBytes(row.sourceSize);
const destinationSize = (row: Row) =>
  row.status === "MISSING_TARGET" || row.status === "BOTH_MISSING"
    ? "—"
    : formatBytes(row.destinationSize);

/**
 * Loads the comparison list; refreshes itself while a comparison runs, and
 * more slowly while some node cannot be reached.
 */
function useComparisonHistory(token: number) {
  const [data, setData] = useState<History>();
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const latest = useRef<History>(undefined);
  useEffect(() => {
    let canceled = false;
    const controller = new AbortController();
    let timer: number | undefined;
    const running = (history?: History) =>
      (history?.items ?? []).some((item) => isActive(item.status));
    const unreachable = (history?: History) =>
      history?.complete === false ||
      (history?.items ?? []).some((item) => item.status === "UNAVAILABLE");
    const delay = (history?: History) =>
      running(history)
        ? HISTORY_POLL_MS
        : unreachable(history)
          ? HISTORY_SLOW_POLL_MS
          : undefined;
    function poll() {
      timer = undefined;
      // Hidden tabs keep their timer but do not query every node.
      if (document.hidden)
        timer = window.setTimeout(
          poll,
          delay(latest.current) ?? HISTORY_POLL_MS,
        );
      else void load(true);
    }
    async function load(background: boolean) {
      // Background refreshes keep the refresh button quiet.
      if (!background) setLoading(true);
      try {
        const result = await api<History>("/api/master/comparisons", {
          signal: controller.signal,
        });
        if (canceled) return;
        latest.current = result;
        setData(result);
        setError("");
      } catch (e) {
        if (canceled || isAbort(e)) return;
        setError(errorMessage(e));
      } finally {
        if (!canceled) {
          setLoading(false);
          const next = delay(latest.current);
          if (next !== undefined) timer = window.setTimeout(poll, next);
        }
      }
    }
    void load(false);
    return () => {
      canceled = true;
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [token]);
  return { data, error, loading };
}

export default function ObjectComparison() {
  useI18n();
  const [params, setParams] = useSearchParams();
  const id = params.get("comparison") || "";
  const [historyToken, setHistoryToken] = useState(0);
  const [resultToken, setResultToken] = useState(0);
  const history = useComparisonHistory(historyToken);
  const refreshHistory = useCallback(
    () => setHistoryToken((value) => value + 1),
    [],
  );
  const resultRef = useRef<HTMLElement>(null);
  const reveal = useRef(false);
  const showResult = useCallback(() => {
    const panel = resultRef.current;
    if (!panel) return;
    // Bring the panel's top into view unless it is already well inside it.
    const top = panel.getBoundingClientRect().top;
    if (top < 0 || top > window.innerHeight * 0.6) {
      const reduce = window.matchMedia?.(
        "(prefers-reduced-motion: reduce)",
      ).matches;
      panel.scrollIntoView({
        block: "start",
        behavior: reduce ? "auto" : "smooth",
      });
    }
    panel.focus({ preventScroll: true });
  }, []);
  const select = useCallback(
    (next: string) => {
      if (next === id) {
        showResult();
        return;
      }
      reveal.current = !!next;
      setParams((previous) => {
        const query = new URLSearchParams(previous);
        if (next) query.set("comparison", next);
        else query.delete("comparison");
        return query;
      });
    },
    [id, setParams, showResult],
  );
  useEffect(() => {
    if (!reveal.current || !id) return;
    reveal.current = false;
    showResult();
  }, [id, showResult]);
  const listed = history.data?.items?.find((item) => item.id === id);
  return (
    <Stack spacing={2.5}>
      <ComparisonForm
        onStarted={(next) => {
          select(next);
          refreshHistory();
        }}
      />
      <ComparisonHistory
        history={history.data}
        error={history.error}
        loading={history.loading}
        selectedId={id}
        onSelect={select}
        onRefresh={() => {
          refreshHistory();
          setResultToken((value) => value + 1);
        }}
      />
      {id && (
        <ComparisonResult
          key={id}
          id={id}
          listed={listed}
          reloadToken={resultToken}
          panelRef={resultRef}
          onChanged={refreshHistory}
          onDeleted={() => {
            select("");
            refreshHistory();
          }}
        />
      )}
    </Stack>
  );
}

function ComparisonForm({ onStarted }: { onStarted: (id: string) => void }) {
  useI18n();
  const [params] = useSearchParams();
  const [form, setForm] = useState(() => ({
    sourceBucket: params.get("sourceBucket") || "",
    destBucket: params.get("destBucket") || "",
    sourcePrefix: params.get("sourceKey") || params.get("sourcePrefix") || "",
    destPrefix: params.get("destKey") || params.get("destPrefix") || "",
    scope: params.has("sourceKey") ? "object" : "prefix",
    mode: "metadata",
  }));
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  async function start(event: FormEvent) {
    event.preventDefault();
    if (submitting) return;
    setSubmitting(true);
    setError("");
    try {
      const result = await api<Progress>("/api/master/comparisons", {
        method: "POST",
        body: {
          ...form,
          sourceBucket: form.sourceBucket.trim(),
          destBucket: form.destBucket.trim(),
        },
      });
      onStarted(result.id);
    } catch (e) {
      // An uncertain reply still names the comparison the Master recorded.
      if (e instanceof ApiError && typeof e.response?.id === "string")
        onStarted(e.response.id);
      setError(errorMessage(e));
    } finally {
      setSubmitting(false);
    }
  }
  const field = (
    key: "sourceBucket" | "destBucket" | "sourcePrefix" | "destPrefix",
    label: string,
    required = false,
  ) => (
    <TextField
      fullWidth
      label={label}
      value={form[key]}
      required={required}
      disabled={submitting}
      onChange={(e) =>
        setForm((previous) => ({ ...previous, [key]: e.target.value }))
      }
    />
  );
  return (
    <Paper
      variant="outlined"
      component="form"
      onSubmit={start}
      sx={{ p: { xs: 2, md: 3 } }}
    >
      <Stack spacing={2.5}>
        <Stack direction={{ xs: "column", sm: "row" }} spacing={2}>
          <SelectField
            label={t("比对范围")}
            value={form.scope}
            disabled={submitting}
            onChange={(e) =>
              setForm((previous) => ({ ...previous, scope: e.target.value }))
            }
            sx={{ minWidth: 180 }}
          >
            <MenuItem value="prefix">{t("存储桶 / 前缀")}</MenuItem>
            <MenuItem value="object">{t("单个对象")}</MenuItem>
          </SelectField>
          <SelectField
            label={t("比对方式")}
            value={form.mode}
            disabled={submitting}
            onChange={(e) =>
              setForm((previous) => ({ ...previous, mode: e.target.value }))
            }
            sx={{ minWidth: 240 }}
          >
            <MenuItem value="metadata">{t("元数据（不读取内容）")}</MenuItem>
            <MenuItem value="content">{t("内容 SHA-256（流式）")}</MenuItem>
          </SelectField>
        </Stack>
        <Box
          sx={{
            display: "grid",
            gridTemplateColumns: { xs: "1fr", md: "1fr 1fr" },
            gap: 3,
          }}
        >
          <Stack spacing={2}>
            {field("sourceBucket", t("源存储桶"), true)}
            {field(
              "sourcePrefix",
              form.scope === "object" ? t("源对象 Key") : t("源前缀"),
              form.scope === "object",
            )}
          </Stack>
          <Stack spacing={2}>
            {field("destBucket", t("目标存储桶"), true)}
            {field(
              "destPrefix",
              form.scope === "object" ? t("目标对象 Key") : t("目标前缀"),
              form.scope === "object",
            )}
          </Stack>
        </Box>
        <Typography variant="body2" color="text.secondary">
          {t(
            "存储桶填写为“连接名称/存储桶”，如 production/photos。前缀留空比对整个存储桶，两端按去掉前缀后的 Key 对应。",
          )}
        </Typography>
        <Alert severity="info">
          {form.mode === "metadata"
            ? t(
                "比较大小、Content-Type、Content-Encoding、Content-Disposition、Cache-Control 和自定义元数据，不读取对象内容。元数据一致不代表内容一致：两端 ETag 都是普通 MD5 形式且不同时，结果标为“ETag 不同”。需要确认内容时使用内容 SHA-256 模式。",
              )
            : t(
                "完整读取两端对象流并计算 SHA-256，不保存对象文件到本地磁盘。会产生读取流量；大对象需要更多时间。",
              )}
        </Alert>
        {error && <Alert severity="error">{error}</Alert>}
        <Stack direction="row" justifyContent="flex-end">
          <Button type="submit" variant="contained" disabled={submitting}>
            {t("开始比对")}
          </Button>
        </Stack>
      </Stack>
    </Paper>
  );
}

function ComparisonHistory({
  history,
  error,
  loading,
  selectedId,
  onSelect,
  onRefresh,
}: {
  history?: History;
  error: string;
  loading: boolean;
  selectedId: string;
  onSelect: (id: string) => void;
  onRefresh: () => void;
}) {
  useI18n();
  const { box, table, fits: wide } = useFittingTable(HISTORY_TABLE_WIDTH);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState(() =>
    savedPageSize(PAGE_SIZE_PREFERENCE),
  );
  const items = history?.items ?? [];
  const pages = Math.max(1, Math.ceil(items.length / pageSize));
  const current = Math.min(page, pages - 1);
  const shown = items.slice(current * pageSize, (current + 1) * pageSize);
  const unavailable = history?.unavailableNodes ?? [];
  const nodeText = (item: HistoryItem) => item.nodeName || item.nodeId || "—";
  const countText = (item: HistoryItem, value?: number) =>
    item.status === "UNAVAILABLE" ? "—" : formatNumber(value);
  return (
    <Box>
      <Paper ref={box} variant="outlined" sx={{ overflow: "hidden" }}>
        <Stack
          direction="row"
          alignItems="center"
          justifyContent="space-between"
          gap={1}
          sx={{ px: { xs: 2, md: 3 }, py: 2 }}
        >
          <Stack direction="row" alignItems="center" gap={1.5}>
            <Typography variant="h6" component="h2">
              {t("比对历史")}
            </Typography>
            {history && (
              <Chip
                size="small"
                label={formatNumber(items.length)}
                sx={{ height: 22, fontSize: 12 }}
              />
            )}
          </Stack>
          <Button
            size="small"
            startIcon={
              loading ? (
                <CircularProgress size={16} color="inherit" />
              ) : (
                <RefreshRounded />
              )
            }
            disabled={loading}
            onClick={onRefresh}
          >
            {t("刷新")}
          </Button>
        </Stack>
        {(error || history?.complete === false) && (
          <Stack spacing={1.5} sx={{ px: { xs: 2, md: 3 }, pb: 2 }}>
            {error && (
              <Alert
                severity={history ? "warning" : "error"}
                action={
                  <Button color="inherit" size="small" onClick={onRefresh}>
                    {t("重试")}
                  </Button>
                }
              >
                {error}
              </Alert>
            )}
            {history?.complete === false && (
              <Alert severity="warning">
                {unavailable.length > 0
                  ? t(
                      "{count} 个节点暂时无法连接，这些节点上的比对未列出最新状态。",
                      { count: formatNumber(unavailable.length) },
                    )
                  : t("部分节点暂时无法连接，比对列表可能不完整。")}
              </Alert>
            )}
          </Stack>
        )}
        {!history ? (
          !error && (
            <Stack
              direction="row"
              spacing={1.5}
              alignItems="center"
              sx={{ px: { xs: 2, md: 3 }, pb: 2.5 }}
            >
              <CircularProgress size={18} />
              <Typography variant="body2" color="text.secondary">
                {t("加载中…")}
              </Typography>
            </Stack>
          )
        ) : items.length === 0 ? (
          <Typography
            variant="body2"
            color="text.secondary"
            sx={{ px: { xs: 2, md: 3 }, pb: 2.5 }}
          >
            {t("暂无比对记录")}
          </Typography>
        ) : (
          <>
            {wide ? (
              <TableContainer ref={table}>
                <Table
                  size="small"
                  aria-label={t("比对历史")}
                  // Short values keep one line; the route takes the rest.
                  sx={{ "& .MuiTableCell-root": { whiteSpace: "nowrap" } }}
                >
                  <TableHead>
                    <TableRow>
                      <TableCell>{t("状态")}</TableCell>
                      <TableCell sx={{ width: "100%" }}>
                        {t("源 → 目标")}
                      </TableCell>
                      <TableCell>{t("比对方式")}</TableCell>
                      <TableCell align="right">{t("已检查")}</TableCell>
                      <TableCell align="right">{t("差异")}</TableCell>
                      <TableCell>{t("开始时间")}</TableCell>
                      <TableCell>{t("节点")}</TableCell>
                    </TableRow>
                  </TableHead>
                  <TableBody>
                    {shown.map((item) => {
                      const selected = item.id === selectedId;
                      const different = differences(item);
                      return (
                        <TableRow
                          key={`${item.nodeId}/${item.id}`}
                          hover
                          selected={selected}
                          onClick={() => onSelect(item.id)}
                          sx={{ cursor: "pointer" }}
                        >
                          <TableCell>
                            <StateChip status={item.status} />
                          </TableCell>
                          <TableCell
                            sx={{
                              "&&": { whiteSpace: "normal" },
                              minWidth: 200,
                            }}
                          >
                            <Link
                              component="button"
                              type="button"
                              variant="body2"
                              underline="hover"
                              aria-current={selected ? "true" : undefined}
                              onClick={(event) => {
                                event.stopPropagation();
                                onSelect(item.id);
                              }}
                              sx={{
                                textAlign: "left",
                                overflowWrap: "anywhere",
                                fontWeight: selected ? 600 : undefined,
                              }}
                            >
                              {routeText(item)}
                            </Link>
                          </TableCell>
                          <TableCell>{modeLabel(item.mode)}</TableCell>
                          <TableCell align="right">
                            {countText(item, item.compared)}
                          </TableCell>
                          <TableCell
                            align="right"
                            sx={{
                              color:
                                different > 0 && item.status !== "UNAVAILABLE"
                                  ? "warning.main"
                                  : undefined,
                            }}
                          >
                            {countText(item, different)}
                          </TableCell>
                          <TableCell>{timeText(item.startedAt)}</TableCell>
                          <TableCell
                            sx={{
                              "&&": { whiteSpace: "normal" },
                              overflowWrap: "anywhere",
                              minWidth: 96,
                              maxWidth: 180,
                            }}
                          >
                            {nodeText(item)}
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </TableContainer>
            ) : (
              <List
                disablePadding
                aria-label={t("比对历史")}
                sx={{ borderTop: 1, borderColor: "divider" }}
              >
                {shown.map((item) => {
                  const selected = item.id === selectedId;
                  return (
                    <ListItemButton
                      key={`${item.nodeId}/${item.id}`}
                      selected={selected}
                      aria-current={selected ? "true" : undefined}
                      divider
                      onClick={() => onSelect(item.id)}
                      sx={{ display: "block", px: 2, py: 1.5 }}
                    >
                      <Stack
                        direction="row"
                        alignItems="center"
                        gap={1}
                        sx={{ mb: 0.75 }}
                      >
                        <StateChip status={item.status} />
                        <Typography variant="caption" color="text.secondary">
                          {modeLabel(item.mode)}
                        </Typography>
                      </Stack>
                      <Typography
                        variant="body2"
                        sx={{ overflowWrap: "anywhere" }}
                      >
                        {routeText(item)}
                      </Typography>
                      <Typography
                        variant="caption"
                        color="text.secondary"
                        display="block"
                      >
                        {`${t("已检查")} ${countText(item, item.compared)} · ${t("差异")} ${countText(item, differences(item))}`}
                      </Typography>
                      <Typography
                        variant="caption"
                        color="text.secondary"
                        display="block"
                        sx={{ overflowWrap: "anywhere" }}
                      >
                        {`${timeText(item.startedAt)} · ${nodeText(item)}`}
                      </Typography>
                    </ListItemButton>
                  );
                })}
              </List>
            )}
            {items.length > listPageSizes[0] && (
              <ListPagination
                ariaLabel={t("比对历史分页")}
                count={items.length}
                page={current}
                pageSize={pageSize}
                onPageChange={setPage}
                onPageSizeChange={(size) => {
                  setPage(Math.floor((current * pageSize) / size));
                  setPageSize(size);
                  rememberPageSize(PAGE_SIZE_PREFERENCE, size);
                }}
              />
            )}
          </>
        )}
      </Paper>
      <Typography
        variant="caption"
        color="text.secondary"
        component="p"
        sx={{ mt: 1.5, px: 0.5 }}
      >
        {t(
          "比对结果保存在执行节点上，默认保留 30 天；每个节点的比对结果默认最多占用 2 GiB，超出时先删除最早结束的比对。",
        )}
      </Typography>
    </Box>
  );
}

function ComparisonResult({
  id,
  listed,
  reloadToken,
  panelRef,
  onChanged,
  onDeleted,
}: {
  id: string;
  listed?: HistoryItem;
  reloadToken: number;
  panelRef: RefObject<HTMLElement | null>;
  onChanged: () => void;
  onDeleted: () => void;
}) {
  useI18n();
  const notify = useNotify();
  const [progress, setProgress] = useState<Progress | null>(null);
  const [filter, setFilter] = useState("");
  // Start positions of the pages visited so far; the last one is shown.
  const [cursors, setCursors] = useState<number[]>([0]);
  const cursor = cursors[cursors.length - 1];
  const requestKey = `${filter}|${cursor}`;
  const [view, setView] = useState<RowsView | null>(null);
  const [searching, setSearching] = useState(false);
  const [stalled, setStalled] = useState<{ key: string; next: number }>();
  const [loadError, setLoadError] = useState("");
  const [notFound, setNotFound] = useState(false);
  // The node holding the results cannot be reached; retried with backoff.
  const [nodeDown, setNodeDown] = useState(false);
  const nodeDownRef = useRef(false);
  nodeDownRef.current = nodeDown;
  const retryDelay = useRef(RETRY_MS);
  const listedStatusRef = useRef(listed?.status);
  listedStatusRef.current = listed?.status;
  const [reload, setReload] = useState(0);
  const [busy, setBusy] = useState<"" | "cancel" | "resume" | "delete">("");
  const [exporting, setExporting] = useState(false);
  const [actionError, setActionError] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);
  const rowsRef = useRef<HTMLDivElement>(null);
  const changed = useRef(onChanged);
  changed.current = onChanged;
  // Stops the current load and its polling until the next effect run.
  const stopLoading = useRef(() => {});

  useEffect(() => {
    let canceled = false;
    const controller = new AbortController();
    let timer: number | undefined;
    const key = `${filter}|${cursor}`;
    const query = filter ? `&status=${encodeURIComponent(filter)}` : "";
    function poll(after: number) {
      timer = undefined;
      if (document.hidden)
        timer = window.setTimeout(() => poll(after), PAGE_POLL_MS);
      else void load(after, 0);
    }
    function loaded(latest: Progress) {
      setProgress(latest);
      setNotFound(false);
      setLoadError("");
      setNodeDown(false);
      retryDelay.current = RETRY_MS;
    }
    function failed(error: unknown, retry: () => void) {
      if (canceled || isAbort(error)) return;
      setSearching(false);
      if (error instanceof ApiError && error.status === 404) {
        setNotFound(true);
        setProgress(null);
        setView(null);
        changed.current();
        return;
      }
      if (
        (error instanceof ApiError && error.status === 503) ||
        listedStatusRef.current === "UNAVAILABLE"
      ) {
        // The node is offline: check back less and less often.
        setNodeDown(true);
        setLoadError("");
        const wait = retryDelay.current;
        retryDelay.current = Math.min(wait * 2, MAX_RETRY_MS);
        timer = window.setTimeout(retry, wait);
        return;
      }
      setNodeDown(false);
      setLoadError(errorMessage(error));
      timer = window.setTimeout(retry, RETRY_MS);
    }
    // While a filtered scan waits for "keep searching", only the progress of
    // a running comparison is refreshed; the cursor and the alert stay.
    function watch() {
      timer = undefined;
      if (document.hidden) timer = window.setTimeout(watch, PAGE_POLL_MS);
      else void refresh();
    }
    async function refresh() {
      try {
        const page = await api<Page>(`${comparisonPath(id)}?after=0&limit=1`, {
          signal: controller.signal,
        });
        if (canceled) return;
        const { items, nextAfter, ...latest } = page;
        loaded(latest);
        if (isActive(latest.status))
          timer = window.setTimeout(watch, PAGE_POLL_MS);
      } catch (error) {
        failed(error, watch);
      }
    }
    async function load(after: number, hops: number): Promise<void> {
      try {
        const page = await api<Page>(
          `${comparisonPath(id)}?after=${after}&limit=${ROWS_PER_PAGE}${query}`,
          { signal: controller.signal },
        );
        if (canceled) return;
        const { items = [], nextAfter = 0, ...latest } = page;
        loaded(latest);
        if (items.length === 0 && nextAfter > after) {
          // Nothing matched within the scan budget, but stored rows remain.
          if (hops < AUTO_CONTINUE) {
            setSearching(true);
            return load(nextAfter, hops + 1);
          }
          setSearching(false);
          setStalled({ key, next: nextAfter });
          if (isActive(latest.status))
            timer = window.setTimeout(watch, PAGE_POLL_MS);
          return;
        }
        setSearching(false);
        setView({ key, after, items, nextAfter });
        if (isActive(latest.status))
          timer = window.setTimeout(() => poll(after), PAGE_POLL_MS);
      } catch (error) {
        failed(error, () => poll(after));
      }
    }
    const stop = () => {
      canceled = true;
      controller.abort();
      window.clearTimeout(timer);
    };
    stopLoading.current = stop;
    void load(cursor, 0);
    return stop;
  }, [id, filter, cursor, reload, reloadToken]);

  // The history list shows the status too; refresh it when this one moves on.
  const lastStatus = useRef<string>(undefined);
  const status = progress?.status;
  useEffect(() => {
    if (!status) return;
    // The first load also refreshes a history that shows another state,
    // for example a node that was unavailable and is back.
    const previous = lastStatus.current ?? listedStatusRef.current;
    if (previous && previous !== status) changed.current();
    lastStatus.current = status;
  }, [status]);
  // Conversely, reload when the history sees a newer state, for example a
  // comparison resumed from the CLI while this panel was not polling, or a
  // node that is reachable again.
  const listedStatus = listed?.status;
  useEffect(() => {
    if (
      listedStatus &&
      listedStatus !== "UNAVAILABLE" &&
      (nodeDownRef.current ||
        (lastStatus.current && listedStatus !== lastStatus.current))
    )
      setReload((value) => value + 1);
  }, [listedStatus]);

  const active = isActive(progress?.status);
  const current = view?.key === requestKey;
  const shownRows = view?.items ?? [];
  const stalledHere = stalled?.key === requestKey ? stalled : undefined;
  const unavailable =
    nodeDown || (!progress && !notFound && listed?.status === "UNAVAILABLE");

  function choose(next: string) {
    if (next === filter) return;
    setFilter(next);
    setCursors([0]);
  }
  function move(next: number[]) {
    setCursors(next);
    const top = rowsRef.current?.getBoundingClientRect().top ?? 0;
    if (top < 0) rowsRef.current?.scrollIntoView({ block: "start" });
  }
  /** Runs cancel or resume; an older poll reply cannot overwrite its result. */
  async function act(action: "cancel" | "resume") {
    setBusy(action);
    setActionError("");
    stopLoading.current();
    try {
      const result = await api<Progress>(`${comparisonPath(id)}/${action}`, {
        method: "POST",
      });
      setProgress((previous) =>
        previous ? { ...previous, ...result } : result,
      );
      changed.current();
    } catch (e) {
      setActionError(errorMessage(e));
    } finally {
      setBusy("");
      // Reload the page and restart polling whatever the outcome.
      setReload((value) => value + 1);
    }
  }
  async function remove() {
    setBusy("delete");
    setActionError("");
    try {
      await api(comparisonPath(id), { method: "DELETE" });
      setConfirmDelete(false);
      notify(t("比对结果已删除。"), "success");
      onDeleted();
    } catch (e) {
      setConfirmDelete(false);
      if (e instanceof ApiError && e.status === 404) onDeleted();
      else setActionError(errorMessage(e));
    } finally {
      setBusy("");
    }
  }
  async function exportResults() {
    if (exporting) return;
    setExporting(true);
    const failed = (message: string) =>
      notify(t("比对结果导出失败：{message}", { message }), "error");
    try {
      await ticketDownload({
        ticketPath: `${comparisonPath(id)}/export-ticket?format=zip`,
        downloadPath: `${comparisonPath(id)}/export`,
        frameName: "comparison-export",
        frameTitle: t("比对结果导出"),
        dataset: { comparisonExport: id },
        messages: {
          invalidTicket: () => t("导出票据无效，请重试。"),
          failed: () => t("导出失败，请重试。"),
          noAttachment: () => t("服务未返回导出文件，请重试。"),
          unreadable: () => t("无法读取导出结果，请检查浏览器下载列表或重试。"),
        },
        onError: failed,
      });
      notify(t("导出已请求，请查看浏览器下载列表。"), "info");
    } catch (e) {
      failed(errorMessage(e));
    } finally {
      setExporting(false);
    }
  }

  const summary: Progress | undefined = progress ?? listed;
  return (
    <Paper
      variant="outlined"
      component="section"
      ref={panelRef}
      tabIndex={-1}
      aria-label={t("比对结果")}
      sx={{
        p: { xs: 2, md: 3 },
        scrollMarginTop: 16,
        "&:focus": { outline: "none" },
      }}
    >
      <Stack spacing={2}>
        {summary && (
          <Stack
            direction="row"
            justifyContent="space-between"
            alignItems="flex-start"
            gap={1.5}
          >
            <Box sx={{ minWidth: 0 }}>
              <Typography variant="h6" component="h2">
                {notFound ? t("比对结果") : stateLabel(summary.status)}
              </Typography>
              <Typography
                variant="body2"
                sx={{ overflowWrap: "anywhere", mt: 0.5 }}
              >
                {routeText(summary)}
              </Typography>
            </Box>
            <Chip size="small" label={modeLabel(summary.mode)} />
          </Stack>
        )}
        {summary && (
          <Box
            component="dl"
            sx={{
              display: "grid",
              gridTemplateColumns: "max-content minmax(0, 1fr)",
              columnGap: 2,
              rowGap: 0.25,
              m: 0,
              "& dt": { color: "text.secondary" },
              "& dd": { m: 0, overflowWrap: "anywhere" },
            }}
          >
            {listed?.nodeName || listed?.nodeId ? (
              <>
                <Typography component="dt" variant="caption">
                  {t("节点")}
                </Typography>
                <Typography component="dd" variant="caption">
                  {listed.nodeName || listed.nodeId}
                </Typography>
              </>
            ) : null}
            <Typography component="dt" variant="caption">
              {t("开始时间")}
            </Typography>
            <Typography component="dd" variant="caption">
              {timeText(summary.startedAt)}
            </Typography>
            {timeText(summary.endedAt) !== "—" && (
              <>
                <Typography component="dt" variant="caption">
                  {t("结束时间")}
                </Typography>
                <Typography component="dd" variant="caption">
                  {timeText(summary.endedAt)}
                </Typography>
              </>
            )}
            <Typography component="dt" variant="caption">
              {t("比对 ID")}
            </Typography>
            <Typography
              component="dd"
              variant="caption"
              sx={{ fontFamily: "var(--font-mono)" }}
            >
              {id}
            </Typography>
          </Box>
        )}
        {notFound && (
          <Alert severity="info">{t("比对结果不存在或已被删除。")}</Alert>
        )}
        {!progress && !notFound && !loadError && !unavailable && (
          <Stack direction="row" spacing={1.5} alignItems="center">
            <CircularProgress size={18} />
            <Typography variant="body2" color="text.secondary">
              {t("加载中…")}
            </Typography>
          </Stack>
        )}
        {unavailable && (
          <Alert severity="warning">
            {t("执行节点暂时无法连接，节点恢复后将自动加载比对结果。")}
          </Alert>
        )}
        {loadError && (
          <Alert severity={progress ? "warning" : "error"}>
            <RuntimeMessageLine line={loadError} variant="inherit" />
          </Alert>
        )}
        {progress && (
          <>
            {active && <LinearProgress aria-label={t("比对进度")} />}
            <Box sx={{ display: "flex", gap: 2.5, flexWrap: "wrap" }}>
              <Typography variant="body2">{statsText(progress)}</Typography>
              <Typography variant="body2">
                {`${t("一致")} ${formatNumber(progress.matched)}`}
              </Typography>
              {!!progress.resumes && (
                <Typography variant="body2" color="text.secondary">
                  {t("已从检查点继续 {count} 次（如节点重启后）", {
                    count: formatNumber(progress.resumes),
                  })}
                </Typography>
              )}
            </Box>
            <Box
              role="group"
              aria-label={t("按结果筛选")}
              sx={{ display: "flex", flexWrap: "wrap", gap: 1 }}
            >
              {filters.map((option) => {
                const selected = option.status === filter;
                return (
                  <Chip
                    key={option.status || "all"}
                    label={`${option.label()} ${formatNumber(option.count(progress))}`}
                    clickable
                    color={selected ? "primary" : "default"}
                    variant={selected ? "filled" : "outlined"}
                    aria-pressed={selected}
                    onClick={() => choose(option.status)}
                  />
                );
              })}
            </Box>
            {progress.rowsTruncated && (
              <Alert severity="warning">{truncationText(progress)}</Alert>
            )}
            {(progress.error || progress.errorDetail) && (
              <Alert
                severity={progress.status === "FAILED" ? "error" : "warning"}
              >
                {progress.error && <Box>{t(progress.error)}</Box>}
                {progress.errorDetail && (
                  <Box
                    component="code"
                    sx={{
                      display: "block",
                      mt: progress.error ? 0.5 : 0,
                      fontSize: 13,
                      whiteSpace: "pre-wrap",
                      overflowWrap: "anywhere",
                    }}
                  >
                    {progress.errorDetail}
                  </Box>
                )}
              </Alert>
            )}
            {actionError && (
              <Alert severity="error">
                <RuntimeMessageLine line={actionError} variant="inherit" />
              </Alert>
            )}
            <Box>
              <Stack
                direction={{ xs: "column", sm: "row" }}
                gap={1.5}
                flexWrap="wrap"
                alignItems={{ xs: "stretch", sm: "center" }}
              >
                {active && (
                  <Button
                    variant="outlined"
                    color="warning"
                    startIcon={<StopCircleOutlined />}
                    disabled={!!busy || progress.status === "CANCELING"}
                    onClick={() => void act("cancel")}
                  >
                    {t("取消比对")}
                  </Button>
                )}
                {!active && progress.resumable && (
                  <Button
                    variant="contained"
                    startIcon={<PlayArrowRounded />}
                    disabled={!!busy}
                    onClick={() => void act("resume")}
                  >
                    {t("继续比对")}
                  </Button>
                )}
                <Button
                  variant="outlined"
                  startIcon={<FileDownloadOutlined />}
                  disabled={exporting}
                  onClick={() => void exportResults()}
                >
                  {exporting ? t("正在准备下载") : t("导出全部差异（ZIP）")}
                </Button>
                {!active && (
                  <Button
                    color="error"
                    startIcon={<DeleteOutlineRounded />}
                    disabled={!!busy}
                    onClick={() => setConfirmDelete(true)}
                  >
                    {t("删除比对结果")}
                  </Button>
                )}
              </Stack>
              {!active && progress.resumable && (
                <Typography
                  variant="caption"
                  color="text.secondary"
                  component="p"
                  sx={{ mt: 1 }}
                >
                  {t("从已比对到的位置继续，已统计的结果保留。")}
                </Typography>
              )}
              {filter && (
                <Typography
                  variant="caption"
                  color="text.secondary"
                  component="p"
                  sx={{ mt: 1 }}
                >
                  {t("导出包含全部已保存的差异，不受当前筛选影响。")}
                </Typography>
              )}
            </Box>
            <Divider />
            <Typography variant="body2" color="text.secondary">
              {t(
                "下方仅列出差异和未能确认的对象。结果反映比对时的状态；持续写入的存储桶不提供同一时刻的快照。",
              )}
            </Typography>
            <Box
              ref={rowsRef}
              aria-busy={!current || searching}
              sx={{ scrollMarginTop: 16 }}
            >
              {(searching || (!current && !stalledHere)) && (
                <LinearProgress aria-label={t("加载中…")} sx={{ mb: 1.5 }} />
              )}
              {searching && (
                <Typography variant="body2" color="text.secondary">
                  {t("正在查找符合条件的差异…")}
                </Typography>
              )}
              {stalledHere && !searching && (
                <Alert
                  severity="info"
                  action={
                    <Button
                      color="inherit"
                      size="small"
                      onClick={() =>
                        setCursors((previous) => [
                          ...previous.slice(0, -1),
                          stalledHere.next,
                        ])
                      }
                    >
                      {t("继续查找")}
                    </Button>
                  }
                >
                  {t("已查找到第 {number} 条，暂未找到符合条件的差异。", {
                    number: formatNumber(stalledHere.next),
                  })}
                </Alert>
              )}
              {current && shownRows.length === 0 && !stalledHere && (
                <Typography variant="body2" color="text.secondary">
                  {filter
                    ? t("没有符合条件的差异。")
                    : differences(progress) > 0
                      ? t("暂无已保存的差异明细。")
                      : active
                        ? t("暂未发现差异。")
                        : t("未发现差异。")}
                </Typography>
              )}
              {shownRows.length > 0 && !stalledHere && (
                <Box
                  sx={{
                    opacity: current ? 1 : 0.5,
                    transition: "opacity .2s",
                  }}
                >
                  <RowsTable
                    rows={shownRows}
                    content={progress.mode === "content"}
                  />
                </Box>
              )}
              {(cursors.length > 1 || (current && !!view?.nextAfter)) && (
                <Stack
                  component="nav"
                  aria-label={t("差异明细分页")}
                  direction="row"
                  alignItems="center"
                  justifyContent="space-between"
                  gap={1}
                  sx={{ mt: 1.5 }}
                >
                  <Button
                    startIcon={<ChevronLeftRounded />}
                    disabled={cursors.length <= 1}
                    onClick={() => move(cursors.slice(0, -1))}
                  >
                    {t("上一页")}
                  </Button>
                  <Typography variant="body2" color="text.secondary">
                    {t("第 {page} 页", { page: cursors.length })}
                  </Typography>
                  <Button
                    endIcon={<ChevronRightRounded />}
                    disabled={!current || !view?.nextAfter}
                    onClick={() =>
                      view &&
                      move([
                        ...cursors.slice(0, -1),
                        view.after,
                        view.nextAfter,
                      ])
                    }
                  >
                    {t("下一页")}
                  </Button>
                </Stack>
              )}
            </Box>
          </>
        )}
      </Stack>
      <Dialog
        open={confirmDelete}
        onClose={() => busy !== "delete" && setConfirmDelete(false)}
        fullWidth
        maxWidth="xs"
      >
        <DialogTitle>{t("删除比对结果？")}</DialogTitle>
        <DialogContent>
          <DialogContentText>
            {t(
              "将从执行节点删除这次比对的统计和差异明细，无法恢复。源端和目标端的对象不受影响。",
            )}
          </DialogContentText>
        </DialogContent>
        <DialogActions>
          <Button
            disabled={busy === "delete"}
            onClick={() => setConfirmDelete(false)}
          >
            {t("取消")}
          </Button>
          <Button
            color="error"
            variant="contained"
            disabled={busy === "delete"}
            onClick={() => void remove()}
          >
            {t("删除")}
          </Button>
        </DialogActions>
      </Dialog>
    </Paper>
  );
}

function RowResult({ row }: { row: Row }) {
  const reasons = reasonsText(row.reasons);
  return (
    <>
      <Typography variant="body2" component="span" display="block">
        {rowStatusLabel(row.status)}
      </Typography>
      {reasons && (
        <Typography variant="caption" display="block" color="text.secondary">
          {reasons}
        </Typography>
      )}
      {row.error && (
        <Typography variant="caption" display="block">
          {t(row.error)}
        </Typography>
      )}
      {row.errorDetail && (
        <Typography
          variant="caption"
          display="block"
          color="text.secondary"
          sx={{ fontFamily: "var(--font-mono)", overflowWrap: "anywhere" }}
        >
          {row.errorDetail}
        </Typography>
      )}
    </>
  );
}

function RowsTable({ rows, content }: { rows: Row[]; content: boolean }) {
  useI18n();
  const [box, width] = useElementWidth<HTMLDivElement>();
  const wide = width >= (content ? CONTENT_ROWS_TABLE_WIDTH : ROWS_TABLE_WIDTH);
  return (
    <Box ref={box}>
      {wide ? (
        <RowsGrid rows={rows} content={content} />
      ) : (
        <RowsList rows={rows} content={content} />
      )}
    </Box>
  );
}

function RowsGrid({ rows, content }: { rows: Row[]; content: boolean }) {
  const top = { verticalAlign: "top" } as const;
  // A hash keeps one line; the full value stays in the text and the tooltip.
  const hash = {
    fontFamily: "var(--font-mono)",
    overflow: "hidden",
    textOverflow: "ellipsis",
    whiteSpace: "nowrap",
  } as const;
  return (
    <TableContainer>
      <Table
        size="small"
        sx={{ tableLayout: "fixed" }}
        aria-label={t("差异明细")}
      >
        <TableHead>
          <TableRow>
            <TableCell>{t("源对象 Key")}</TableCell>
            <TableCell>{t("目标对象 Key")}</TableCell>
            <TableCell sx={{ width: content ? 168 : 200 }}>
              {t("结果")}
            </TableCell>
            <TableCell align="right" sx={{ width: 104 }}>
              {t("源大小")}
            </TableCell>
            <TableCell align="right" sx={{ width: 104 }}>
              {t("目标大小")}
            </TableCell>
            {content && <TableCell sx={{ width: 160 }}>SHA-256</TableCell>}
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map((row) => (
            <TableRow key={row.number}>
              <TableCell sx={{ ...top, overflowWrap: "anywhere" }}>
                {row.sourceKey}
              </TableCell>
              <TableCell sx={{ ...top, overflowWrap: "anywhere" }}>
                {row.destinationKey}
              </TableCell>
              <TableCell sx={top}>
                <RowResult row={row} />
              </TableCell>
              <TableCell align="right" sx={{ ...top, whiteSpace: "nowrap" }}>
                {sourceSize(row)}
              </TableCell>
              <TableCell align="right" sx={{ ...top, whiteSpace: "nowrap" }}>
                {destinationSize(row)}
              </TableCell>
              {content && (
                <TableCell sx={top}>
                  {[row.sourceSha256, row.destinationSha256].map(
                    (value, index) => (
                      <Typography
                        key={index}
                        variant="caption"
                        display="block"
                        title={value}
                        sx={hash}
                      >
                        {value || "—"}
                      </Typography>
                    ),
                  )}
                </TableCell>
              )}
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}

function RowsList({ rows, content }: { rows: Row[]; content: boolean }) {
  const hash = {
    fontFamily: "var(--font-mono)",
    overflowWrap: "anywhere",
  } as const;
  return (
    <Box
      component="ul"
      aria-label={t("差异明细")}
      sx={{ listStyle: "none", m: 0, p: 0 }}
    >
      {rows.map((row) => (
        <Box
          component="li"
          key={row.number}
          sx={{
            py: 1.5,
            "&:not(:last-of-type)": {
              borderBottom: 1,
              borderColor: "divider",
            },
          }}
        >
          <RowResult row={row} />
          <Box
            sx={{
              display: "grid",
              gridTemplateColumns: "auto minmax(0, 1fr) auto",
              columnGap: 1.5,
              rowGap: 0.5,
              mt: 1,
              alignItems: "baseline",
            }}
          >
            <Typography variant="caption" color="text.secondary">
              {t("源")}
            </Typography>
            <Typography variant="body2" sx={{ overflowWrap: "anywhere" }}>
              {row.sourceKey}
            </Typography>
            <Typography variant="body2" sx={{ whiteSpace: "nowrap" }}>
              {sourceSize(row)}
            </Typography>
            <Typography variant="caption" color="text.secondary">
              {t("目标")}
            </Typography>
            <Typography variant="body2" sx={{ overflowWrap: "anywhere" }}>
              {row.destinationKey}
            </Typography>
            <Typography variant="body2" sx={{ whiteSpace: "nowrap" }}>
              {destinationSize(row)}
            </Typography>
          </Box>
          {content && (row.sourceSha256 || row.destinationSha256) && (
            <Box sx={{ mt: 1 }}>
              <Typography variant="caption" sx={hash} display="block">
                {row.sourceSha256 || "—"}
              </Typography>
              <Typography variant="caption" sx={hash} display="block">
                {row.destinationSha256 || "—"}
              </Typography>
            </Box>
          )}
        </Box>
      ))}
    </Box>
  );
}
