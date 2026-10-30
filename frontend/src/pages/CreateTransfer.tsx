import { t as tr, useI18n, getLocale } from "../lib/i18n";
import { Link } from "react-router-dom";
import { useEffect, useRef, useState, type ReactNode } from "react";
import {
  Accordion,
  AccordionDetails,
  AccordionSummary,
  Alert,
  AlertTitle,
  Autocomplete,
  Box,
  Button,
  Chip,
  Checkbox,
  CircularProgress,
  Divider,
  IconButton,
  LinearProgress,
  MenuItem,
  Paper,
  Stack,
  Tab,
  Tabs,
  TextField,
  Tooltip,
  Typography,
} from "@mui/material";
import {
  AddRounded,
  ArrowForwardRounded,
  CheckCircleOutlineRounded,
  CloudDownloadOutlined,
  CloudOutlined,
  CloudUploadOutlined,
  CodeRounded,
  DeleteOutlineRounded,
  ExpandMoreRounded,
  FolderOutlined,
  RefreshRounded,
  RocketLaunchOutlined,
  StorageOutlined,
  TuneRounded,
} from "@mui/icons-material";
import { api, useApi, useNotify } from "../lib/api";
import type { NodeDisplay } from "../lib/nodeRegistry";
import { useNodeLabels } from "../lib/workerStatus";
import { PageHeader } from "../components/Common";
import { TaskBandwidthField } from "../components/TaskBandwidthField";
import { TaskRequestLimitsFields } from "../components/TaskRequestLimitsFields";
import {
  parseTaskRequestLimits,
  validateTaskRequestLimits,
} from "../lib/taskRequestLimits";
import {
  formatTaskBandwidth,
  parseTaskBandwidth,
  validateTaskBandwidth,
} from "../lib/taskBandwidth";
import {
  buildKeyMapping,
  keyMappingLabels,
  mappingModes,
  validateOssKeyMapping,
  type OssKeyMappingMode,
} from "../lib/ossKeyMapping";

type Direction = "OSS_TO_OSS" | "OSS_TO_LOCAL" | "LOCAL_TO_OSS";
type Connection = { alias: string; endpoint: string };
type Node = NodeDisplay & {
  executionNodeIp: string;
  label?: string;
  hostname?: string;
  localIps?: string[];
  connectionAddress?: string;
  reachable: boolean;
  localTransferSupported: boolean;
};
function isAbsoluteAgentPath(value: string) {
  if (value.startsWith("/")) return true;
  return (
    /^[A-Za-z]:[\\/]/.test(value) || /^\\\\[^\\]+\\[^\\]+(?:\\|$)/.test(value)
  );
}

type Binding = { executionNodeIp: string; localPath: string };
type TransferForm = {
  direction: Direction;
  sourceAlias: string;
  sourceBucket: string;
  destinationAlias: string;
  destinationBucket: string;
  sourceMode: "all" | "prefix" | "object";
  sourceSelection: string;
  destinationMode: "original" | "object" | "prefix";
  destinationKey: string;
  ossMappingMode: OssKeyMappingMode;
  ossTargetPrefix: string;
  ossTargetKey: string;
  targetRelativePath: string;
  bindings: Binding[];
  eligibleWorkerNodes: string[] | null;
  conflictPolicy: "SKIP" | "OVERWRITE";
  distributionMode: "affinity" | "balanced";
  dateAfter: string;
  dateBefore: string;
  taskBandwidthLimitMbps: string;
  listQps: string;
  headQps: string;
  putQps: string;
};
type Precheck = {
  valid: boolean;
  nodeBindings: Array<
    Binding & {
      valid: boolean;
      message?: string;
      code?: string;
      pathType?: string;
    }
  >;
};
type Submission = {
  severity: "success" | "warning" | "error";
  title: string;
  message: string;
  details?: string[];
  taskIds?: number[];
};
type Payload = Record<string, unknown>;
type Draft = {
  version: 1;
  form: TransferForm;
  tab: number;
  batchKind: "buckets" | "tasks" | "local";
  batchFormat: "json" | "csv";
  batchText: string;
  pendingRequest?: { id: string; fingerprint: string };
};

const DRAFT_KEY = "oss-transfer-create-draft";
const localUploadMode = (nodeCount: number) =>
  nodeCount > 1 ? "balanced" : "affinity";
const distributionLabel = (mode: string | undefined, direction?: string) =>
  mode === "balanced"
    ? tr("均衡分发")
    : direction === "LOCAL_TO_OSS"
      ? tr("单节点执行")
      : tr("亲和分发");
const freshForm = (): TransferForm => ({
  direction: "OSS_TO_OSS",
  sourceAlias: "",
  sourceBucket: "",
  destinationAlias: "",
  destinationBucket: "",
  sourceMode: "all",
  sourceSelection: "",
  destinationMode: "original",
  destinationKey: "",
  ossMappingMode: "PRESERVE",
  ossTargetPrefix: "",
  ossTargetKey: "",
  targetRelativePath: "",
  bindings: [{ executionNodeIp: "", localPath: "" }],
  eligibleWorkerNodes: null,
  conflictPolicy: "SKIP",
  distributionMode: "affinity",
  dateAfter: "",
  dateBefore: "",
  taskBandwidthLimitMbps: "",
  listQps: "",
  headQps: "",
  putQps: "",
});

const errorText = (e: unknown) => (e instanceof Error ? e.message : String(e));
const dateValue = (value: string) =>
  value.length === 16 ? `${value}:00` : value;
const optional = (value: string) => value.trim() || undefined;

function cleanMappingFields(form: TransferForm): TransferForm {
  const next = { ...form };
  if (
    next.direction !== "OSS_TO_OSS" ||
    !mappingModes(next.sourceMode).includes(next.ossMappingMode)
  )
    next.ossMappingMode = "PRESERVE";
  if (!["ADD_PREFIX", "REPLACE_PREFIX"].includes(next.ossMappingMode))
    next.ossTargetPrefix = "";
  if (next.ossMappingMode !== "EXACT_KEY") next.ossTargetKey = "";
  if (next.sourceMode === "all") next.sourceSelection = "";
  return next;
}

function validateOssDestination(
  sourceAlias: unknown,
  sourceBucket: unknown,
  destinationAlias: unknown,
  destinationBucket: unknown,
  context = "",
) {
  const alias = (value: unknown) =>
    typeof value === "string" && value.trim() ? value.trim() : "default";
  if (
    typeof sourceBucket === "string" &&
    typeof destinationBucket === "string" &&
    sourceBucket.trim() === destinationBucket.trim() &&
    alias(sourceAlias) === alias(destinationAlias)
  )
    throw new Error(
      tr("{value1}源和目标不能是同一存储桶，请选择其他目标存储桶。", {
        value1: context,
      }),
    );
}

function readDraft(): Draft | null {
  try {
    const saved = sessionStorage.getItem(DRAFT_KEY);
    if (!saved) return null;
    const draft = JSON.parse(saved) as Draft;
    if (
      draft.version !== 1 ||
      !draft.form ||
      ![0, 1].includes(draft.tab) ||
      !["buckets", "tasks", "local"].includes(draft.batchKind) ||
      !["json", "csv"].includes(draft.batchFormat) ||
      typeof draft.batchText !== "string"
    )
      return null;
    const baseline = freshForm();
    const form = { ...baseline, ...draft.form };
    delete (form as TransferForm & { storageId?: unknown }).storageId;
    if (
      !["OSS_TO_OSS", "OSS_TO_LOCAL", "LOCAL_TO_OSS"].includes(
        form.direction,
      ) ||
      !["all", "prefix", "object"].includes(form.sourceMode) ||
      !["original", "object", "prefix"].includes(form.destinationMode) ||
      !["PRESERVE", "ADD_PREFIX", "REPLACE_PREFIX", "EXACT_KEY"].includes(
        form.ossMappingMode,
      ) ||
      !["SKIP", "OVERWRITE"].includes(form.conflictPolicy) ||
      !["affinity", "balanced"].includes(form.distributionMode) ||
      !Array.isArray(form.bindings) ||
      form.bindings.length === 0 ||
      form.bindings.some(
        (binding) =>
          typeof binding?.executionNodeIp !== "string" ||
          typeof binding?.localPath !== "string",
      ) ||
      (form.eligibleWorkerNodes !== null &&
        (!Array.isArray(form.eligibleWorkerNodes) ||
          form.eligibleWorkerNodes.some((node) => typeof node !== "string"))) ||
      Object.keys(baseline).some(
        (key) =>
          key !== "bindings" &&
          key !== "eligibleWorkerNodes" &&
          typeof form[key as keyof TransferForm] !== "string",
      )
    )
      return null;
    if (
      draft.pendingRequest &&
      (!/^[a-f0-9]{32}$/.test(draft.pendingRequest.id) ||
        typeof draft.pendingRequest.fingerprint !== "string")
    )
      delete draft.pendingRequest;
    return { ...draft, form: cleanMappingFields(form) };
  } catch {
    return null;
  }
}

