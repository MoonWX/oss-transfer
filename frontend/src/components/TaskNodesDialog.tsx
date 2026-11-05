import { useCallback, useEffect, useMemo, useRef, useState } from "react";
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
  FormControlLabel,
  Paper,
  Radio,
  RadioGroup,
  Stack,
  TextField,
  Typography,
} from "@mui/material";
import { api, ApiError, useNotify } from "../lib/api";
import { t, useI18n } from "../lib/i18n";
import { useNodeLabels } from "../lib/workerStatus";
import type { NodeDisplay, NodeLabels } from "../lib/nodeRegistry";
import { formatBytes, formatNumber } from "./Common";

export type NodeShare = {
  bandwidthBytesPerSecond?: number;
  listQps?: number;
  headQps?: number;
  putQps?: number;
} | null;
export type MemberState =
  "ACTIVE" | "JOINING" | "DRAINING" | "REMOVING" | "DISCONNECTED";
export type MemberNode = NodeDisplay & {
  id?: string;
  address: string;
  state: MemberState | string;
  drainMode?: string;
  scanner?: boolean;
  online?: boolean;
  localPath?: string;
  pendingChildren?: number;
  share?: NodeShare;
};
type Candidate = NodeDisplay & {
  id?: string;
  address: string;
  eligible: boolean;
  reason?: string;
};
type NodesView = {
  taskId: number;
  generation: number;
  nodeRevision: number;
  status: string;
  direction: string;
  localTransfer: boolean;
  editable: boolean;
  reason?: string;
  nowSupported: boolean;
  nowReason?: string;
  pending: boolean;
  bandwidthLimitMbps?: number | null;
  nodes: MemberNode[];
  removedNodes?: (NodeDisplay & { id?: string; address: string })[];
  candidates: Candidate[];
};
type BindingCheck = NodeDisplay & {
  executionNodeIp: string;
  localPath?: string;
  valid: boolean;
  pathType?: string;
  code?: string;
  message?: string;
};
type ChangeResult = {
  accepted: boolean;
  dryRun: boolean;
  replayed?: boolean;
  valid: boolean;
  nodeBindings?: BindingCheck[];
  validationErrors?: string[];
  view?: NodesView;
};
type Mode = "drain" | "now";
type Preflight = {
  fingerprint: string;
  valid: boolean;
  bindings: BindingCheck[];
  errors: string[];
};

function readView(value: unknown): NodesView {
  const view = value as NodesView | undefined;
  if (
    !view ||
    typeof view.editable !== "boolean" ||
    !Array.isArray(view.nodes) ||
    !Array.isArray(view.candidates)
  )
    throw new Error(t("执行节点信息无效，请重试。"));
  return view;
}

function isAbsoluteAgentPath(value: string) {
  if (value.startsWith("/")) return true;
  return (
    /^[A-Za-z]:[\\/]/.test(value) || /^\\\\[^\\]+\\[^\\]+(?:\\|$)/.test(value)
  );
}

export function memberStateLabel(node: { state?: string; drainMode?: string }) {
  switch (node.state) {
    case "JOINING":
      return t("加入中");
    case "DRAINING":
      return t("排空中");
    case "REMOVING":
      return t("立即移除中");
    case "DISCONNECTED":
      return t("已失联");
    default:
      return "";
  }
}

const stateColor = (state?: string) =>
  state === "JOINING"
    ? "info"
    : state === "DRAINING" || state === "REMOVING"
      ? "warning"
      : state === "DISCONNECTED"
        ? "error"
        : "default";

/** Effective per-node bandwidth share; 0 means unlimited. */
export function shareLabel(share: NodeShare | undefined) {
  if (!share || share.bandwidthBytesPerSecond == null) return "";
  return share.bandwidthBytesPerSecond > 0
    ? `${formatBytes(share.bandwidthBytesPerSecond)}/s`
    : t("不限");
}

