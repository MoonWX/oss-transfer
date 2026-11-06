import {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
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
  FormControlLabel,
  ListItemText,
  Menu,
  MenuItem,
  Radio,
  RadioGroup,
  Stack,
  TextField,
  Typography,
  useMediaQuery,
} from "@mui/material";
import { alpha, useTheme, type Theme } from "@mui/material/styles";
import { AddRounded, UndoRounded } from "@mui/icons-material";
import { api, ApiError, useNotify } from "../lib/api";
import { t, useI18n } from "../lib/i18n";
import { useNodeLabels } from "../lib/workerStatus";
import type { NodeDisplay, NodeLabels } from "../lib/nodeRegistry";
import { groupedCardRadius, radii } from "../theme";
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
  /**
   * How the task limits are divided for this node: busy nodes share them,
   * idle nodes keep a small floor. Older Masters (equal split) omit it.
   */
  demand?: "busy" | "idle" | string;
  /** The node still holds its previous share until it confirms a reduction. */
  adjusting?: boolean;
  /**
   * An offline leaving node: LEASE_EXPIRY leaves once its lease expires;
   * RECONNECT (no lease fencing) leaves after it reconnects.
   */
  progress?: string;
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
  /** SCANNING, HANDOFF, WAITING or COMPLETE; older Masters omit it. */
  scan?: { phase?: string; complete?: boolean };
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

const leaving = (state?: string) =>
  state === "DRAINING" || state === "REMOVING";

/** Membership state of a node that does not take work normally; "" when active. */
export function memberStateLabel(node: { state?: string; drainMode?: string }) {
  switch (node.state) {
    case "JOINING":
      return t("加入中");
    case "DRAINING":
      return t("平滑移出中");
    case "REMOVING":
      return t("立即移出中");
    case "DISCONNECTED":
      return t("已失联");
    default:
      return "";
  }
}

type StateColor = "info" | "warning" | "error" | "default";
const stateColor = (state?: string): StateColor =>
  state === "JOINING"
    ? "info"
    : leaving(state)
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

/** The node's demand under dynamic allocation; empty for an equal split. */
export function demandLabel(node: Pick<MemberNode, "demand" | "adjusting">) {
  const base =
    node.demand === "busy"
      ? t("按需（忙）")
      : node.demand === "idle"
        ? t("空闲（已释放）")
        : "";
  if (!base) return "";
  return node.adjusting ? `${base} · ${t("调整中：等待节点确认降额")}` : base;
}

/** Request shares that are actually limited; unlimited ones are left out. */
function qpsShareLabel(share: NodeShare | undefined) {
  if (!share) return "";
  const parts = (
    [
      ["LIST", share.listQps],
      ["HEAD", share.headQps],
      ["PUT", share.putQps],
    ] as const
  )
    .filter(([, value]) => value != null && value > 0)
    .map(([name, value]) => `${name} ${formatNumber(value)}`);
  return parts.length ? `${parts.join(" · ")} ${t("次/秒")}` : "";
}

/**
 * Execution nodes in the task detail list: one name per line. Only nodes that
 * do not take work normally carry a short state; shares and roles are in the
 * node dialog.
 */
export function TaskMembershipNodes({
  nodes,
  nodeLabel,
}: {
  nodes: MemberNode[];
  nodeLabel: NodeLabels;
}) {
  useI18n();
  return (
    <Box
      component="ul"
      data-testid="task-membership-nodes"
      sx={{ m: 0, p: 0, listStyle: "none", minWidth: 0 }}
    >
      {nodes.map((node) => {
        const state = leaving(node.state)
          ? t("移出中")
          : memberStateLabel(node);
        const color = stateColor(node.state);
        return (
          <Typography
            key={node.id || node.address}
            component="li"
            variant="body2"
            sx={{ overflowWrap: "anywhere" }}
          >
            {nodeLabel(node.address || node.id, node)}
            {state && (
              <Typography
                component="span"
                variant="caption"
                sx={{
                  ml: 1,
                  color:
                    color === "default" ? "text.secondary" : `${color}.main`,
                  whiteSpace: "nowrap",
                }}
              >
                {state}
              </Typography>
            )}
          </Typography>
        );
      })}
    </Box>
  );
}