function clearSavedDraft() {
  try {
    sessionStorage.removeItem(DRAFT_KEY);
  } catch {
    /* Draft storage may be unavailable in restricted browsers. */
  }
}

function EndpointCard({
  title,
  description,
  icon,
  children,
}: {
  title: string;
  description: string;
  icon: ReactNode;
  children: ReactNode;
}) {
  useI18n();
  return (
    <Paper variant="outlined" sx={{ p: { xs: 2.5, md: 3 }, minWidth: 0 }}>
      <Stack direction="row" spacing={1.5} alignItems="center" sx={{ mb: 3 }}>
        <Box
          sx={{
            p: 1.25,
            borderRadius: "12px",
            display: "flex",
            bgcolor: "action.selected",
            color: "primary.main",
          }}
        >
          {icon}
        </Box>
        <Box>
          <Typography variant="h6">{title}</Typography>
          <Typography variant="body2" color="text.secondary">
            {description}
          </Typography>
        </Box>
      </Stack>
      <Stack spacing={2.5}>{children}</Stack>
    </Paper>
  );
}

function OssFields({
  side,
  configs,
  alias,
  bucket,
  onAlias,
  onBucket,
}: {
  side: "源" | "目标";
  configs: Connection[];
  alias: string;
  bucket: string;
  onAlias: (value: string) => void;
  onBucket: (value: string) => void;
}) {
  useI18n();
  const [buckets, setBuckets] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const activeRequest = useRef(0);
  useEffect(() => {
    activeRequest.current += 1;
    setBuckets([]);
    setError(null);
    setLoading(false);
  }, [alias]);
  const fetchBuckets = async () => {
    if (!alias) return;
    const request = ++activeRequest.current;
    setLoading(true);
    setError(null);
    try {
      const result = await api<{ buckets: string[] }>(
        `/api/master/s3-config/${encodeURIComponent(alias)}/buckets`,
      );
      if (request === activeRequest.current) setBuckets(result.buckets ?? []);
    } catch (e) {
      if (request === activeRequest.current) setError(errorText(e));
    } finally {
      if (request === activeRequest.current) setLoading(false);
    }
  };
  return (
    <>
      <TextField
        select
        required
        fullWidth
        label={tr("{value1}存储连接", { value1: tr(side) })}
        value={alias}
        onChange={(e) => onAlias(e.target.value)}
        helperText={configs.find((config) => config.alias === alias)?.endpoint}
        slotProps={{
          formHelperText: { sx: { overflowWrap: "anywhere" } },
        }}
      >
        {configs.map((config) => (
          <MenuItem key={config.alias} value={config.alias}>
            {config.alias}
          </MenuItem>
        ))}
      </TextField>
      <Stack spacing={1}>
        <Autocomplete
          freeSolo
          options={buckets}
          value={bucket}
          inputValue={bucket}
          onInputChange={(_, value) => onBucket(value)}
          onChange={(_, value) => onBucket(value || "")}
          loading={loading}
          renderInput={(params) => (
            <TextField
              {...params}
              required
              label={tr("{value1}存储桶", { value1: tr(side) })}
              placeholder={tr("输入存储桶名称")}
              helperText={tr("输入存储桶名称，或获取列表后选择。")}
            />
          )}
        />
        <Box>
          <Button
            size="small"
            disabled={!alias || loading}
            startIcon={
              loading ? <CircularProgress size={15} /> : <RefreshRounded />
            }
            onClick={() => void fetchBuckets()}
          >
            {loading ? tr("正在获取存储桶列表") : tr("获取存储桶列表")}
          </Button>
          {buckets.length > 0 && (
            <Typography
              component="span"
              variant="caption"
              color="text.secondary"
              sx={{ ml: 1 }}
            >
              {tr("{count} 个可选", {
                count: buckets.length.toLocaleString(getLocale()),
              })}
            </Typography>
          )}
        </Box>
        {error && (
          <Alert severity="warning" sx={{ fontSize: 12 }}>
            {tr(
              "无法获取存储桶列表：{error}。如有对应存储桶的访问权限，可直接填写名称。",
              { error },
            )}
          </Alert>
        )}
      </Stack>
    </>
  );
}