/** Execution nodes of a task row with their membership states. */
export function TaskMembershipNodes({
  nodes,
  nodeLabel,
}: {
  nodes: MemberNode[];
  nodeLabel: NodeLabels;
}) {
  useI18n();
  return (
    <Stack gap={0.5} data-testid="task-membership-nodes">
      {nodes.map((node) => {
        const state = memberStateLabel(node);
        const share = shareLabel(node.share);
        return (
          <Stack
            key={node.id || node.address}
            direction="row"
            gap={0.75}
            alignItems="center"
            flexWrap="wrap"
          >
            <Typography variant="body2" sx={{ overflowWrap: "anywhere" }}>
              {nodeLabel(node.address || node.id, node)}
            </Typography>
            {state && (
              <Chip
                size="small"
                variant="outlined"
                color={stateColor(node.state)}
                label={state}
              />
            )}
            {node.scanner && (
              <Typography variant="caption" color="text.secondary">
                {t("扫描")}
              </Typography>
            )}
            {share && (
              <Typography variant="caption" color="text.secondary">
                {t("带宽份额 {value}", { value: share })}
              </Typography>
            )}
          </Stack>
        );
      })}
    </Stack>
  );
}

export default function TaskNodesDialog({
  taskId,
  open,
  refreshMs,
  onClose,
  onChanged,
}: {
  taskId: number;
  open: boolean;
  refreshMs: number;
  onClose: () => void;
  onChanged?: () => void;
}) {
  useI18n();
  const nodeLabel = useNodeLabels();
  const notify = useNotify();
  const path = `/api/master/tasks/${taskId}/nodes`;
  const [view, setView] = useState<NodesView>();
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState("");
  const [adds, setAdds] = useState<string[]>([]);
  const [removes, setRemoves] = useState<string[]>([]);
  const [mode, setMode] = useState<Mode>("drain");
  const [paths, setPaths] = useState<Record<string, string>>({});
  const [preflight, setPreflight] = useState<Preflight>();
  const [checking, setChecking] = useState(false);
  const [step, setStep] = useState<"edit" | "confirm">("edit");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const sequence = useRef(0);
  const controller = useRef<AbortController | null>(null);
  const reading = useRef(false);
  const pendingRequest = useRef<{ id: string; fingerprint: string }>(undefined);

  const load = useCallback(async () => {
    const current = ++sequence.current;
    controller.current?.abort();
    const next = new AbortController();
    controller.current = next;
    reading.current = true;
    setLoading(true);
    try {
      const response = await api<{ data?: NodesView }>(path, {
        signal: next.signal,
      });
      if (current === sequence.current) {
        setView(readView(response.data));
        setLoadError("");
      }
    } catch (failure) {
      if (
        current === sequence.current &&
        !(failure instanceof DOMException && failure.name === "AbortError")
      )
        setLoadError(
          failure instanceof Error ? failure.message : t("读取失败，请重试。"),
        );
    } finally {
      if (current === sequence.current) {
        reading.current = false;
        setLoading(false);
      }
    }
  }, [path]);

  useEffect(() => {
    if (!open) return;
    setView(undefined);
    setLoadError("");
    setAdds([]);
    setRemoves([]);
    setMode("drain");
    setPaths({});
    setPreflight(undefined);
    setStep("edit");
    setError("");
    pendingRequest.current = undefined;
    void load();
    return () => {
      ++sequence.current;
      controller.current?.abort();
      reading.current = false;
    };
  }, [open, load]);

  // Follow joining and draining progress while the dialog is open. The
  // confirmation step keeps the view it summarises.
  const pending = !!view?.pending;
  useEffect(() => {
    if (!open || !pending || step !== "edit" || submitting || refreshMs <= 0)
      return;
    const timer = window.setInterval(() => {
      if (!document.hidden && !reading.current) void load();
    }, refreshMs);
    return () => window.clearInterval(timer);
  }, [open, pending, step, submitting, refreshMs, load]);

  const nodes = view?.nodes || [];
  const byAddress = useMemo(
    () => new Map(nodes.map((node) => [node.address, node])),
    [nodes],
  );
  const candidateByAddress = useMemo(
    () => new Map((view?.candidates || []).map((node) => [node.address, node])),
    [view?.candidates],
  );
  // Selections that the latest view no longer offers are dropped.
  const validAdds = adds.filter(
    (address) =>
      candidateByAddress.get(address)?.eligible ||
      byAddress.get(address)?.state === "DRAINING" ||
      byAddress.get(address)?.state === "REMOVING",
  );
  // Re-adding a leaving member cancels its removal and keeps its binding;
  // only Agents that are not members yet join and need a path.
  const joinAdds = validAdds.filter((address) => !byAddress.has(address));
  const cancels = validAdds.length - joinAdds.length;
  const validRemoves = removes.filter((address) => {
    const state = byAddress.get(address)?.state;
    return !!state && state !== "REMOVING";
  });
  const editable = !!view?.editable;
  const nowDisabled = !view?.nowSupported;
  const effectiveMode: Mode = nowDisabled ? "drain" : mode;
  const local = !!view?.localTransfer;
  const bindings = local
    ? joinAdds.map((address) => ({
        executionNodeIp: address,
        localPath: (paths[address] ?? "").trim(),
      }))
    : [];
  const change = {
    add: validAdds,
    remove: validRemoves,
    ...(local && joinAdds.length ? { bindings } : {}),
    ...(validRemoves.length ? { mode: effectiveMode } : {}),
    expectedGeneration: view?.generation,
    expectedNodeRevision: view?.nodeRevision,
  };
  const fingerprint = JSON.stringify(change);
  const remaining = nodes.filter(
    (node) =>
      node.state !== "REMOVING" &&
      node.state !== "DRAINING" &&
      !validRemoves.includes(node.address),
  ).length;
  const empty = validAdds.length === 0 && validRemoves.length === 0;
  // Joining Agents do not count: the task needs a working member until they join.
  const lastNode = remaining + cancels === 0;
  const pathMissing = bindings.some(
    (binding) => !isAbsoluteAgentPath(binding.localPath),
  );
  const needsPreflight = local && joinAdds.length > 0;
  const preflightPassed =
    !needsPreflight ||
    (preflight?.fingerprint === fingerprint && preflight.valid);

  const toggle = (
    list: string[],
    setList: (next: string[]) => void,
    address: string,
  ) => {
    setList(
      list.includes(address)
        ? list.filter((item) => item !== address)
        : [...list, address],
    );
    setError("");
  };

  async function runPreflight() {
    if (!view || checking || !needsPreflight || pathMissing) return;
    setChecking(true);
    setError("");
    const checked = fingerprint;
    try {
      const response = await api<{ data: ChangeResult }>(path, {
        method: "POST",
        body: { ...change, dryRun: true },
      });
      const result = response.data;
      setPreflight({
        fingerprint: checked,
        valid: result.valid === true,
        bindings: result.nodeBindings || [],
        errors: result.validationErrors || [],
      });
    } catch (failure) {
      const response = failure instanceof ApiError ? failure.response : null;
      setPreflight({
        fingerprint: checked,
        valid: false,
        bindings: Array.isArray(response?.nodeBindings)
          ? response.nodeBindings
          : [],
        errors: [],
      });
      setError(
        failure instanceof Error ? failure.message : t("预检失败，请重试。"),
      );
    } finally {
      setChecking(false);
    }
  }

  async function submit() {
    if (!view || submitting || empty || lastNode || !preflightPassed) return;
    if (pendingRequest.current?.fingerprint !== fingerprint)
      pendingRequest.current = {
        id: Array.from(crypto.getRandomValues(new Uint8Array(16)), (byte) =>
          byte.toString(16).padStart(2, "0"),
        ).join(""),
        fingerprint,
      };
    // A retry of the same change reuses the request ID so a lost response
    // cannot apply the change twice.
    const body = {
      requestId: pendingRequest.current.id,
      ...change,
    };
    setSubmitting(true);
    setError("");
    try {
      const response = await api<{ data: ChangeResult }>(path, {
        method: "POST",
        body,
      });
      pendingRequest.current = undefined;
      const next = response.data.view;
      notify(t("执行节点变更已提交。"));
      onChanged?.();
      setAdds([]);
      setRemoves([]);
      setPaths({});
      setPreflight(undefined);
      setMode("drain");
      setStep("edit");
      if (next) {
        try {
          setView(readView(next));
        } catch {
          void load();
        }
      } else void load();
      if (next && !next.pending) onClose();
    } catch (failure) {
      const response = failure instanceof ApiError ? failure.response : null;
      if (failure instanceof ApiError && failure.status === 409)
        pendingRequest.current = undefined;
      if (Array.isArray(response?.nodeBindings))
        setPreflight({
          fingerprint,
          valid: false,
          bindings: response.nodeBindings,
          errors: [],
        });
      setError(
        failure instanceof Error ? failure.message : t("提交失败，请重试。"),
      );
      setStep("edit");
      if (failure instanceof ApiError && failure.status === 409) void load();
    } finally {
      setSubmitting(false);
    }
  }

  const label = (address: string, display?: NodeDisplay) =>
    nodeLabel(address, display);
  const summaryLabel = (address: string) =>
    label(address, byAddress.get(address) || candidateByAddress.get(address));

  const nodeRow = (node: MemberNode) => {
    const state = memberStateLabel(node);
    const share = shareLabel(node.share);
    const name = label(node.address || node.id || "", node);
    const draining = node.state === "DRAINING" || node.state === "REMOVING";
    return (
      <Paper
        key={node.id || node.address}
        variant="outlined"
        sx={{ px: 1.5, py: 1 }}
        data-testid={`task-node-${node.address}`}
      >
        <Stack direction="row" gap={1} alignItems="center" flexWrap="wrap">
          <Typography variant="body2" sx={{ overflowWrap: "anywhere" }}>
            {name}
          </Typography>
          {state && (
            <Chip
              size="small"
              variant="outlined"
              color={stateColor(node.state)}
              label={state}
            />
          )}
          {node.scanner && (
            <Chip size="small" variant="outlined" label={t("扫描节点")} />
          )}
          {node.online === false && node.state !== "DISCONNECTED" && (
            <Chip size="small" color="error" label={t("离线")} />
          )}
        </Stack>
        <Stack
          direction="row"
          gap={1.5}
          flexWrap="wrap"
          sx={{ mt: 0.5, color: "text.secondary" }}
        >
          {share && (
            <Typography variant="caption">
              {t("带宽份额 {value}", { value: share })}
            </Typography>
          )}
          {node.pendingChildren != null && (
            <Typography variant="caption">
              {draining || node.state === "REMOVING"
                ? t("剩余 {count} 个子任务待移交或完成", {
                    count: formatNumber(node.pendingChildren),
                  })
                : t("未完成子任务 {count}", {
                    count: formatNumber(node.pendingChildren),
                  })}
            </Typography>
          )}
          {node.state === "JOINING" && (
            <Typography variant="caption">
              {t("正在完成握手与检查，加入后开始分配任务")}
            </Typography>
          )}
          {node.localPath && (
            <Typography variant="caption" sx={{ overflowWrap: "anywhere" }}>
              {node.localPath}
            </Typography>
          )}
        </Stack>
        {(node.state !== "REMOVING" || draining) && (
          <Stack direction="row" gap={2} flexWrap="wrap">
            {node.state !== "REMOVING" && (
              <FormControlLabel
                control={
                  <Checkbox
                    size="small"
                    checked={validRemoves.includes(node.address)}
                    disabled={!editable || submitting}
                    onChange={() => toggle(removes, setRemoves, node.address)}
                  />
                }
                label={
                  <Typography variant="body2">
                    {t("移除 {node}", { node: name })}
                  </Typography>
                }
              />
            )}
            {draining && (
              <FormControlLabel
                control={
                  <Checkbox
                    size="small"
                    checked={validAdds.includes(node.address)}
                    disabled={!editable || submitting}
                    onChange={() => {
                      toggle(adds, setAdds, node.address);
                    }}
                  />
                }
                label={
                  <Typography variant="body2">
                    {node.state === "REMOVING"
                      ? t("取消立即移除，保留 {node}", { node: name })
                      : t("取消排空，保留 {node}", { node: name })}
                  </Typography>
                }
              />
            )}
          </Stack>
        )}
        {local && joinAdds.includes(node.address) && bindingField(node)}
      </Paper>
    );
  };

  const bindingField = (node: { address: string } & NodeDisplay) => {
    const value = paths[node.address] ?? "";
    const check = preflight?.bindings.find(
      (binding) => binding.executionNodeIp === node.address,
    );
    const invalid = value.trim() !== "" && !isAbsoluteAgentPath(value.trim());
    return (
      <Box sx={{ mt: 1 }}>
        <TextField
          size="small"
          fullWidth
          label={t("{node} 的本地绝对路径", {
            node: label(node.address, node),
          })}
          value={value}
          disabled={!editable || submitting}
          error={invalid || (check ? !check.valid : false)}
          helperText={
            invalid
              ? t("请输入执行节点上的绝对路径。")
              : check
                ? check.valid
                  ? t("预检通过")
                  : check.message || t("预检未通过")
                : t("与任务其他节点指向同一共享目录")
          }
          onChange={(event) => {
            setPaths({ ...paths, [node.address]: event.target.value });
            setError("");
          }}
        />
      </Box>
    );
  };

  const candidates = view?.candidates || [];
  const canConfirm =
    editable &&
    !empty &&
    !lastNode &&
    !pathMissing &&
    preflightPassed &&
    !submitting &&
    !checking;

  return (
    <Dialog
      open={open}
      onClose={() => {
        if (!submitting) onClose();
      }}
      maxWidth="sm"
      fullWidth
      aria-labelledby="task-nodes-title"
    >
      <DialogTitle id="task-nodes-title">
        {t("任务 #{id} 执行节点", { id: taskId })}
      </DialogTitle>
      <DialogContent>
        {!view && loading && (
          <Stack direction="row" gap={1} alignItems="center" role="status">
            <CircularProgress size={16} />
            <Typography variant="body2" color="text.secondary">
              {t("读取执行节点…")}
            </Typography>
          </Stack>
        )}
        {loadError && (
          <Alert
            severity="warning"
            sx={{ mb: 2 }}
            action={
              <Button
                color="inherit"
                size="small"
                disabled={loading}
                onClick={() => void load()}
              >
                {t("重试")}
              </Button>
            }
          >
            {t("无法读取执行节点：{error}", { error: loadError })}
          </Alert>
        )}
        {view && step === "edit" && (
          <Stack gap={2}>
            {!view.editable && (
              <Alert severity="info">
                {view.reason || t("当前任务不可调整执行节点")}
              </Alert>
            )}
            {view.editable && view.pending && (
              <Alert severity="info" role="status">
                {t(
                  "上一次节点变更仍在进行中。此时只能取消加入中的节点、将排空中的节点改为立即移除，或保留排空中的节点。",
                )}
              </Alert>
            )}
            <Box>
              <Typography variant="subtitle2" sx={{ mb: 1 }}>
                {t("当前节点")}
              </Typography>
              <Stack gap={1}>{nodes.map(nodeRow)}</Stack>
            </Box>
            <Box>
              <Typography variant="subtitle2" sx={{ mb: 1 }}>
                {t("添加节点")}
              </Typography>
              {candidates.length === 0 ? (
                <Typography variant="body2" color="text.secondary">
                  {t("没有可添加的在线 Agent")}
                </Typography>
              ) : (
                <Stack gap={1}>
                  {candidates.map((candidate) => (
                    <Paper
                      key={candidate.id || candidate.address}
                      variant="outlined"
                      sx={{ px: 1.5, py: 0.5 }}
                    >
                      <FormControlLabel
                        control={
                          <Checkbox
                            size="small"
                            checked={validAdds.includes(candidate.address)}
                            disabled={
                              !editable || !candidate.eligible || submitting
                            }
                            onChange={() =>
                              toggle(adds, setAdds, candidate.address)
                            }
                          />
                        }
                        label={
                          <Box>
                            <Typography variant="body2">
                              {t("添加 {node}", {
                                node: label(candidate.address, candidate),
                              })}
                            </Typography>
                            {!candidate.eligible && (
                              <Typography
                                variant="caption"
                                color="text.secondary"
                              >
                                {candidate.reason || t("暂不可添加")}
                              </Typography>
                            )}
                          </Box>
                        }
                      />
                      {local &&
                        validAdds.includes(candidate.address) &&
                        bindingField(candidate)}
                    </Paper>
                  ))}
                </Stack>
              )}
            </Box>
            {validRemoves.length > 0 && (
              <Box>
                <Typography variant="subtitle2" id="task-nodes-mode">
                  {t("移除方式")}
                </Typography>
                <RadioGroup
                  aria-labelledby="task-nodes-mode"
                  value={effectiveMode}
                  onChange={(event) => {
                    setMode(event.target.value as Mode);
                    setError("");
                  }}
                >
                  <FormControlLabel
                    value="drain"
                    disabled={submitting}
                    control={<Radio size="small" />}
                    label={
                      <Box>
                        <Typography variant="body2">
                          {t("排空后移除（推荐）")}
                        </Typography>
                        <Typography variant="caption" color="text.secondary">
                          {t(
                            "不再分配新任务，排队中的任务移交其他节点，进行中的任务完成后节点退出。",
                          )}
                        </Typography>
                      </Box>
                    }
                  />
                  <FormControlLabel
                    value="now"
                    disabled={submitting || nowDisabled}
                    control={<Radio size="small" />}
                    label={
                      <Box>
                        <Typography variant="body2">{t("立即移除")}</Typography>
                        <Typography variant="caption" color="text.secondary">
                          {nowDisabled
                            ? view.nowReason || t("当前任务不支持立即移除")
                            : t(
                                "进行中的任务会被停止，并由剩余节点从已保存的进度继续。",
                              )}
                        </Typography>
                      </Box>
                    }
                  />
                </RadioGroup>
              </Box>
            )}
            {lastNode && !empty && (
              <Alert severity="warning">{t("至少保留一个执行节点")}</Alert>
            )}
            {needsPreflight && (
              <Box>
                <Button
                  variant="outlined"
                  size="small"
                  disabled={!editable || pathMissing || checking || submitting}
                  onClick={() => void runPreflight()}
                  startIcon={
                    checking ? (
                      <CircularProgress size={16} color="inherit" />
                    ) : undefined
                  }
                >
                  {t("预检")}
                </Button>
                {preflight?.fingerprint === fingerprint && (
                  <Alert
                    severity={preflight.valid ? "success" : "error"}
                    sx={{ mt: 1 }}
                  >
                    <Typography variant="body2">
                      {preflight.valid ? t("预检通过") : t("预检未通过")}
                    </Typography>
                    {preflight.errors.map((message) => (
                      <Typography key={message} variant="body2">
                        {message}
                      </Typography>
                    ))}
                    {preflight.bindings.map((binding) => (
                      <Typography
                        key={binding.executionNodeIp}
                        variant="body2"
                        sx={{ overflowWrap: "anywhere" }}
                      >
                        {label(binding.executionNodeIp, binding)}：
                        {binding.valid
                          ? t("通过")
                          : binding.message || t("未通过")}
                      </Typography>
                    ))}
                  </Alert>
                )}
                {!preflightPassed && preflight?.fingerprint !== fingerprint && (
                  <Typography
                    variant="caption"
                    color="text.secondary"
                    sx={{ display: "block", mt: 0.5 }}
                  >
                    {t("本地传输需要预检通过后才能提交")}
                  </Typography>
                )}
              </Box>
            )}
          </Stack>
        )}
        {view && step === "confirm" && (
          <Stack gap={1.5} data-testid="task-nodes-summary">
            <Typography variant="body2">
              {t("请确认以下执行节点变更：")}
            </Typography>
            {validAdds.length > 0 && (
              <Typography variant="body2" sx={{ overflowWrap: "anywhere" }}>
                {t("添加：{nodes}", {
                  nodes: validAdds
                    .map((address) =>
                      local && joinAdds.includes(address)
                        ? `${summaryLabel(address)} (${(paths[address] ?? "").trim()})`
                        : summaryLabel(address),
                    )
                    .join(", "),
                })}
              </Typography>
            )}
            {validRemoves.length > 0 && (
              <>
                <Typography variant="body2" sx={{ overflowWrap: "anywhere" }}>
                  {t("移除：{nodes}", {
                    nodes: validRemoves.map(summaryLabel).join(", "),
                  })}
                </Typography>
                <Typography variant="body2">
                  {effectiveMode === "now"
                    ? t("移除方式：立即移除")
                    : t("移除方式：排空后移除")}
                </Typography>
              </>
            )}
            <Typography variant="caption" color="text.secondary">
              {view.status === "RUNNING"
                ? t("变更将立即开始生效。")
                : t("变更将在任务继续运行时生效。")}
            </Typography>
          </Stack>
        )}
        {error && (
          <Alert severity="error" sx={{ mt: 2 }}>
            {error}
          </Alert>
        )}
      </DialogContent>
      <DialogActions>
        {step === "confirm" ? (
          <>
            <Button disabled={submitting} onClick={() => setStep("edit")}>
              {t("返回修改")}
            </Button>
            <Button
              variant="contained"
              disabled={!canConfirm}
              onClick={() => void submit()}
              startIcon={
                submitting ? (
                  <CircularProgress size={16} color="inherit" />
                ) : undefined
              }
            >
              {submitting ? t("正在提交") : t("确认变更")}
            </Button>
          </>
        ) : (
          <>
            <Button disabled={submitting} onClick={onClose}>
              {t("关闭")}
            </Button>
            <Button
              variant="contained"
              disabled={!canConfirm}
              onClick={() => {
                setError("");
                setStep("confirm");
              }}
            >
              {t("下一步")}
            </Button>
          </>
        )}
      </DialogActions>
    </Dialog>
  );
}