/** A labelled value in a node row; the label column keeps rows aligned. */
function Field({
  label,
  children,
  grow,
}: {
  label: string;
  children: ReactNode;
  grow?: boolean;
}) {
  return (
    <Box
      sx={{
        minWidth: 0,
        flex: grow ? "1 1 12rem" : "0 0 auto",
        width: grow ? undefined : { sm: "8.5rem" },
      }}
    >
      <Typography
        variant="caption"
        color="text.secondary"
        component="div"
        sx={{ lineHeight: 1.5 }}
      >
        {label}
      </Typography>
      <Typography
        variant="body2"
        component="div"
        sx={{
          lineHeight: 1.5,
          overflowWrap: "anywhere",
          fontVariantNumeric: "tabular-nums",
        }}
      >
        {children}
      </Typography>
    </Box>
  );
}

type Tone = "remove" | "add" | undefined;
const rowSx = (index: number, count: number, tone: Tone) => (theme: Theme) => {
  const color =
    tone === "remove"
      ? theme.palette.error.main
      : tone === "add"
        ? theme.palette.primary.main
        : undefined;
  return {
    border: "1px solid",
    borderColor: color ?? theme.palette.divider,
    backgroundColor: color ? alpha(color, 0.06) : "transparent",
    borderRadius: groupedCardRadius(index, count),
    px: { xs: 1.5, sm: 2 },
    py: 1.25,
    minWidth: 0,
  };
};

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
  const theme = useTheme();
  const phone = useMediaQuery(theme.breakpoints.down("sm"));
  const ids = useId();
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
  const [menuAnchor, setMenuAnchor] = useState<HTMLElement | null>(null);
  const sequence = useRef(0);
  const controller = useRef<AbortController | null>(null);
  const reading = useRef(false);
  const backgroundFailures = useRef(0);
  const pendingRequest = useRef<{
    id: string;
    selection: string;
    body: Record<string, unknown>;
  }>(undefined);

  // Invalidates any read in flight and clears its state, so that its late
  // response is ignored and the next refresh can start.
  const cancelRead = useCallback(() => {
    ++sequence.current;
    controller.current?.abort();
    reading.current = false;
    setLoading(false);
  }, []);

  // A background refresh only replaces the displayed view. One failed refresh
  // keeps the last view; a second consecutive failure is reported.
  const load = useCallback(
    async (background = false) => {
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
          backgroundFailures.current = 0;
        }
      } catch (failure) {
        if (
          current === sequence.current &&
          !(failure instanceof DOMException && failure.name === "AbortError") &&
          (!background || ++backgroundFailures.current >= 2)
        )
          setLoadError(
            failure instanceof Error
              ? failure.message
              : t("读取失败，请重试。"),
          );
      } finally {
        if (current === sequence.current) {
          reading.current = false;
          setLoading(false);
        }
      }
    },
    [path],
  );

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
    setMenuAnchor(null);
    pendingRequest.current = undefined;
    backgroundFailures.current = 0;
    void load();
    return cancelRead;
  }, [open, load, cancelRead]);

  // Keep node statistics and membership progress live while the dialog is
  // open, at the console's refresh interval and only while the page is
  // visible. A refresh replaces only the displayed view: selections, paths and
  // preflight results are keyed by node and survive it. The confirmation step
  // keeps the view it summarises.
  useEffect(() => {
    if (!open || step !== "edit" || submitting || refreshMs <= 0) return;
    const refresh = () => {
      if (!document.hidden && !reading.current) void load(true);
    };
    const timer = window.setInterval(refresh, refreshMs);
    document.addEventListener("visibilitychange", refresh);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", refresh);
      // Entering the confirmation step or submitting: a refresh still in
      // flight must not replace the view being confirmed or the POST result.
      if (reading.current) cancelRead();
    };
  }, [open, step, submitting, refreshMs, load, cancelRead]);

  const nodes = view?.nodes || [];
  const byAddress = useMemo(
    () => new Map(nodes.map((node) => [node.address, node])),
    [nodes],
  );
  const candidates = view?.candidates || [];
  const candidateByAddress = useMemo(
    () => new Map(candidates.map((node) => [node.address, node])),
    [candidates],
  );
  // Selections that the latest view no longer offers are dropped.
  const validAdds = adds.filter(
    (address) =>
      candidateByAddress.get(address)?.eligible ||
      leaving(byAddress.get(address)?.state),
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
  const selection = {
    add: validAdds,
    remove: validRemoves,
    ...(local && joinAdds.length ? { bindings } : {}),
    ...(validRemoves.length ? { mode: effectiveMode } : {}),
  };
  const change = {
    ...selection,
    expectedGeneration: view?.generation,
    expectedNodeRevision: view?.nodeRevision,
  };
  // What the user chose. Live refreshes do not change it, so a passed
  // preflight stays valid while statistics update.
  const fingerprint = JSON.stringify(selection);
  // Matches the Master's rule: only active members that keep receiving work
  // count; joining, draining and disconnected members do not.
  const remaining = nodes.filter(
    (node) => node.state === "ACTIVE" && !validRemoves.includes(node.address),
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
    // A retry of the same change resends the same request, including the ID
    // and the membership revision it was based on, so a lost response cannot
    // apply the change twice even after a refresh brought a newer revision.
    if (pendingRequest.current?.selection !== fingerprint) {
      const id = Array.from(
        crypto.getRandomValues(new Uint8Array(16)),
        (byte) => byte.toString(16).padStart(2, "0"),
      ).join("");
      pendingRequest.current = {
        id,
        selection: fingerprint,
        body: { requestId: id, ...change },
      };
    }
    const body = pendingRequest.current.body;
    setSubmitting(true);
    setError("");
    try {
      const response = await api<{ data: ChangeResult }>(path, {
        method: "POST",
        body,
      });
      pendingRequest.current = undefined;
      const next = response.data.view;
      // The change's own view is newer than any read that may still be open
      // (for example a retry clicked on the confirmation step), and it
      // supersedes an earlier read failure.
      cancelRead();
      setLoadError("");
      backgroundFailures.current = 0;
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
  const locked = !editable || submitting;

  const nodeRow = (node: MemberNode, index: number) => {
    const state = memberStateLabel(node);
    const share = shareLabel(node.share);
    const qps = qpsShareLabel(node.share);
    const demand = node.share ? demandLabel(node) : "";
    const name = label(node.address || node.id || "", node);
    const isLeaving = leaving(node.state);
    const removing = validRemoves.includes(node.address);
    const keeping = isLeaving && validAdds.includes(node.address);
    const tone: Tone = removing ? "remove" : keeping ? "add" : undefined;
    const marker = removing ? t("将移出") : keeping ? t("将保留") : "";
    const actions: ReactNode[] = [];
    if (removing || keeping)
      actions.push(
        <Button
          key="undo"
          size="small"
          color="inherit"
          startIcon={<UndoRounded />}
          disabled={locked}
          aria-label={
            removing
              ? t("撤销移出 {node}", { node: name })
              : t("撤销保留 {node}", { node: name })
          }
          onClick={() => {
            if (removing) toggle(removes, setRemoves, node.address);
            else toggle(adds, setAdds, node.address);
          }}
        >
          {t("撤销")}
        </Button>,
      );
    else {
      if (isLeaving)
        actions.push(
          <Button
            key="keep"
            size="small"
            disabled={locked}
            aria-label={t("保留 {node}", { node: name })}
            onClick={() => toggle(adds, setAdds, node.address)}
          >
            {t("保留")}
          </Button>,
        );
      // Escalating a drain needs immediate removal; without it the draining
      // node can only be kept.
      if (node.state === "DRAINING") {
        if (!nowDisabled)
          actions.push(
            <Button
              key="now"
              size="small"
              disabled={locked}
              aria-label={t("改为立即移出 {node}", { node: name })}
              onClick={() => {
                toggle(removes, setRemoves, node.address);
                setMode("now");
              }}
            >
              {t("改为立即移出")}
            </Button>,
          );
      } else if (!isLeaving)
        actions.push(
          <Button
            key="remove"
            size="small"
            disabled={locked}
            aria-label={
              node.state === "JOINING"
                ? t("取消加入 {node}", { node: name })
                : t("移出 {node}", { node: name })
            }
            onClick={() => toggle(removes, setRemoves, node.address)}
          >
            {node.state === "JOINING" ? t("取消加入") : t("移出")}
          </Button>,
        );
    }
    return (
      <Box
        component="li"
        key={node.id || node.address}
        sx={rowSx(index, nodes.length, tone)}
        data-testid={`task-node-${node.address}`}
        data-marked={tone || undefined}
      >
        <Stack
          direction="row"
          columnGap={1}
          alignItems="flex-start"
          justifyContent="space-between"
          // On a phone, two actions move below the name instead of squeezing it.
          flexWrap="wrap"
        >
          <Stack
            direction="row"
            gap={1}
            alignItems="center"
            flexWrap="wrap"
            sx={{ minWidth: 0, py: 0.25, flex: "1 1 14rem" }}
          >
            <Typography
              variant="body2"
              sx={{
                fontWeight: 500,
                overflowWrap: "anywhere",
                ...(removing && {
                  textDecoration: "line-through",
                  color: "text.secondary",
                }),
              }}
            >
              {name}
            </Typography>
            {marker && (
              <Typography
                variant="caption"
                sx={{
                  fontWeight: 500,
                  color: removing ? "error.main" : "primary.main",
                }}
              >
                {marker}
              </Typography>
            )}
            {node.scanner && (
              <Typography variant="caption" color="text.secondary">
                {t("扫描节点")}
              </Typography>
            )}
            {state && (
              <Chip
                size="small"
                variant="outlined"
                color={stateColor(node.state)}
                label={state}
              />
            )}
            {node.online === false && node.state !== "DISCONNECTED" && (
              <Chip
                size="small"
                variant="outlined"
                color="error"
                label={t("离线")}
              />
            )}
          </Stack>
          <Stack
            direction="row"
            gap={0.5}
            sx={{ flexShrink: 0, mr: -1, mt: -0.25, ml: "auto" }}
          >
            {actions}
          </Stack>
        </Stack>
        <Stack
          direction="row"
          columnGap={2}
          rowGap={0.5}
          flexWrap="wrap"
          sx={{ mt: 0.5, opacity: removing ? 0.7 : 1 }}
        >
          {node.pendingChildren != null && (
            <Field label={isLeaving ? t("待完成或移交") : t("未完成子任务")}>
              {formatNumber(node.pendingChildren)}
            </Field>
          )}
          {/* Joining nodes get their share once they join. */}
          <Field label={t("带宽份额")}>{share || "—"}</Field>
          {qps && <Field label={t("请求份额")}>{qps}</Field>}
          {demand && <Field label={t("需求")}>{demand}</Field>}
          {node.localPath && (
            <Field label={t("本地路径")} grow>
              {node.localPath}
            </Field>
          )}
        </Stack>
        {node.state === "JOINING" && (
          <Typography
            variant="caption"
            color="text.secondary"
            component="div"
            sx={{ mt: 0.5 }}
          >
            {t("正在完成握手与检查，加入后开始分配任务")}
          </Typography>
        )}
        {node.progress === "LEASE_EXPIRY" && (
          <Typography
            variant="caption"
            color="text.secondary"
            component="div"
            sx={{ mt: 0.5 }}
          >
            {t("节点离线，执行租约到期后离开，其任务由其他节点继续")}
          </Typography>
        )}
        {node.progress === "RECONNECT" && (
          <Typography
            variant="caption"
            color="text.secondary"
            component="div"
            sx={{ mt: 0.5 }}
          >
            {t("节点离线，重新连接并完成或移交其任务后离开")}
          </Typography>
        )}
      </Box>
    );
  };

  // Where the scan stands. Members carry the scanner tag only while one scans
  // (including a new scanner taking over); afterwards the dialog says so here.
  const scanPhase = view?.scan?.phase;
  const scanNote =
    scanPhase === "COMPLETE"
      ? t("扫描已完成，不再需要扫描节点")
      : scanPhase === "HANDOFF"
        ? t("正在移交扫描，从 Master 保存的断点继续")
        : scanPhase === "WAITING"
          ? t("原扫描节点已退出，其他节点将从 Master 保存的断点接管扫描")
          : "";

  const bindingField = (node: { address: string } & NodeDisplay) => {
    const value = paths[node.address] ?? "";
    const check = preflight?.bindings.find(
      (binding) => binding.executionNodeIp === node.address,
    );
    const invalid = value.trim() !== "" && !isAbsoluteAgentPath(value.trim());
    return (
      <Box sx={{ mt: 1.5 }}>
        <TextField
          size="small"
          fullWidth
          label={t("{node} 的本地绝对路径", {
            node: label(node.address, node),
          })}
          value={value}
          disabled={locked}
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

  const addRow = (address: string, index: number) => {
    const candidate = candidateByAddress.get(address);
    const name = label(address, candidate);
    return (
      <Box
        component="li"
        key={address}
        sx={rowSx(index, joinAdds.length, "add")}
        data-testid={`task-node-add-${address}`}
      >
        <Stack
          direction="row"
          gap={1}
          alignItems="flex-start"
          justifyContent="space-between"
        >
          <Stack
            direction="row"
            gap={1}
            alignItems="center"
            flexWrap="wrap"
            sx={{ minWidth: 0, py: 0.25 }}
          >
            <Typography
              variant="body2"
              sx={{ fontWeight: 500, overflowWrap: "anywhere" }}
            >
              {name}
            </Typography>
            <Typography
              variant="caption"
              sx={{ fontWeight: 500, color: "primary.main" }}
            >
              {t("将添加")}
            </Typography>
          </Stack>
          <Button
            size="small"
            color="inherit"
            startIcon={<UndoRounded />}
            disabled={locked}
            aria-label={t("撤销添加 {node}", { node: name })}
            onClick={() => toggle(adds, setAdds, address)}
            sx={{ flexShrink: 0, mr: -1, mt: -0.25 }}
          >
            {t("撤销")}
          </Button>
        </Stack>
        {local && candidate && bindingField(candidate)}
      </Box>
    );
  };

  const eligible = candidates.filter((candidate) => candidate.eligible);
  const offered = candidates.filter(
    (candidate) => !validAdds.includes(candidate.address),
  );
  const offeredEligible = offered.filter((candidate) => candidate.eligible);

  const sectionTitle = (text: string, count?: number, id?: string) => (
    <Typography variant="subtitle2" id={id}>
      {text}
      {count != null && (
        <Typography
          component="span"
          variant="subtitle2"
          color="text.secondary"
          sx={{ ml: 1 }}
        >
          {formatNumber(count)}
        </Typography>
      )}
    </Typography>
  );

  const addSection = (
    <Box component="section" aria-labelledby={`${ids}-add`}>
      <Stack
        direction="row"
        alignItems="center"
        justifyContent="space-between"
        gap={1}
        sx={{ mb: 1, minHeight: 32 }}
      >
        {sectionTitle(t("添加节点"), undefined, `${ids}-add`)}
        {candidates.length > 0 && (
          <Button
            size="small"
            startIcon={<AddRounded />}
            disabled={locked || offered.length === 0}
            aria-haspopup="menu"
            aria-expanded={menuAnchor ? "true" : undefined}
            aria-controls={menuAnchor ? `${ids}-menu` : undefined}
            onClick={(event) => setMenuAnchor(event.currentTarget)}
            sx={{ mr: -1 }}
          >
            {t("添加 Agent")}
          </Button>
        )}
      </Stack>
      <Menu
        id={`${ids}-menu`}
        anchorEl={menuAnchor}
        open={!!menuAnchor}
        onClose={() => setMenuAnchor(null)}
        anchorOrigin={{ vertical: "bottom", horizontal: "right" }}
        transformOrigin={{ vertical: "top", horizontal: "right" }}
        slotProps={{ paper: { sx: { maxWidth: 420, minWidth: 260 } } }}
      >
        {offered.map((candidate) => (
          <MenuItem
            key={candidate.id || candidate.address}
            disabled={!candidate.eligible}
            onClick={() => {
              toggle(adds, setAdds, candidate.address);
              setMenuAnchor(null);
            }}
            sx={{
              whiteSpace: "normal",
              "&.Mui-disabled": { opacity: 1 },
            }}
          >
            <ListItemText
              primary={label(candidate.address, candidate)}
              secondary={
                candidate.eligible
                  ? undefined
                  : candidate.reason || t("暂不可添加")
              }
              slotProps={{
                primary: {
                  variant: "body2",
                  color: candidate.eligible ? "text.primary" : "text.disabled",
                  sx: { overflowWrap: "anywhere" },
                },
                secondary: { variant: "caption" },
              }}
            />
          </MenuItem>
        ))}
      </Menu>
      {candidates.length === 0 ? (
        <Box
          sx={{
            border: "1px dashed",
            borderColor: "divider",
            borderRadius: radii.inset,
            px: 2,
            py: 2,
            textAlign: "center",
          }}
        >
          <Typography variant="body2">{t("没有可添加的在线 Agent")}</Typography>
          <Typography variant="caption" color="text.secondary">
            {t("Agent 上线并完成注册后会出现在这里。")}
          </Typography>
        </Box>
      ) : joinAdds.length === 0 ? (
        <Typography variant="body2" color="text.secondary">
          {eligible.length === 0
            ? t("在线 Agent 暂不满足加入条件，可在列表中查看原因。")
            : offeredEligible.length === 0
              ? t("所有可添加的 Agent 都已选择。")
              : t("{count} 个在线 Agent 可添加。", {
                  count: formatNumber(offeredEligible.length),
                })}
        </Typography>
      ) : (
        <Stack component="ul" gap={0.5} sx={{ m: 0, p: 0, listStyle: "none" }}>
          {joinAdds.map(addRow)}
        </Stack>
      )}
      {needsPreflight && (
        <Box sx={{ mt: 1.5 }}>
          <Stack direction="row" gap={1.5} alignItems="center" flexWrap="wrap">
            <Button
              variant="outlined"
              size="small"
              disabled={locked || pathMissing || checking}
              onClick={() => void runPreflight()}
              startIcon={
                checking ? (
                  <CircularProgress size={16} color="inherit" />
                ) : undefined
              }
            >
              {t("预检")}
            </Button>
            {!preflightPassed && preflight?.fingerprint !== fingerprint && (
              <Typography variant="caption" color="text.secondary">
                {t("本地传输需要预检通过后才能提交")}
              </Typography>
            )}
          </Stack>
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
                  {binding.valid ? t("通过") : binding.message || t("未通过")}
                </Typography>
              ))}
            </Alert>
          )}
        </Box>
      )}
    </Box>
  );

  const modeOption = (value: Mode, title: string, description: string) => {
    const disabled = submitting || (value === "now" && nowDisabled);
    const selected = effectiveMode === value;
    return (
      <FormControlLabel
        value={value}
        disabled={disabled}
        control={<Radio size="small" sx={{ mt: -0.5 }} />}
        label={
          <Box>
            <Typography variant="body2" sx={{ fontWeight: 500 }}>
              {title}
            </Typography>
            <Typography
              variant="caption"
              color="text.secondary"
              component="div"
            >
              {description}
            </Typography>
          </Box>
        }
        sx={{
          m: 0,
          alignItems: "flex-start",
          border: "1px solid",
          borderColor: selected && !disabled ? "primary.main" : "divider",
          borderRadius: radii.control,
          px: 1,
          py: 1,
          pr: 1.5,
          "&:has(.Mui-focusVisible)": {
            outline: "2px solid",
            outlineColor: "primary.main",
            outlineOffset: 2,
          },
          "& .MuiFormControlLabel-label.Mui-disabled .MuiTypography-caption": {
            color: "text.secondary",
          },
        }}
      />
    );
  };

  const modeSection = validRemoves.length > 0 && (
    <Box component="section">
      <Typography variant="subtitle2" id={`${ids}-mode`} sx={{ mb: 1 }}>
        {t("移出方式")}
      </Typography>
      <RadioGroup
        aria-labelledby={`${ids}-mode`}
        value={effectiveMode}
        onChange={(event) => {
          setMode(event.target.value as Mode);
          setError("");
        }}
        sx={{
          display: "grid",
          gridTemplateColumns: { xs: "1fr", sm: "1fr 1fr" },
          gap: 1,
        }}
      >
        {modeOption(
          "drain",
          t("平滑移出（推荐）"),
          t("正在传输的子任务完成后离开"),
        )}
        {modeOption(
          "now",
          t("立即移出"),
          nowDisabled
            ? view?.nowReason || t("当前任务不支持立即移出")
            : t("正在传输的子任务转给其他节点继续"),
        )}
      </RadioGroup>
    </Box>
  );

  // What the change does, one clause per kind, for the footer and the
  // confirmation button.
  const parts = [
    joinAdds.length ? t("添加 {count} 个节点", { count: joinAdds.length }) : "",
    validRemoves.length
      ? t("移出 {count} 个节点", { count: validRemoves.length })
      : "",
    cancels ? t("保留 {count} 个节点", { count: cancels }) : "",
  ].filter(Boolean);
  const kinds = parts.length;
  const confirmLabel = kinds === 1 ? parts[0] : t("提交变更");
  const blocked = !editable
    ? view?.reason || t("当前任务不可调整执行节点")
    : empty
      ? t("选择要添加或移出的节点")
      : lastNode
        ? t("至少保留一个执行节点")
        : pathMissing
          ? t("填写新节点的本地绝对路径后才能继续")
          : checking
            ? t("正在预检…")
            : !preflightPassed
              ? t("预检通过后才能继续")
              : "";
  const canConfirm = !blocked && !submitting;
  const footerText =
    step === "confirm"
      ? view?.status === "RUNNING"
        ? t("变更将立即开始生效。")
        : t("变更将在任务继续运行时生效。")
      : blocked || t("待提交：{changes}", { changes: parts.join(" · ") });
  const footerWarning = step === "edit" && editable && !empty && lastNode;

  const modeLabel = effectiveMode === "now" ? t("立即移出") : t("平滑移出");
  const summarySection = (title: string, items: ReactNode[], note?: string) => (
    <Box>
      <Typography variant="caption" color="text.secondary" component="div">
        {title}
      </Typography>
      <Box component="ul" sx={{ m: 0, pl: 2.5 }}>
        {items.map((item, index) => (
          <Typography
            key={index}
            component="li"
            variant="body2"
            sx={{ overflowWrap: "anywhere" }}
          >
            {item}
          </Typography>
        ))}
      </Box>
      {note && (
        <Typography variant="caption" color="text.secondary" component="div">
          {note}
        </Typography>
      )}
    </Box>
  );

  return (
    <Dialog
      open={open}
      onClose={() => {
        if (!submitting) onClose();
      }}
      maxWidth="sm"
      fullWidth
      fullScreen={phone}
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
          <Stack gap={3}>
            {!view.editable && (
              <Alert severity="info">
                {view.reason || t("当前任务不可调整执行节点")}
              </Alert>
            )}
            {view.editable && view.pending && (
              <Alert severity="info" role="status">
                {t(
                  "上一次节点变更仍在进行中。完成前只能取消加入中的节点、保留正在移出的节点，或把平滑移出改为立即移出。",
                )}
              </Alert>
            )}
            <Box component="section" aria-labelledby={`${ids}-current`}>
              <Stack
                direction="row"
                alignItems="center"
                justifyContent="space-between"
                gap={1}
                flexWrap="wrap"
                sx={{ mb: 1 }}
              >
                {sectionTitle(t("当前节点"), nodes.length, `${ids}-current`)}
                {scanPhase === "COMPLETE" && (
                  <Chip
                    size="small"
                    variant="outlined"
                    color="success"
                    label={t("扫描已完成")}
                  />
                )}
              </Stack>
              {scanNote && (
                <Typography
                  variant="caption"
                  color="text.secondary"
                  component="div"
                  data-testid="task-nodes-scan"
                  sx={{ mb: 1 }}
                >
                  {scanNote}
                </Typography>
              )}
              <Stack
                component="ul"
                gap={0.5}
                sx={{ m: 0, p: 0, listStyle: "none" }}
              >
                {nodes.map(nodeRow)}
              </Stack>
            </Box>
            {modeSection}
            {addSection}
          </Stack>
        )}
        {view && step === "confirm" && (
          <Stack gap={2} data-testid="task-nodes-summary">
            <Typography variant="body2">
              {t("请确认以下执行节点变更：")}
            </Typography>
            {joinAdds.length > 0 &&
              summarySection(
                t("添加"),
                joinAdds.map((address) =>
                  local
                    ? `${summaryLabel(address)} (${(paths[address] ?? "").trim()})`
                    : summaryLabel(address),
                ),
              )}
            {cancels > 0 &&
              summarySection(
                t("保留（取消移出）"),
                validAdds
                  .filter((address) => byAddress.has(address))
                  .map(summaryLabel),
              )}
            {validRemoves.length > 0 &&
              summarySection(
                t("移出（{mode}）", { mode: modeLabel }),
                validRemoves.map(summaryLabel),
                effectiveMode === "now"
                  ? t("正在传输的子任务转给其他节点继续")
                  : t("正在传输的子任务完成后离开"),
              )}
          </Stack>
        )}
        {error && (
          <Alert severity="error" sx={{ mt: 2 }}>
            {error}
          </Alert>
        )}
      </DialogContent>
      <DialogActions
        sx={{
          flexWrap: "wrap",
          gap: 1,
          "& > :not(style) ~ :not(style)": { ml: 0 },
          borderTop: phone ? 1 : 0,
          borderColor: "divider",
          pt: phone ? 1.5 : undefined,
        }}
      >
        {view && (
          <Typography
            variant="body2"
            role="status"
            id={`${ids}-footer`}
            data-testid="task-nodes-footer"
            sx={{
              flex: "1 1 14rem",
              minWidth: 0,
              color: footerWarning ? "warning.main" : "text.secondary",
            }}
          >
            {footerText}
          </Typography>
        )}
        <Stack direction="row" gap={1} sx={{ ml: "auto" }}>
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
                {submitting ? t("正在提交") : confirmLabel}
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
                aria-describedby={view ? `${ids}-footer` : undefined}
                onClick={() => {
                  setError("");
                  setStep("confirm");
                }}
              >
                {t("预览变更")}
              </Button>
            </>
          )}
        </Stack>
      </DialogActions>
    </Dialog>
  );
}