export default function CreateTransfer() {
  const directions: Array<{
    value: Direction;
    title: string;
    description: string;
    icon: ReactNode;
  }> = [
    {
      value: "OSS_TO_OSS",
      title: tr("对象存储 → 对象存储"),
      description: tr("跨存储桶或云平台迁移"),
      icon: <CloudOutlined />,
    },
    {
      value: "OSS_TO_LOCAL",
      title: tr("对象存储 → 本地"),
      description: tr("下载至执行节点或共享存储"),
      icon: <CloudDownloadOutlined />,
    },
    {
      value: "LOCAL_TO_OSS",
      title: tr("本地 → 对象存储"),
      description: tr("上传本地文件或目录"),
      icon: <CloudUploadOutlined />,
    },
  ];

  useI18n();
  const connections = useApi<{ configs: Connection[] }>(
    "/api/master/s3-config",
  );
  const localNodes = useApi<{ data: { nodes: Node[] } }>(
    "/api/master/local-nodes",
  );
  const notify = useNotify();
  const [initialDraft] = useState(readDraft);
  const [draftRestored, setDraftRestored] = useState(Boolean(initialDraft));
  const [tab, setTab] = useState(initialDraft?.tab ?? 0);
  const [form, setForm] = useState<TransferForm>(
    () => initialDraft?.form ?? freshForm(),
  );
  const [busy, setBusy] = useState<"create" | "validate" | null>(null);
  const [precheck, setPrecheck] = useState<Precheck | null>(null);
  const precheckRequest = useRef<string | null>(null);
  const [submission, setSubmission] = useState<Submission | null>(null);
  const [submitted, setSubmitted] = useState(false);
  const [batchKind, setBatchKind] = useState<"buckets" | "tasks" | "local">(
    initialDraft?.batchKind ?? "buckets",
  );
  const [batchFormat, setBatchFormat] = useState<"json" | "csv">(
    initialDraft?.batchFormat ?? "json",
  );
  const [batchText, setBatchText] = useState(initialDraft?.batchText ?? "");
  const submittedDraft = useRef(false);
  const submissionInFlight = useRef(false);
  const pendingRequest = useRef(initialDraft?.pendingRequest);
  const defaultNodeSelection = useRef<string[] | null>(null);
  useEffect(() => {
    if (
      ((submitted || submittedDraft.current) && !pendingRequest.current) ||
      (JSON.stringify({ ...form, eligibleWorkerNodes: null }) ===
        JSON.stringify(freshForm()) &&
        (form.eligibleWorkerNodes === null ||
          form.eligibleWorkerNodes === defaultNodeSelection.current) &&
        !batchText)
    ) {
      clearSavedDraft();
      return;
    }
    const draft: Draft = {
      version: 1,
      form,
      tab,
      batchKind,
      batchFormat,
      batchText,
      pendingRequest: pendingRequest.current,
    };
    try {
      sessionStorage.setItem(DRAFT_KEY, JSON.stringify(draft));
    } catch {
      /* Keep the form usable if session storage is full or disabled. */
    }
  }, [form, tab, batchKind, batchFormat, batchText, submitted]);
  const configs = connections.data?.configs ?? [];
  const nodes = localNodes.data?.data?.nodes ?? [];
  const resolveNode = useNodeLabels();
  const nodeLabel = (id: string) => {
    const node = nodes.find((item) => item.executionNodeIp === id);
    return resolveNode(id, node);
  };
  const availableLocalNodes = nodes.filter(
    (node) => node.reachable && node.localTransferSupported,
  );
  const nextLocalNode = availableLocalNodes.find(
    (node) =>
      !form.bindings.some(
        (binding) => binding.executionNodeIp === node.executionNodeIp,
      ),
  );
  useEffect(() => {
    if (!localNodes.data || form.eligibleWorkerNodes !== null) return;
    const available = (localNodes.data.data?.nodes ?? [])
      .filter((node) => node.reachable)
      .map((node) => node.executionNodeIp);
    defaultNodeSelection.current = available;
    setForm((current) =>
      current.eligibleWorkerNodes !== null
        ? current
        : { ...current, eligibleWorkerNodes: available },
    );
  }, [localNodes.data, form.eligibleWorkerNodes]);
  const selectedWorkerNodes =
    form.eligibleWorkerNodes ??
    nodes.filter((node) => node.reachable).map((node) => node.executionNodeIp);
  const unavailableSelection = selectedWorkerNodes.some(
    (ip) =>
      !nodes.some((node) => node.executionNodeIp === ip && node.reachable),
  );
  const local = form.direction !== "OSS_TO_OSS";
  const requiresPrecheck = tab === 0 ? local : batchKind === "local";
  const effectiveDistributionMode =
    form.direction === "LOCAL_TO_OSS"
      ? localUploadMode(form.bindings.length)
      : form.distributionMode;
  const clearFeedback = (inputChanged = true) => {
    setPrecheck(null);
    precheckRequest.current = null;
    setSubmission(null);
    setSubmitted(false);
    if (inputChanged) submittedDraft.current = false;
  };
  const update = (patch: Partial<TransferForm>) => {
    setForm((current) => {
      const next = { ...current, ...patch };
      if (patch.sourceMode && patch.sourceMode !== current.sourceMode) {
        next.sourceSelection = "";
        next.targetRelativePath = "";
      }
      if (
        patch.ossMappingMode &&
        patch.ossMappingMode !== current.ossMappingMode
      ) {
        next.ossTargetPrefix = "";
        next.ossTargetKey = "";
      }
      return cleanMappingFields(next);
    });
    clearFeedback();
  };
  const setBinding = (index: number, patch: Partial<Binding>) =>
    update({
      bindings: form.bindings.map((binding, i) =>
        i === index ? { ...binding, ...patch } : binding,
      ),
    });
  const resetDraft = () => {
    pendingRequest.current = undefined;
    setForm(freshForm());
    setTab(0);
    setBatchKind("buckets");
    setBatchFormat("json");
    setBatchText("");
    clearFeedback();
    setDraftRestored(false);
    clearSavedDraft();
  };

  const buildRequest = (): Payload => {
    if (form.dateAfter && form.dateBefore && form.dateBefore <= form.dateAfter)
      throw new Error(tr("结束时间必须晚于开始时间。"));
    const sourceOss = form.direction !== "LOCAL_TO_OSS";
    const destinationOss = form.direction !== "OSS_TO_LOCAL";
    if (sourceOss && (!form.sourceAlias || !form.sourceBucket.trim()))
      throw new Error(tr("请选择源存储连接并填写源存储桶。"));
    if (
      destinationOss &&
      (!form.destinationAlias || !form.destinationBucket.trim())
    )
      throw new Error(tr("请选择目标存储连接并填写目标存储桶。"));
    if (sourceOss && form.sourceMode !== "all" && !form.sourceSelection)
      throw new Error(tr("请填写源对象前缀或对象 Key。"));
    const common = {
      dateAfter: optional(dateValue(form.dateAfter)),
      dateBefore: optional(dateValue(form.dateBefore)),
      distributionMode: effectiveDistributionMode,
      requestLimits: parseTaskRequestLimits(form),
      taskBandwidthLimitMbps:
        parseTaskBandwidth(form.taskBandwidthLimitMbps) ?? undefined,
    };
    if (!local) {
      if (!selectedWorkerNodes.length)
        throw new Error(tr("请至少选择一个执行节点。"));
      if (unavailableSelection)
        throw new Error(tr("所选执行节点不可用，请刷新节点列表或重新选择。"));
      validateOssDestination(
        form.sourceAlias,
        form.sourceBucket,
        form.destinationAlias,
        form.destinationBucket,
      );
      return {
        ...common,
        sourceS3Alias: form.sourceAlias,
        sourceBucket: form.sourceBucket.trim(),
        destS3Alias: form.destinationAlias,
        destBucket: form.destinationBucket.trim(),
        prefix: form.sourceMode === "prefix" ? form.sourceSelection : undefined,
        objectKey:
          form.sourceMode === "object" ? form.sourceSelection : undefined,
        keyMapping: buildKeyMapping(
          form.sourceMode,
          form.sourceSelection,
          form.ossMappingMode,
          form.ossTargetPrefix,
          form.ossTargetKey,
        ),
        overwrite: form.conflictPolicy === "OVERWRITE",
        eligibleWorkerNodes: selectedWorkerNodes,
      };
    }
    if (
      form.bindings.some(
        (binding) =>
          !binding.executionNodeIp.trim() || !binding.localPath.trim(),
      )
    )
      throw new Error(tr("请填写每个执行节点及其本地绝对路径。"));
    if (
      form.bindings.some(
        (binding) => !isAbsoluteAgentPath(binding.localPath.trim()),
      )
    )
      throw new Error(tr("请输入执行节点上的绝对路径。"));
    if (
      new Set(form.bindings.map((binding) => binding.executionNodeIp.trim()))
        .size !== form.bindings.length
    )
      throw new Error(tr("同一个执行节点只能绑定一次。"));
    if (
      form.bindings.some(
        (binding) =>
          !availableLocalNodes.some(
            (node) => node.executionNodeIp === binding.executionNodeIp,
          ),
      )
    )
      throw new Error(tr("所选执行节点不可用，请刷新节点列表或重新选择。"));
    if (
      !sourceOss &&
      form.destinationMode !== "original" &&
      !form.destinationKey
    )
      throw new Error(tr("请填写目标对象 Key 或目标前缀。"));
    const localEndpoint = {
      type: "LOCAL",
      nodeBindings: form.bindings.map((binding) => ({
        executionNodeIp: binding.executionNodeIp.trim(),
        localPath: binding.localPath.trim(),
      })),
    };
    const ossEndpoint = sourceOss
      ? {
          type: "OSS",
          s3Alias: form.sourceAlias,
          bucket: form.sourceBucket.trim(),
          objectKey:
            form.sourceMode === "object" ? form.sourceSelection : undefined,
          prefix:
            form.sourceMode === "prefix" ? form.sourceSelection : undefined,
        }
      : {
          type: "OSS",
          s3Alias: form.destinationAlias,
          bucket: form.destinationBucket.trim(),
          objectKey:
            form.destinationMode === "object" ? form.destinationKey : undefined,
          prefix:
            form.destinationMode === "prefix" ? form.destinationKey : undefined,
        };
    return {
      ...common,
      direction: form.direction,
      conflictPolicy: form.conflictPolicy,
      sourceEndpoint: sourceOss ? ossEndpoint : localEndpoint,
      destinationEndpoint: sourceOss ? localEndpoint : ossEndpoint,
      targetRelativePath:
        form.direction === "OSS_TO_LOCAL" && form.sourceMode === "object"
          ? optional(form.targetRelativePath)
          : undefined,
    };
  };

  const parseBatch = (): Payload[] => {
    if (!batchText.trim())
      throw new Error(tr("请填写任务数据或读取数据文件。"));
    let rows: unknown;
    if (batchKind === "buckets" && batchFormat === "csv") {
      rows = batchText
        .trim()
        .split(/\r?\n/)
        .filter((line) => line.trim())
        .map((line, index) => {
          const cells = line.split(",").map((cell) => cell.trim());
          const source = cells[0]?.split("/"),
            destination = cells[1]?.split("/");
          if (
            source?.length !== 2 ||
            destination?.length !== 2 ||
            !source.every(Boolean) ||
            !destination.every(Boolean)
          )
            throw new Error(
              tr(
                "第 {value1} 行格式错误，请使用“源连接/源桶,目标连接/目标桶”。",
                { value1: index + 1 },
              ),
            );
          if (cells[3] && !["true", "false"].includes(cells[3].toLowerCase()))
            throw new Error(
              tr("第 {value1} 行的覆盖选项必须是 true 或 false。", {
                value1: index + 1,
              }),
            );
          if (cells[4] && !["affinity", "balanced"].includes(cells[4]))
            throw new Error(
              tr("第 {value1} 行的分发模式必须是 affinity 或 balanced。", {
                value1: index + 1,
              }),
            );
          if (cells.length > 5)
            throw new Error(
              tr("第 {value1} 行超过 5 列，其他配置请使用 JSON 格式。", {
                value1: index + 1,
              }),
            );
          return {
            sourceS3Alias: source[0],
            sourceBucket: source[1],
            destS3Alias: destination[0],
            destBucket: destination[1],
            dateAfter: cells[2] || undefined,
            overwrite: cells[3]?.toLowerCase() === "true",
            distributionMode: cells[4] || "affinity",
          };
        });
    } else {
      try {
        rows = JSON.parse(batchText);
      } catch {
        throw new Error(tr("JSON 格式无效，请检查括号、双引号和逗号。"));
      }
    }
    if (!Array.isArray(rows) || rows.length === 0)
      throw new Error(tr("任务数据必须是非空数组。"));
    rows.forEach((row, index) => {
      if (!row || typeof row !== "object" || Array.isArray(row))
        throw new Error(
          tr("第 {value1} 项必须是任务对象。", { value1: index + 1 }),
        );
      try {
        validateTaskBandwidth(row.taskBandwidthLimitMbps);
        validateTaskRequestLimits(row.requestLimits);
      } catch (error) {
        throw new Error(
          tr("第 {value1} 项：{value2}", {
            value1: index + 1,
            value2: (error as Error).message,
          }),
        );
      }
      if (batchKind === "local") {
        if (
          !["OSS_TO_LOCAL", "LOCAL_TO_OSS"].includes(row.direction) ||
          !row.sourceEndpoint ||
          !row.destinationEndpoint
        )
          throw new Error(
            tr(
              "第 {value1} 项的 direction 必须为 OSS_TO_LOCAL 或 LOCAL_TO_OSS，并提供 sourceEndpoint 和 destinationEndpoint。",
              { value1: index + 1 },
            ),
          );
        if (row.direction === "LOCAL_TO_OSS") {
          row.distributionMode = localUploadMode(
            Array.isArray(row.sourceEndpoint.nodeBindings)
              ? row.sourceEndpoint.nodeBindings.length
              : 1,
          );
        }
      } else if (!row.sourceBucket || !row.destBucket)
        throw new Error(
          tr("第 {value1} 项必须填写 sourceBucket 和 destBucket。", {
            value1: index + 1,
          }),
        );
      else if (row.direction && row.direction !== "OSS_TO_OSS")
        throw new Error(
          tr("第 {value1} 项涉及本地路径，请选择“本地迁移请求”。", {
            value1: index + 1,
          }),
        );
      if (batchKind !== "local") {
        try {
          validateOssKeyMapping(row, batchKind);
        } catch (error) {
          throw new Error(
            tr("第 {value1} 项：{value2}", {
              value1: index + 1,
              value2: errorText(error),
            }),
          );
        }
        validateOssDestination(
          row.sourceS3Alias,
          row.sourceBucket,
          row.destS3Alias,
          row.destBucket,
          tr("第 {value1} 项：", { value1: index + 1 }),
        );
      }
    });
    return rows as Payload[];
  };

  const applyUploadPrecheck = (checked: Precheck): Precheck => {
    if (tab !== 0 || form.direction !== "LOCAL_TO_OSS") return checked;
    const nodeBindings = checked.nodeBindings.map((binding) => {
      if (form.bindings.length > 1 && binding.pathType === "FILE")
        return {
          ...binding,
          valid: false,
          code: "SINGLE_FILE_REQUIRES_ONE_NODE",
          message: tr("单文件上传请只保留一个执行节点。"),
        };
      if (
        form.destinationMode === "object" &&
        binding.valid &&
        binding.pathType === "DIRECTORY"
      )
        return {
          ...binding,
          valid: false,
          message: tr(
            "源路径是目录。请选择“保留源文件名 / 相对路径”或“添加目标前缀”；指定对象 Key 仅适用于单文件。",
          ),
        };
      return binding;
    });
    return {
      ...checked,
      nodeBindings,
      valid: checked.valid && nodeBindings.every((binding) => binding.valid),
    };
  };

  const submit = async (validateOnly = false) => {
    if (submissionInFlight.current || submitted) return;
    submissionInFlight.current = true;
    setSubmission(null);
    let payload: Payload[];
    try {
      payload = tab === 0 ? [buildRequest()] : parseBatch();
    } catch (e) {
      setSubmission({
        severity: "error",
        title: tr("任务配置无效"),
        message: errorText(e),
      });
      submissionInFlight.current = false;
      return;
    }
    const isLocal = requiresPrecheck;
    const checkedRequest = JSON.stringify(payload);
    if (
      !validateOnly &&
      isLocal &&
      (!precheck?.valid || precheckRequest.current !== checkedRequest)
    ) {
      notify(tr("请先通过预检，再创建任务。"), "warning");
      submissionInFlight.current = false;
      return;
    }
    setBusy(validateOnly ? "validate" : "create");
    try {
      if (validateOnly) {
        setPrecheck(null);
        precheckRequest.current = null;
        const response = await api<{ data: Precheck }>(
          "/api/master/local-transfers/validate",
          { method: "POST", body: payload },
        );
        const checked = applyUploadPrecheck(response.data);
        setPrecheck(checked);
        precheckRequest.current = checked.valid ? checkedRequest : null;
        notify(
          checked.valid
            ? tr("所有节点预检通过")
            : tr("预检完成，存在未通过的节点。"),
          checked.valid ? "success" : "warning",
        );
        return;
      }
      const path = isLocal
        ? "/api/master/local-transfers"
        : tab === 1 && batchKind === "tasks"
          ? "/api/master/import"
          : "/api/master/add-buckets";
      const fingerprint = JSON.stringify({ path, payload });
      if (pendingRequest.current?.fingerprint !== fingerprint) {
        const bytes = crypto.getRandomValues(new Uint8Array(16));
        pendingRequest.current = {
          id: Array.from(bytes, (byte) =>
            byte.toString(16).padStart(2, "0"),
          ).join(""),
          fingerprint,
        };
      }
      // Persist before sending: a lost response or page reload must retry the
      // same logical creation. A changed payload gets a new request identity.
      try {
        sessionStorage.setItem(
          DRAFT_KEY,
          JSON.stringify({
            version: 1,
            form,
            tab,
            batchKind,
            batchFormat,
            batchText,
            pendingRequest: pendingRequest.current,
          } satisfies Draft),
        );
      } catch {
        // The in-memory identity still protects retries in this page.
      }
      const response = await api<Record<string, unknown>>(path, {
        method: "POST",
        body: payload,
        headers: { "Idempotency-Key": pendingRequest.current.id },
      });
      const result = (isLocal ? response.data : response) as Record<
        string,
        unknown
      >;
      const count = Number(
        result[
          isLocal
            ? "acceptedCount"
            : path.endsWith("/import")
              ? "importedCount"
              : "addedCount"
        ],
      );
      const partial =
        result.partial === true ||
        Number(result.unknownCount || 0) > 0 ||
        !Number.isFinite(count) ||
        count !== payload.length ||
        Number(result.rejectedCount || 0) > 0;
      if (
        isLocal &&
        (Number(result.rejectedCount || 0) > 0 ||
          (Array.isArray(result.validationErrors) &&
            result.validationErrors.length > 0))
      ) {
        setPrecheck(null);
        precheckRequest.current = null;
      }
      const rawIds =
        isLocal && Array.isArray(result.parentTaskIds)
          ? result.parentTaskIds
          : result.taskIds;
      const ids = Array.isArray(rawIds)
        ? [
            ...new Set(
              rawIds
                .map(Number)
                .filter((id) => Number.isSafeInteger(id) && id > 0),
            ),
          ]
        : [];
      const accepted = Array.isArray(result.acceptedNodeBindings)
        ? (result.acceptedNodeBindings as Array<{
            parentTaskId?: number;
            distributionMode?: string;
            nodeBindings?: Binding[];
          }>)
        : [];
      const details = accepted.map((item, index) =>
        tr("任务 #{value1} · {value2} · {value3} 个执行节点", {
          value1: item.parentTaskId ?? "—",
          value2: distributionLabel(
            item.distributionMode,
            String(payload[index]?.direction || ""),
          ),
          value3: item.nodeBindings?.length ?? 0,
        }),
      );
      if (Array.isArray(result.validationErrors))
        details.push(
          ...result.validationErrors.map((item) =>
            typeof item === "string" ? item : JSON.stringify(item),
          ),
        );
      if (partial)
        details.push(
          tr("部分任务可能已受理。重试前请核对任务列表，避免重复提交。"),
        );
      const message = tr(
        "提交 {value1} 个任务，{value2}。{value3}任务自动执行，可在“迁移任务”中查看状态。",
        {
          value1: payload.length,
          value2: Number.isFinite(count)
            ? tr("已受理 {value1} 个", { value1: count })
            : tr("服务端未确认受理数量"),
          value3: ids.length
            ? tr("任务 ID：{value1}。", { value1: ids.join("、") })
            : "",
        },
      );
      setSubmission({
        severity: partial ? "warning" : "success",
        title: partial ? tr("提交结果待核对") : tr("迁移任务已提交"),
        message,
        details,
        taskIds: ids,
      });
      submittedDraft.current = true;
      if (!partial) pendingRequest.current = undefined;
      setSubmitted(true);
      setDraftRestored(false);
      if (!partial) clearSavedDraft();
      notify(
        partial ? tr("部分任务未确认，请核对提交结果") : tr("迁移任务已提交"),
        partial ? "warning" : "success",
      );
    } catch (e) {
      setSubmission({
        severity: "error",
        title: validateOnly ? tr("预检未通过") : tr("任务提交失败"),
        message: errorText(e),
        details: validateOnly
          ? undefined
          : [
              tr(
                "服务端可能已受理部分任务。重试前请核对任务列表，避免重复提交。",
              ),
            ],
      });
      if (validateOnly) setPrecheck(null);
    } finally {
      submissionInFlight.current = false;
      setBusy(null);
    }
  };

  const localFields = (
    <>
      {localNodes.error && (
        <Alert
          severity="warning"
          action={
            <Button
              color="inherit"
              size="small"
              onClick={() => void localNodes.refresh()}
            >
              {tr("重试")}
            </Button>
          }
        >
          {tr("无法获取节点信息：")}
          {localNodes.error}
        </Alert>
      )}
      {localNodes.loading && !localNodes.data && <LinearProgress />}
      <Stack spacing={2.5}>
        {form.bindings.map((binding, index) => {
          const selectedNode = nodes.find(
            (node) => node.executionNodeIp === binding.executionNodeIp,
          );
          return (
            <Box
              key={index}
              sx={{
                border: "1px solid",
                borderColor: "divider",
                borderRadius: "12px",
                p: 2,
              }}
            >
              <Stack
                direction="row"
                alignItems="center"
                justifyContent="space-between"
                sx={{ mb: 2 }}
              >
                <Typography variant="subtitle2">
                  {tr("执行节点 {index}", { index: index + 1 })}
                </Typography>
                {form.bindings.length > 1 && (
                  <IconButton
                    size="small"
                    aria-label={tr("移除执行节点 {value1}", {
                      value1: index + 1,
                    })}
                    onClick={() =>
                      update({
                        bindings: form.bindings.filter((_, i) => i !== index),
                      })
                    }
                  >
                    <DeleteOutlineRounded fontSize="small" />
                  </IconButton>
                )}
              </Stack>
              <Stack spacing={2}>
                <Autocomplete
                  options={Array.from(
                    new Set([
                      ...nodes.map((node) => node.executionNodeIp),
                      ...(binding.executionNodeIp
                        ? [binding.executionNodeIp]
                        : []),
                    ]),
                  )}
                  value={binding.executionNodeIp || null}
                  getOptionLabel={nodeLabel}
                  getOptionDisabled={(id) =>
                    !availableLocalNodes.some(
                      (node) => node.executionNodeIp === id,
                    ) ||
                    form.bindings.some(
                      (other, otherIndex) =>
                        otherIndex !== index && other.executionNodeIp === id,
                    )
                  }
                  onChange={(_, value) =>
                    setBinding(index, { executionNodeIp: value || "" })
                  }
                  noOptionsText={tr("暂无可用节点")}
                  renderOption={(props, ip) => {
                    const node = nodes.find(
                      (item) => item.executionNodeIp === ip,
                    );
                    const { key, ...optionProps } = props;
                    return (
                      <li {...optionProps} key={key}>
                        <Box sx={{ flex: 1 }}>{nodeLabel(ip)}</Box>
                        <Chip
                          size="small"
                          label={
                            node?.reachable && node?.localTransferSupported
                              ? tr("可用")
                              : tr("不可用")
                          }
                          color={
                            node?.reachable && node?.localTransferSupported
                              ? "success"
                              : "default"
                          }
                        />
                      </li>
                    );
                  }}
                  renderInput={(params) => (
                    <TextField
                      {...params}
                      required
                      label={tr("执行节点")}
                      placeholder={tr("选择执行节点")}
                    />
                  )}
                />
                <TextField
                  required
                  fullWidth
                  label={
                    form.direction === "LOCAL_TO_OSS"
                      ? tr("源文件或目录绝对路径")
                      : tr("目标目录绝对路径")
                  }
                  placeholder="/mnt/storage/transfer"
                  value={binding.localPath}
                  onChange={(e) =>
                    setBinding(index, { localPath: e.target.value })
                  }
                />
                {selectedNode &&
                  (!selectedNode.reachable ||
                    !selectedNode.localTransferSupported) && (
                    <Alert severity="warning">
                      {tr("节点不可达或不支持本地迁移，请检查节点状态。")}
                    </Alert>
                  )}
              </Stack>
            </Box>
          );
        })}
        <Button
          variant="outlined"
          startIcon={<AddRounded />}
          sx={{ borderRadius: "12px" }}
          disabled={
            !nextLocalNode ||
            form.bindings.some((binding) => !binding.executionNodeIp)
          }
          onClick={() => {
            if (!nextLocalNode) return;
            update({
              bindings: [
                ...form.bindings,
                {
                  executionNodeIp: nextLocalNode.executionNodeIp,
                  localPath: form.bindings[0]?.localPath || "",
                },
              ],
            });
          }}
        >
          {tr("添加共享存储节点")}
        </Button>
      </Stack>
      {form.bindings.length > 1 && (
        <Alert severity="info">
          {tr("多个节点必须访问同一共享存储中的同一份数据，挂载路径可以不同。")}
        </Alert>
      )}
      {form.direction === "OSS_TO_LOCAL" && form.sourceMode === "object" && (
        <TextField
          label={tr("目标相对路径（可选）")}
          value={form.targetRelativePath}
          onChange={(e) => update({ targetRelativePath: e.target.value })}
          placeholder="archive/renamed-file.zip"
          helperText={tr("指定单个对象在目标目录内的文件名或相对路径。")}
        />
      )}
    </>
  );

  const sourceCard = (
    <EndpointCard
      title={tr("迁移源")}
      description={
        form.direction === "LOCAL_TO_OSS"
          ? tr("执行节点上的文件或目录")
          : tr("待迁移的存储桶或对象")
      }
      icon={
        form.direction === "LOCAL_TO_OSS" ? (
          <StorageOutlined />
        ) : (
          <CloudOutlined />
        )
      }
    >
      {form.direction === "LOCAL_TO_OSS" ? (
        localFields
      ) : (
        <>
          <OssFields
            side="源"
            configs={configs}
            alias={form.sourceAlias}
            bucket={form.sourceBucket}
            onAlias={(value) =>
              update({ sourceAlias: value, sourceBucket: "" })
            }
            onBucket={(value) => update({ sourceBucket: value })}
          />
          <TextField
            select
            label={tr("迁移范围")}
            value={form.sourceMode}
            onChange={(e) =>
              update({
                sourceMode: e.target.value as TransferForm["sourceMode"],
              })
            }
          >
            <MenuItem value="all">{tr("整个存储桶")}</MenuItem>
            <MenuItem value="prefix">{tr("按对象前缀筛选")}</MenuItem>
            <MenuItem value="object">{tr("单个对象")}</MenuItem>
          </TextField>
          {form.sourceMode !== "all" && (
            <TextField
              required
              label={
                form.sourceMode === "object"
                  ? tr("源对象 Key")
                  : tr("源对象前缀")
              }
              value={form.sourceSelection}
              onChange={(e) => update({ sourceSelection: e.target.value })}
              placeholder={
                form.sourceMode === "object"
                  ? "backups/database.tar.gz"
                  : "backups/"
              }
              helperText={
                form.sourceMode === "object"
                  ? tr("填写完整对象 Key，区分大小写。")
                  : tr("仅迁移 Key 以此前缀开头的对象，区分大小写。")
              }
            />
          )}
        </>
      )}
    </EndpointCard>
  );
  const destinationCard = (
    <EndpointCard
      title={tr("迁移目标")}
      description={
        form.direction === "OSS_TO_LOCAL"
          ? tr("执行节点或共享存储上的目标目录")
          : tr("接收数据的存储桶")
      }
      icon={
        form.direction === "OSS_TO_LOCAL" ? (
          <FolderOutlined />
        ) : (
          <CloudOutlined />
        )
      }
    >
      {form.direction === "OSS_TO_LOCAL" ? (
        localFields
      ) : (
        <>
          <OssFields
            side="目标"
            configs={configs}
            alias={form.destinationAlias}
            bucket={form.destinationBucket}
            onAlias={(value) =>
              update({ destinationAlias: value, destinationBucket: "" })
            }
            onBucket={(value) => update({ destinationBucket: value })}
          />
          {form.direction === "LOCAL_TO_OSS" ? (
            <>
              <TextField
                select
                label={tr("目标对象命名")}
                value={form.destinationMode}
                onChange={(e) =>
                  update({
                    destinationMode: e.target
                      .value as TransferForm["destinationMode"],
                  })
                }
              >
                <MenuItem value="original">
                  {tr("保留源文件名 / 相对路径")}
                </MenuItem>
                <MenuItem value="prefix">{tr("添加目标前缀")}</MenuItem>
                <MenuItem value="object">{tr("指定单文件目标 Key")}</MenuItem>
              </TextField>
              {form.destinationMode !== "original" && (
                <TextField
                  required
                  label={
                    form.destinationMode === "object"
                      ? tr("目标对象 Key")
                      : tr("目标对象前缀")
                  }
                  value={form.destinationKey}
                  onChange={(e) => update({ destinationKey: e.target.value })}
                  placeholder={
                    form.destinationMode === "object"
                      ? "archive/backup.tar.gz"
                      : "backups/"
                  }
                />
              )}
            </>
          ) : (
            <>
              <TextField
                select
                label={tr("目标对象命名")}
                value={form.ossMappingMode}
                onChange={(event) =>
                  update({
                    ossMappingMode: event.target.value as OssKeyMappingMode,
                  })
                }
              >
                {mappingModes(form.sourceMode).map((mode) => (
                  <MenuItem key={mode} value={mode}>
                    {keyMappingLabels[mode]}
                  </MenuItem>
                ))}
              </TextField>
              {form.ossMappingMode !== "PRESERVE" && (
                <TextField
                  required={form.ossMappingMode !== "REPLACE_PREFIX"}
                  label={
                    form.ossMappingMode === "EXACT_KEY"
                      ? tr("目标对象 Key")
                      : tr("目标对象前缀")
                  }
                  value={
                    form.ossMappingMode === "EXACT_KEY"
                      ? form.ossTargetKey
                      : form.ossTargetPrefix
                  }
                  onChange={(event) =>
                    update(
                      form.ossMappingMode === "EXACT_KEY"
                        ? { ossTargetKey: event.target.value }
                        : { ossTargetPrefix: event.target.value },
                    )
                  }
                  placeholder={
                    form.ossMappingMode === "EXACT_KEY"
                      ? "archive/renamed.txt"
                      : "archive/"
                  }
                />
              )}
            </>
          )}
        </>
      )}
    </EndpointCard>
  );

  const examples = () => {
    const uploadNodes = nodes
      .filter((node) => node.reachable)
      .slice(0, 2)
      .map((node) => ({
        executionNodeIp: node.executionNodeIp,
        localPath: "/mnt/storage/backups",
      }));
    if (uploadNodes.length === 0)
      uploadNodes.push({
        executionNodeIp: "10.0.0.10",
        localPath: "/mnt/storage/backups",
      });
    const sample =
      batchKind === "buckets"
        ? [
            {
              sourceS3Alias: configs[0]?.alias || "source",
              sourceBucket: "source-bucket",
              destS3Alias: configs[1]?.alias || "destination",
              destBucket: "destination-bucket",
              prefix: "backups/",
              keyMapping: {
                mode: "REPLACE_PREFIX",
                sourcePrefix: "backups/",
                targetPrefix: "archive/",
              },
              overwrite: false,
              distributionMode: "affinity",
            },
          ]
        : batchKind === "tasks"
          ? [
              {
                sourceS3Alias: configs[0]?.alias || "source",
                sourceBucket: "source-bucket",
                destS3Alias: configs[1]?.alias || "destination",
                destBucket: "destination-bucket",
                prefix: "backups/",
                startKey: "",
                endKey: "",
                workerNode: "",
                overwrite: false,
                keyMapping: {
                  mode: "ADD_PREFIX",
                  targetPrefix: "archive/",
                },
              },
            ]
          : [
              {
                direction: "OSS_TO_LOCAL",
                sourceEndpoint: {
                  type: "OSS",
                  s3Alias: configs[0]?.alias || "source",
                  bucket: "source-bucket",
                  prefix: "backups/",
                },
                destinationEndpoint: {
                  type: "LOCAL",
                  nodeBindings: [
                    {
                      executionNodeIp: nodes[0]?.executionNodeIp || "10.0.0.10",
                      localPath: "/mnt/storage/backups",
                    },
                  ],
                },
                conflictPolicy: "SKIP",
                distributionMode: "affinity",
              },
              {
                direction: "LOCAL_TO_OSS",
                sourceEndpoint: { type: "LOCAL", nodeBindings: uploadNodes },
                destinationEndpoint: {
                  type: "OSS",
                  s3Alias: configs[0]?.alias || "destination",
                  bucket: "destination-bucket",
                  prefix: "backups/",
                },
                conflictPolicy: "SKIP",
                distributionMode: localUploadMode(uploadNodes.length),
              },
            ];
    return batchKind === "buckets" && batchFormat === "csv"
      ? "source/source-bucket,destination/destination-bucket,,false,affinity"
      : JSON.stringify(sample, null, 2);
  };

  return (
    <Stack spacing={3}>
      <PageHeader
        title={tr("创建迁移")}
        description={tr("配置迁移源、目标、范围和执行方式。")}
        action={
          <Button
            component={Link}
            to="/tasks"
            endIcon={<ArrowForwardRounded />}
          >
            {tr("查看迁移任务")}
          </Button>
        }
      />
      <Tabs
        value={tab}
        onChange={(_, value) => {
          setTab(value);
          clearFeedback(false);
        }}
        aria-label={tr("创建方式")}
        sx={{ borderBottom: "1px solid", borderColor: "divider" }}
      >
        <Tab label={tr("配置迁移")} disabled={Boolean(busy)} />
        <Tab
          disabled={Boolean(busy)}
          label={tr("批量导入")}
          icon={<CodeRounded fontSize="small" />}
          iconPosition="start"
        />
      </Tabs>
      {draftRestored && (
        <Alert severity="info" onClose={() => setDraftRestored(false)}>
          {tr("已恢复当前标签页的草稿。预检结果未保存。")}
        </Alert>
      )}
      {connections.error && (
        <Alert
          severity="error"
          action={
            <Button color="inherit" onClick={() => void connections.refresh()}>
              {tr("重试")}
            </Button>
          }
        >
          {tr("无法加载存储连接：")}
          {connections.error}
        </Alert>
      )}
      {!connections.loading && !connections.error && configs.length === 0 && (
        <Alert
          severity="info"
          action={
            <Button component={Link} to="/storage" color="inherit">
              {tr("添加连接")}
            </Button>
          }
        >
          {tr("创建任务前需添加存储连接。")}
        </Alert>
      )}
      <Box
        component="form"
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <Box
          component="fieldset"
          disabled={Boolean(busy)}
          sx={{ p: 0, m: 0, border: 0, minWidth: 0 }}
        >
          <Stack spacing={3}>
            {tab === 0 ? (
              <>
                <Box
                  sx={{
                    display: "grid",
                    gridTemplateColumns: {
                      xs: "1fr",
                      md: "repeat(3, minmax(0, 1fr))",
                    },
                    gap: 1.5,
                  }}
                >
                  {directions.map((direction) => (
                    <Button
                      key={direction.value}
                      onClick={() =>
                        update({
                          direction: direction.value,
                        })
                      }
                      aria-pressed={form.direction === direction.value}
                      variant="outlined"
                      sx={{
                        p: 2,
                        justifyContent: "flex-start",
                        textAlign: "left",
                        borderRadius: "12px",
                        borderColor:
                          form.direction === direction.value
                            ? "primary.main"
                            : "divider",
                        bgcolor:
                          form.direction === direction.value
                            ? "action.selected"
                            : "background.paper",
                        color:
                          form.direction === direction.value
                            ? "primary.main"
                            : "text.primary",
                      }}
                    >
                      <Stack direction="row" spacing={1.5} alignItems="center">
                        {direction.icon}
                        <Box>
                          <Typography variant="subtitle2">
                            {direction.title}
                          </Typography>
                          <Typography variant="caption" color="text.secondary">
                            {direction.description}
                          </Typography>
                        </Box>
                      </Stack>
                    </Button>
                  ))}
                </Box>
                <Box
                  sx={{
                    display: "grid",
                    gridTemplateColumns: { xs: "1fr", lg: "1fr 1fr" },
                    gap: 2.5,
                    alignItems: "start",
                  }}
                >
                  {sourceCard}
                  {destinationCard}
                </Box>
                <Accordion
                  disableGutters
                  elevation={0}
                  sx={{
                    border: "1px solid",
                    borderColor: "divider",
                    borderRadius: "16px !important",
                    "&:before": { display: "none" },
                  }}
                >
                  <AccordionSummary
                    expandIcon={<ExpandMoreRounded />}
                    sx={{ px: 3 }}
                  >
                    <Stack direction="row" alignItems="center" spacing={1.5}>
                      <TuneRounded color="action" />
                      <Box>
                        <Typography fontWeight={600}>
                          {tr("迁移选项")}
                        </Typography>
                        <Typography variant="caption" color="text.secondary">
                          {tr("冲突处理、节点分发、时间与限速")}
                        </Typography>
                      </Box>
                    </Stack>
                  </AccordionSummary>
                  <AccordionDetails sx={{ px: 3, pb: 3 }}>
                    <Box
                      sx={{
                        display: "grid",
                        gridTemplateColumns: { xs: "1fr", md: "1fr 1fr" },
                        gap: 2.5,
                      }}
                    >
                      {!local && (
                        <Stack
                          direction="row"
                          spacing={1}
                          alignItems="flex-start"
                          sx={{ gridColumn: "1 / -1" }}
                        >
                          <Autocomplete
                            multiple
                            disableCloseOnSelect
                            options={Array.from(
                              new Set([
                                ...nodes.map((node) => node.executionNodeIp),
                                ...selectedWorkerNodes,
                              ]),
                            )}
                            value={selectedWorkerNodes}
                            getOptionLabel={nodeLabel}
                            loading={localNodes.loading}
                            getOptionDisabled={(ip) =>
                              !nodes.some(
                                (node) =>
                                  node.executionNodeIp === ip && node.reachable,
                              )
                            }
                            onChange={(_, value) =>
                              update({ eligibleWorkerNodes: value })
                            }
                            noOptionsText={tr("暂无可用节点")}
                            renderOption={(props, ip, { selected }) => {
                              const { key, ...optionProps } = props;
                              const available = nodes.some(
                                (node) =>
                                  node.executionNodeIp === ip && node.reachable,
                              );
                              return (
                                <li key={key} {...optionProps}>
                                  <Checkbox checked={selected} sx={{ mr: 1 }} />
                                  {nodeLabel(ip)}
                                  {!available && tr(" · 不可用")}
                                </li>
                              );
                            }}
                            renderInput={(params) => (
                              <TextField
                                {...params}
                                label={tr("执行节点")}
                                error={
                                  !localNodes.loading &&
                                  (selectedWorkerNodes.length === 0 ||
                                    unavailableSelection ||
                                    !!localNodes.error)
                                }
                                helperText={
                                  localNodes.error
                                    ? tr("节点列表加载失败")
                                    : !localNodes.loading &&
                                        selectedWorkerNodes.length === 0
                                      ? tr("请至少选择一个执行节点")
                                      : !localNodes.loading &&
                                          unavailableSelection
                                        ? tr("所选节点不可用，请重新选择")
                                        : undefined
                                }
                              />
                            )}
                            sx={{ flex: 1, minWidth: 0 }}
                          />
                          <Tooltip title={tr("刷新执行节点")}>
                            <span>
                              <IconButton
                                aria-label={tr("刷新执行节点")}
                                onClick={localNodes.refresh}
                                disabled={localNodes.loading}
                                sx={{ mt: 1 }}
                              >
                                <RefreshRounded />
                              </IconButton>
                            </span>
                          </Tooltip>
                        </Stack>
                      )}
                      <TextField
                        select
                        label={tr("目标已存在时")}
                        value={form.conflictPolicy}
                        onChange={(e) =>
                          update({
                            conflictPolicy: e.target
                              .value as TransferForm["conflictPolicy"],
                          })
                        }
                        helperText={tr("覆盖会替换目标同名对象或文件。")}
                      >
                        <MenuItem value="SKIP">
                          {tr("跳过同名对象或文件")}
                        </MenuItem>
                        <MenuItem value="OVERWRITE">
                          {tr("覆盖同名对象或文件")}
                        </MenuItem>
                      </TextField>
                      {form.direction !== "LOCAL_TO_OSS" && (
                        <TextField
                          select
                          label={tr("节点分发方式")}
                          value={form.distributionMode}
                          onChange={(e) =>
                            update({
                              distributionMode: e.target
                                .value as TransferForm["distributionMode"],
                            })
                          }
                          helperText={
                            local &&
                            form.bindings.length === 1 &&
                            form.distributionMode === "balanced"
                              ? tr("当前仅绑定一个节点，任务将在该节点执行。")
                              : tr(
                                  "亲和分发优先使用固定节点；均衡分发将任务分配给可用节点。",
                                )
                          }
                        >
                          <MenuItem value="affinity">{tr("亲和分发")}</MenuItem>
                          <MenuItem value="balanced">{tr("均衡分发")}</MenuItem>
                        </TextField>
                      )}
                      <TextField
                        label={tr("修改时间晚于（可选）")}
                        type="datetime-local"
                        value={form.dateAfter}
                        onChange={(e) => update({ dateAfter: e.target.value })}
                        slotProps={{ inputLabel: { shrink: true } }}
                        helperText={tr("时间按服务端时区解释。")}
                      />
                      <TextField
                        label={tr("修改时间早于（可选）")}
                        type="datetime-local"
                        value={form.dateBefore}
                        onChange={(e) => update({ dateBefore: e.target.value })}
                        slotProps={{ inputLabel: { shrink: true } }}
                      />
                      <Box sx={{ gridColumn: "1 / -1" }}>
                        <Divider sx={{ mb: 2.5 }} />
                        <Stack
                          direction="row"
                          spacing={1}
                          alignItems="baseline"
                          sx={{ mb: 2 }}
                        >
                          <Typography variant="subtitle2">
                            {tr("任务限速")}
                          </Typography>
                          <Typography variant="caption" color="text.secondary">
                            {tr("所有执行节点合计")}
                          </Typography>
                        </Stack>
                        <Box
                          sx={{
                            display: "grid",
                            gridTemplateColumns: {
                              xs: "1fr",
                              sm: "1fr 1fr",
                              lg: "repeat(4, minmax(0, 1fr))",
                            },
                            gap: 2,
                          }}
                        >
                          <TaskBandwidthField
                            value={form.taskBandwidthLimitMbps}
                            onChange={(taskBandwidthLimitMbps) =>
                              update({ taskBandwidthLimitMbps })
                            }
                          />
                          <TaskRequestLimitsFields
                            value={form}
                            onChange={(values) => update(values)}
                          />
                        </Box>
                      </Box>
                    </Box>
                  </AccordionDetails>
                </Accordion>
              </>
            ) : (
              <Paper variant="outlined" sx={{ p: { xs: 2.5, md: 3 } }}>
                <Stack spacing={2.5}>
                  <Box>
                    <Typography variant="h6">{tr("批量导入")}</Typography>
                    <Typography
                      variant="body2"
                      color="text.secondary"
                      sx={{ mt: 0.5 }}
                    >
                      {tr(
                        "选择任务类型，粘贴数据或读取文件。提交后保留输入数据供核对。",
                      )}
                    </Typography>
                  </Box>
                  <Box
                    sx={{
                      display: "grid",
                      gridTemplateColumns: { xs: "1fr", sm: "2fr 1fr" },
                      gap: 2,
                    }}
                  >
                    <TextField
                      select
                      label={tr("导入类型")}
                      value={batchKind}
                      onChange={(e) => {
                        setBatchKind(e.target.value as typeof batchKind);
                        clearFeedback();
                      }}
                    >
                      <MenuItem value="buckets">
                        {tr("OSS 存储桶迁移")}
                      </MenuItem>
                      <MenuItem value="tasks">
                        {tr("OSS 子任务恢复 / 导入")}
                      </MenuItem>
                      <MenuItem value="local">{tr("本地迁移请求")}</MenuItem>
                    </TextField>
                    <TextField
                      select
                      label={tr("数据格式")}
                      value={batchKind === "buckets" ? batchFormat : "json"}
                      disabled={batchKind !== "buckets"}
                      onChange={(e) => {
                        setBatchFormat(e.target.value as typeof batchFormat);
                        clearFeedback();
                      }}
                    >
                      <MenuItem value="json">{tr("JSON 数组")}</MenuItem>
                      <MenuItem value="csv">{tr("逗号分隔文本")}</MenuItem>
                    </TextField>
                  </Box>
                  {batchKind === "tasks" && (
                    <Alert severity="info">
                      {tr(
                        "用于导入已有的 OSS 子任务或恢复断点。支持 startKey、endKey、marker 和 workerNode；留空 workerNode 时由集群分配。",
                      )}
                    </Alert>
                  )}
                  {batchKind === "buckets" && batchFormat === "csv" && (
                    <Alert severity="info">
                      {tr(
                        "每行一个任务：源连接/源桶,目标连接/目标桶,修改时间晚于,是否覆盖,分发模式。后三项可留空。前缀或修改时间早于筛选请使用 JSON。目标命名规则和任务带宽上限请使用 JSON 配置。",
                      )}
                    </Alert>
                  )}
                  <TextField
                    multiline
                    minRows={12}
                    maxRows={24}
                    fullWidth
                    label={tr("任务数据")}
                    value={batchText}
                    onChange={(e) => {
                      setBatchText(e.target.value);
                      clearFeedback();
                    }}
                    placeholder={examples()}
                    slotProps={{
                      input: {
                        sx: {
                          fontFamily: "monospace",
                          fontSize: 13,
                          lineHeight: 1.8,
                        },
                      },
                    }}
                  />
                  <Stack direction={{ xs: "column", sm: "row" }} spacing={1}>
                    <Button
                      variant="outlined"
                      component="label"
                      startIcon={<CloudUploadOutlined />}
                    >
                      {tr("读取数据文件")}
                      <input
                        hidden
                        type="file"
                        accept=".json,.csv,.txt,application/json,text/plain,text/csv"
                        onChange={async (e) => {
                          const file = e.target.files?.[0];
                          if (file) {
                            try {
                              setBatchText(await file.text());
                              clearFeedback();
                            } catch (err) {
                              notify(errorText(err), "error");
                            }
                          }
                          e.target.value = "";
                        }}
                      />
                    </Button>
                    <Button
                      onClick={() => {
                        setBatchText(examples());
                        clearFeedback();
                      }}
                    >
                      {tr("填入示例")}
                    </Button>
                  </Stack>
                  <Accordion
                    disableGutters
                    elevation={0}
                    sx={{
                      bgcolor: "transparent",
                      "&:before": { display: "none" },
                    }}
                  >
                    <AccordionSummary expandIcon={<ExpandMoreRounded />}>
                      <Typography variant="body2" fontWeight={600}>
                        {tr("字段说明")}
                      </Typography>
                    </AccordionSummary>
                    <AccordionDetails>
                      <Typography
                        variant="body2"
                        color="text.secondary"
                        sx={{ lineHeight: 1.9 }}
                      >
                        {batchKind === "local"
                          ? tr(
                              "direction：OSS_TO_LOCAL 或 LOCAL_TO_OSS；sourceEndpoint / destinationEndpoint：OSS 端提供 type、s3Alias、bucket 和可选 objectKey / prefix；LOCAL 端提供 type、nodeBindings，多节点绑定必须访问同一份共享数据。LOCAL_TO_OSS 按节点数自动分发，单文件仅支持一个节点。可选 conflictPolicy、dateAfter、dateBefore、targetRelativePath；OSS_TO_LOCAL 可选 distributionMode。taskBandwidthLimitMbps：任务带宽上限，单位 Mbps，0.1–1,000,000；省略或 null 表示仅使用节点限速。",
                            )
                          : tr(
                              "sourceS3Alias / destS3Alias：连接名称；sourceBucket / destBucket：存储桶；prefix：源前缀筛选，objectKey：单个源对象，二者互斥。{value1}keyMapping 省略或 null 保留完整源 Key；mode 可选 PRESERVE、ADD_PREFIX（targetPrefix）、REPLACE_PREFIX（sourcePrefix 与源筛选前缀一致，targetPrefix 可为空）、EXACT_KEY（仅单对象，targetKey）。ADD_PREFIX 末尾缺少 / 时补一个；REPLACE_PREFIX 字面替换且不补 /；所有 Key 和前缀均保留空格与斜杠。dateAfter / dateBefore：修改时间范围，格式 YYYY-MM-DDTHH:mm:ss；overwrite：是否覆盖；distributionMode：affinity 或 balanced。taskBandwidthLimitMbps：任务带宽上限，单位 Mbps，0.1–1,000,000；省略或 null 表示仅使用节点限速。",
                              {
                                value1:
                                  batchKind === "tasks"
                                    ? tr(
                                        "子任务也支持 sourceEndpoint.objectKey；使用 sourceEndpoint.prefix 时须设置 sourceEndpoint.prefixFilterEnabled:true，与顶层字段重复时值必须一致。可选 startKey、endKey、marker、workerNode。",
                                      )
                                    : tr("存储桶迁移会按源范围生成子任务。"),
                              },
                            )}
                      </Typography>
                    </AccordionDetails>
                  </Accordion>
                </Stack>
              </Paper>
            )}

            {precheck && (
              <Paper variant="outlined" sx={{ overflow: "hidden" }}>
                <Alert
                  severity={precheck.valid ? "success" : "warning"}
                  sx={{ borderRadius: 0 }}
                >
                  <AlertTitle>
                    {precheck.valid ? tr("节点预检通过") : tr("节点预检未通过")}
                  </AlertTitle>
                  {tr(
                    "通过 {passed} / {total} 个节点配置。修改配置后需重新预检。",
                    {
                      passed: precheck.nodeBindings
                        .filter((binding) => binding.valid)
                        .length.toLocaleString(getLocale()),
                      total:
                        precheck.nodeBindings.length.toLocaleString(
                          getLocale(),
                        ),
                    },
                  )}
                </Alert>
                <Stack divider={<Divider />} sx={{ px: 2.5 }}>
                  {precheck.nodeBindings.map((binding, index) => (
                    <Stack
                      key={index}
                      direction={{ xs: "column", sm: "row" }}
                      justifyContent="space-between"
                      spacing={1}
                      sx={{ py: 2 }}
                    >
                      <Box>
                        <Typography variant="body2" fontWeight={600}>
                          {nodeLabel(binding.executionNodeIp)}
                        </Typography>
                        <Typography
                          variant="caption"
                          color="text.secondary"
                          sx={{ overflowWrap: "anywhere" }}
                        >
                          {binding.localPath}
                        </Typography>
                        {(!binding.valid || binding.pathType === "MISSING") && (
                          <Typography
                            variant="body2"
                            color={
                              binding.valid ? "text.secondary" : "error.main"
                            }
                          >
                            {binding.message ||
                              binding.code ||
                              tr(binding.valid ? "将创建目录" : "预检失败")}
                          </Typography>
                        )}
                      </Box>
                      <Chip
                        size="small"
                        color={binding.valid ? "success" : "error"}
                        label={
                          binding.valid
                            ? tr("通过{value1}", {
                                value1:
                                  binding.pathType === "FILE"
                                    ? ` · ${tr("文件")}`
                                    : binding.pathType === "DIRECTORY"
                                      ? ` · ${tr("目录")}`
                                      : binding.pathType === "MISSING"
                                        ? ` · ${tr("将创建目录")}`
                                        : "",
                              })
                            : tr("未通过")
                        }
                        sx={{ alignSelf: "flex-start" }}
                      />
                    </Stack>
                  ))}
                </Stack>
              </Paper>
            )}
            {submission && (
              <Alert
                severity={submission.severity}
                action={
                  submission.severity !== "error" ||
                  submission.taskIds?.length ? (
                    <Button
                      component={Link}
                      to={
                        submission.taskIds?.length === 1
                          ? `/tasks?taskId=${submission.taskIds[0]}`
                          : "/tasks"
                      }
                      color="inherit"
                      size="small"
                    >
                      {tr("查看任务")}
                    </Button>
                  ) : undefined
                }
              >
                <AlertTitle>{submission.title}</AlertTitle>
                {submission.message}
                {(submission.taskIds?.length || 0) > 1 && (
                  <Stack direction="row" gap={1} flexWrap="wrap" sx={{ mt: 1 }}>
                    {submission.taskIds!.map((taskId) => (
                      <Button
                        component={Link}
                        to={`/tasks?taskId=${taskId}`}
                        color="inherit"
                        size="small"
                        key={taskId}
                      >
                        {tr("查看任务 #")}
                        {taskId}
                      </Button>
                    ))}
                  </Stack>
                )}
                {submission.details && submission.details.length > 0 && (
                  <Box component="ul" sx={{ pl: 2, mb: 0 }}>
                    {submission.details.map((detail, index) => (
                      <li key={index}>{detail}</li>
                    ))}
                  </Box>
                )}
              </Alert>
            )}
            <Paper variant="outlined" sx={{ p: 2.5 }}>
              <Stack
                direction={{ xs: "column", md: "row" }}
                spacing={2}
                alignItems={{ md: "center" }}
                justifyContent="space-between"
              >
                <Box>
                  <Typography variant="subtitle2">
                    {tab === 0
                      ? directions.find(
                          (direction) => direction.value === form.direction,
                        )?.title
                      : tr("批量创建迁移任务")}
                  </Typography>
                  <Typography variant="caption" color="text.secondary">
                    {tab === 0
                      ? `${form.conflictPolicy === "SKIP" ? tr("跳过同名对象或文件") : tr("覆盖同名对象或文件")} · ${distributionLabel(effectiveDistributionMode, form.direction)}`
                      : tr("提交前请核对存储连接和迁移路径。")}
                    {tab === 0 &&
                    form.taskBandwidthLimitMbps.trim() &&
                    Number.isFinite(Number(form.taskBandwidthLimitMbps)) &&
                    Number(form.taskBandwidthLimitMbps) >= 0.1 &&
                    Number(form.taskBandwidthLimitMbps) <= 1_000_000
                      ? tr(" · 上限 {value1}", {
                          value1: formatTaskBandwidth(
                            Number(form.taskBandwidthLimitMbps),
                          ),
                        })
                      : ""}
                  </Typography>
                </Box>
                <Stack direction={{ xs: "column", sm: "row" }} spacing={1.5}>
                  <Button disabled={Boolean(busy)} onClick={resetDraft}>
                    {tr("重置草稿")}
                  </Button>
                  {requiresPrecheck && (
                    <Tooltip
                      title={tr(
                        "检查执行节点和本地路径；多节点会验证共享目录。",
                      )}
                    >
                      <span>
                        <Button
                          fullWidth
                          variant="outlined"
                          startIcon={
                            busy === "validate" ? (
                              <CircularProgress size={17} />
                            ) : (
                              <CheckCircleOutlineRounded />
                            )
                          }
                          disabled={Boolean(busy) || submitted}
                          onClick={() => void submit(true)}
                        >
                          {busy === "validate"
                            ? tr("正在预检")
                            : tr("运行预检")}
                        </Button>
                      </span>
                    </Tooltip>
                  )}
                  {submitted ? (
                    <Button
                      key="continue-configuration"
                      type="button"
                      variant="outlined"
                      onClick={(event) => {
                        event.preventDefault();
                        clearFeedback();
                      }}
                    >
                      {tr("继续配置任务")}
                    </Button>
                  ) : (
                    <Button
                      key="submit-migration"
                      type="submit"
                      variant="contained"
                      disabled={
                        Boolean(busy) || (requiresPrecheck && !precheck?.valid)
                      }
                      startIcon={
                        busy === "create" ? (
                          <CircularProgress size={17} color="inherit" />
                        ) : (
                          <RocketLaunchOutlined />
                        )
                      }
                    >
                      {busy === "create"
                        ? tr("正在提交")
                        : tab === 0
                          ? tr("创建迁移任务")
                          : tr("导入任务")}
                    </Button>
                  )}
                </Stack>
              </Stack>
              {requiresPrecheck && (
                <Typography
                  variant="caption"
                  color="text.secondary"
                  sx={{ display: "block", mt: 1.5 }}
                >
                  {tr(
                    precheck?.valid
                      ? "多节点预检会读写临时文件，验证共享目录。"
                      : "请先通过预检，再创建任务。",
                  )}
                </Typography>
              )}
              <Typography
                variant="caption"
                color="text.secondary"
                sx={{ display: "block", mt: 1 }}
              >
                {tr("未提交的配置自动保存在当前标签页，切换页面后可继续填写。")}
              </Typography>
            </Paper>
          </Stack>
        </Box>
      </Box>
    </Stack>
  );
}
