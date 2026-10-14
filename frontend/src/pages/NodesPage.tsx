import { useState } from "react";
import { Link as RouterLink } from "react-router-dom";
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
  TableRow,
  Tabs,
  TextField,
  Typography,
} from "@mui/material";
import {
  DnsRounded,
  HistoryRounded,
  SpeedRounded,
  TuneRounded,
  AddRounded,
  EditOutlined,
  DeleteOutlineRounded,
} from "@mui/icons-material";
import { api, useApi, useNotify } from "../lib/api";
import { useWorkerStatus } from "../lib/workerStatus";
import type { Worker } from "../lib/rateStatus";
import NodeRateLimits, {
  adjustmentDetailsPath,
  adjustmentPhase,
  adjustmentReason,
  rateNames,
  rateText,
  rateTypes,
  rateTarget,
  type RateType,
  type Adjustment,
} from "../components/NodeRateLimits";
import {
  refreshDescription,
  useRefreshPreference,
} from "../lib/refreshPreference";
import RefreshControls from "../components/RefreshControls";
import AgentInvitationDialog from "../components/AgentInvitationDialog";
import { EmptyState, PageHeader, formatNumber } from "../components/Common";
import {
  removeNode,
  saveNode,
  type NodeRegistry,
  type NodeChange,
  type RegisteredNode,
} from "../lib/nodeRegistry";

const queues = [
  { key: "list", label: "对象扫描" },
  { key: "put", label: "对象传输" },
  { key: "progress", label: "进度上报" },
  { key: "failedKey", label: "失败记录" },
  { key: "fallback", label: "降级记录" },
];
export default function NodesPage() {
  const { refreshMs, refreshSeconds } = useRefreshPreference();
  const workers = useWorkerStatus();
  const registry = useApi<{ data: NodeRegistry }>(
    "/api/master/nodes",
    refreshMs,
  );
  const history = useApi<{
    adjustments: Adjustment[];
    partial?: boolean;
    complete?: boolean;
    failedWorkers?: string[];
    errors?: Record<string, string>;
  }>("/api/master/rate-limit/history?limit=50", refreshMs);
  const notify = useNotify();
  const [tab, setTab] = useState(0);
  const [edit, setEdit] = useState<{
    worker: string;
    type: RateType;
    value: string;
  } | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [nodeEdit, setNodeEdit] = useState<{
    original: string | null;
    address: string;
    label: string;
    master: boolean;
    agent: boolean;
    roots: string | null;
    initialRoots: string | null;
    goAgent: boolean;
  } | null>(null);
  const [nodeDelete, setNodeDelete] = useState<RegisteredNode | null>(null);
  const [nodeSaving, setNodeSaving] = useState(false);
  const [nodeError, setNodeError] = useState<string | null>(null);
  const [inviteOpen, setInviteOpen] = useState(false);
  const knownWorkers = workers.data
    ? [
        ...new Set([
          ...Object.keys(workers.data.workers ?? {}),
          ...(registry.data?.data?.nodes ?? []).map((node) => node.address),
        ]),
      ]
    : [];
  const entries: [string, Worker][] = knownWorkers.map((ip) => [
    ip,
    workers.data?.workers?.[ip] ?? {
      success: false,
      message: "节点未返回状态，请检查节点服务和网络。",
    },
  ]);
  const reachable = entries.filter(([address, worker]) => {
    const node = registry.data?.data?.nodes?.find(
      (entry) => entry.address === address,
    );
    return worker.success && (!node?.agent || node.online === true);
  }).length;
  const refresh = () =>
    Promise.all([workers.refresh(), history.refresh(), registry.refresh()]);
  const openNodeEditor = (node?: RegisteredNode) => {
    setNodeError(null);
    const status = node ? workers.data?.workers?.[node.address] : undefined;
    const declaredRoots = status?.localStorage?.advertisedLocalRoots;
    const roots =
      status?.success &&
      !workers.stale &&
      Array.isArray(declaredRoots) &&
      declaredRoots.every((root) => typeof root === "string")
        ? declaredRoots.join("\n")
        : null;
    setNodeEdit({
      original: node?.address ?? null,
      address: node?.address ?? "",
      label: node?.label ?? "",
      master: !!node?.master,
      agent: !!node?.agent,
      roots,
      initialRoots: roots,
      goAgent: !!status?.executionStatus,
    });
  };
  async function saveNodeEdit() {
    if (!nodeEdit) return;
    if (!nodeEdit.address.trim()) {
      setNodeError("请输入节点标识。");
      return;
    }
    setNodeSaving(true);
    setNodeError(null);
    try {
      const change: NodeChange = {
        address: nodeEdit.address.trim(),
        label: nodeEdit.label.trim(),
      };
      if (
        nodeEdit.goAgent &&
        nodeEdit.roots !== null &&
        nodeEdit.roots !== nodeEdit.initialRoots
      ) {
        const current = workers.data?.workers?.[nodeEdit.original ?? ""];
        if (!current?.success || workers.stale) {
          throw new Error("节点状态已失效，请刷新后重新编辑根目录。");
        }
        const roots = nodeEdit.roots
          .split(/\r?\n/)
          .filter((root) => root !== "");
        if (new Set(roots).size !== roots.length) {
          throw new Error("本地根目录不能重复。");
        }
        change.advertisedLocalRoots = roots;
      }
      await saveNode(nodeEdit.original, change);
      notify(
        nodeEdit.original ? "节点设置已保存。" : "节点已添加。",
        "success",
      );
      setNodeEdit(null);
      await refresh();
    } catch (error) {
      setNodeError(error instanceof Error ? error.message : String(error));
    } finally {
      setNodeSaving(false);
    }
  }
  async function deleteSelectedNode() {
    if (!nodeDelete) return;
    setNodeSaving(true);
    setNodeError(null);
    try {
      await removeNode(nodeDelete.address);
      notify("节点已移除。", "success");
      setNodeDelete(null);
      await refresh();
    } catch (error) {
      setNodeError(error instanceof Error ? error.message : String(error));
    } finally {
      setNodeSaving(false);
    }
  }
  const targetForEditor = (worker: string, type: RateType) => {
    const target = rateTarget(
      workers.data?.workers?.[worker]?.rateLimit?.[type],
      type,
    );
    return target === undefined
      ? ""
      : String(Math.max(0, target) / (type === "bandwidth" ? 1_000_000 : 1));
  };
  const openEditor = (worker = "", type: RateType = "put", value?: number) => {
    setSaveError(null);
    setEdit({
      worker,
      type,
      value:
        value === undefined
          ? ""
          : String(
              value > 0
                ? type === "bandwidth"
                  ? value / 1_000_000
                  : value
                : 0,
            ),
    });
  };
  async function saveRate() {
    const qps = edit
      ? Number(edit.value) * (edit.type === "bandwidth" ? 1_000_000 : 1)
      : NaN;
    if (!edit || edit.value.trim() === "" || !Number.isFinite(qps) || qps < 0) {
      setSaveError("请输入大于或等于 0 的数值。");
      return;
    }
    setSaving(true);
    setSaveError(null);
    try {
      await api(
        `/api/master/rate-limit/${edit.type}${edit.worker ? `/${encodeURIComponent(edit.worker)}` : ""}`,
        { method: "POST", body: { qps } },
      );
      notify("限速设置已提交，正在刷新节点状态。", "success");
      setEdit(null);
      await refresh();
    } catch (error) {
      setSaveError(error instanceof Error ? error.message : String(error));
      await workers.refresh();
    } finally {
      setSaving(false);
    }
  }

  return (
    <Stack spacing={3}>
      <PageHeader
        title="执行节点"
        description="查看节点状态和速率。"
        action={
          <Stack
            direction="row"
            spacing={1}
            flexWrap="wrap"
            useFlexGap
            alignItems="center"
          >
            <Button
              variant="outlined"
              startIcon={<AddRounded />}
              onClick={() => setInviteOpen(true)}
            >
              添加节点
            </Button>
            <RefreshControls
              onRefresh={refresh}
              loading={workers.loading || registry.loading || history.loading}
            />
            <Button
              variant="contained"
              startIcon={<TuneRounded />}
              onClick={() => openEditor()}
              disabled={!entries.length}
            >
              调整限速
            </Button>
          </Stack>
        }
      />
      {workers.error && <Alert severity="error">{workers.error}</Alert>}
      {registry.error && (
        <Alert severity="error">节点配置读取失败：{registry.error}</Alert>
      )}
      <Paper variant="outlined" sx={{ p: { xs: 2, md: 3 } }}>
        <Stack
          direction={{ xs: "column", sm: "row" }}
          spacing={3}
          alignItems={{ sm: "center" }}
          justifyContent="space-between"
        >
          <Stack direction="row" spacing={2} alignItems="center">
            <Box
              sx={{
                p: 1.5,
                bgcolor: "action.selected",
                color: "primary.main",
                borderRadius: 3,
                display: "flex",
              }}
            >
              <DnsRounded />
            </Box>
            <Box>
              <Typography variant="h6">节点列表</Typography>
              <Typography variant="body2" color="text.secondary">
                {workers.data
                  ? `${reachable} / ${entries.length} 个节点正常响应`
                  : "正在读取节点状态"}
              </Typography>
            </Box>
          </Stack>
          <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
            {workers.data && (
              <Chip
                size="small"
                label={
                  reachable === entries.length && entries.length > 0
                    ? "节点连接正常"
                    : entries.length
                      ? "部分节点不可达"
                      : "尚未配置节点"
                }
                color={
                  reachable === entries.length && entries.length > 0
                    ? "success"
                    : "warning"
                }
                variant="outlined"
              />
            )}
            <Chip
              size="small"
              label={refreshDescription(refreshSeconds)}
              variant="outlined"
            />
          </Stack>
        </Stack>
      </Paper>
      <Tabs
        value={tab}
        onChange={(_, value) => setTab(value)}
        aria-label="节点视图"
        sx={{ borderBottom: 1, borderColor: "divider" }}
      >
        <Tab
          icon={<SpeedRounded fontSize="small" />}
          iconPosition="start"
          label="实时状态"
        />
        <Tab
          icon={<HistoryRounded fontSize="small" />}
          iconPosition="start"
          label="限速历史"
        />
      </Tabs>
      {workers.loading && !workers.data && <LinearProgress />}
      {tab === 0 && (
        <>
          {workers.data && entries.length === 0 && (
            <EmptyState
              title="暂无执行节点"
              description="添加 Agent 后可查看节点状态。"
            />
          )}
          <Box
            sx={{
              display: "grid",
              gridTemplateColumns: {
                xs: "1fr",
                lg: "repeat(2, minmax(0, 1fr))",
              },
              gap: 3,
              alignItems: "start",
            }}
          >
            {entries.map(([ip, worker]) => {
              const registered = registry.data?.data?.nodes?.find(
                (node) => node.address === ip,
              );
              const execution = worker.executionStatus;
              const activity = execution
                ? [
                    {
                      key: "scans",
                      label: "扫描并发",
                      size: execution.activeScans,
                      capacity: execution.maxConcurrentScans,
                    },
                    {
                      key: "children",
                      label: "传输并发",
                      size: execution.activeChildren,
                      capacity: execution.maxConcurrentChildren,
                    },
                    {
                      key: "unfinished",
                      label: "未完成子任务",
                      size: execution.unfinishedChildren,
                      capacity: execution.maxUnfinishedChildren,
                    },
                  ]
                : queues.map((queue) => ({
                    ...queue,
                    size: worker.queueStatus?.[`${queue.key}QueueSize`],
                    capacity: worker.queueStatus?.[`${queue.key}QueueCapacity`],
                  }));
              const visibleActivity = activity.filter(
                (
                  item,
                ): item is typeof item & { size: number; capacity: number } =>
                  typeof item.size === "number" &&
                  Number.isFinite(item.size) &&
                  item.size >= 0 &&
                  typeof item.capacity === "number" &&
                  Number.isFinite(item.capacity) &&
                  item.capacity > 0,
              );
              const localRoots = worker.localStorage?.advertisedLocalRoots;
              return (
                <Paper
                  key={ip}
                  component="section"
                  aria-label={`执行节点 ${ip}`}
                  variant="outlined"
                  sx={{ overflow: "hidden" }}
                >
                  <Stack
                    direction="row"
                    spacing={2}
                    justifyContent="space-between"
                    alignItems="center"
                    sx={{ p: 2.5 }}
                  >
                    <Box>
                      <Typography variant="h6" sx={{ fontFamily: "monospace" }}>
                        {ip}
                      </Typography>
                      <Typography variant="body2" color="text.secondary">
                        {registered?.label ||
                          (registered?.master
                            ? "主节点 · 执行节点"
                            : "执行节点")}
                      </Typography>
                      {registered?.agent &&
                        !registered.online &&
                        registered.lastSeen && (
                          <Typography variant="caption" color="text.secondary">
                            最后连接：
                            {new Date(registered.lastSeen).toLocaleString(
                              "zh-CN",
                              { hour12: false },
                            )}
                          </Typography>
                        )}
                    </Box>
                    <Stack direction="row" spacing={1} alignItems="center">
                      {registered?.agent && (
                        <Chip size="small" variant="outlined" label="Agent" />
                      )}
                      <Chip
                        size="small"
                        variant="outlined"
                        color={
                          (
                            registered?.agent
                              ? registered.online === true
                              : worker.success
                          )
                            ? "success"
                            : "error"
                        }
                        label={
                          registered?.agent
                            ? registered.online
                              ? "在线"
                              : "离线"
                            : worker.success
                              ? "在线"
                              : "无法连接"
                        }
                      />
                    </Stack>
                  </Stack>
                  {registered && (
                    <Stack direction="row" spacing={1} sx={{ px: 2.5, pb: 2 }}>
                      <Button
                        size="small"
                        startIcon={<EditOutlined />}
                        onClick={() => openNodeEditor(registered)}
                      >
                        编辑节点
                      </Button>
                      {!registered.master && (
                        <Button
                          size="small"
                          color="error"
                          startIcon={<DeleteOutlineRounded />}
                          onClick={() => {
                            setNodeError(null);
                            setNodeDelete(registered);
                          }}
                        >
                          移除节点
                        </Button>
                      )}
                    </Stack>
                  )}
                  {!worker.success ? (
                    <Alert severity="error" sx={{ mx: 2.5, mb: 2.5 }}>
                      {worker.message ||
                        "节点未返回有效状态，请检查节点服务和网络。"}
                    </Alert>
                  ) : (
                    <>
                      <Divider />
                      <Box sx={{ p: 2.5 }}>
                        <NodeRateLimits
                          ip={ip}
                          rateLimit={worker.rateLimit}
                          refreshMs={refreshMs}
                          stale={workers.stale}
                          onEdit={openEditor}
                        />
                      </Box>
                      {visibleActivity.length > 0 && (
                        <>
                          <Divider />
                          <Box sx={{ p: 2.5 }}>
                            <Typography variant="subtitle2" sx={{ mb: 2 }}>
                              {execution ? "执行负载" : "队列占用"}
                            </Typography>
                            <Stack spacing={1.75}>
                              {visibleActivity.map((queue) => {
                                const { size, capacity } = queue;
                                const percentage = Math.min(
                                  100,
                                  (size / capacity) * 100,
                                );
                                return (
                                  <Box key={queue.key}>
                                    <Stack
                                      direction="row"
                                      justifyContent="space-between"
                                      sx={{ mb: 0.75 }}
                                    >
                                      <Typography
                                        variant="body2"
                                        color="text.secondary"
                                      >
                                        {queue.label}
                                      </Typography>
                                      <Typography variant="caption">
                                        {formatNumber(size)} /{" "}
                                        {formatNumber(capacity)}
                                      </Typography>
                                    </Stack>
                                    <LinearProgress
                                      variant="determinate"
                                      value={percentage}
                                      color={
                                        percentage > 85 ? "warning" : "primary"
                                      }
                                      sx={{ height: 5, borderRadius: 5 }}
                                      aria-label={
                                        execution
                                          ? queue.label
                                          : `${queue.label}队列占用`
                                      }
                                    />
                                  </Box>
                                );
                              })}
                            </Stack>
                          </Box>
                        </>
                      )}
                      {Array.isArray(localRoots) && (
                        <>
                          <Divider />
                          <Box sx={{ p: 2.5 }}>
                            <Typography variant="subtitle2" sx={{ mb: 1 }}>
                              本地根目录
                            </Typography>
                            {localRoots.length > 0 ? (
                              localRoots.map((root) => (
                                <Typography
                                  key={root}
                                  variant="body2"
                                  sx={{
                                    fontFamily: "monospace",
                                    overflowWrap: "anywhere",
                                  }}
                                >
                                  {root}
                                </Typography>
                              ))
                            ) : (
                              <Typography
                                variant="body2"
                                color="text.secondary"
                              >
                                未设置
                              </Typography>
                            )}
                          </Box>
                        </>
                      )}
                    </>
                  )}
                </Paper>
              );
            })}
          </Box>
        </>
      )}
      {tab === 1 && (
        <Stack spacing={2}>
          {history.error && <Alert severity="error">{history.error}</Alert>}
          {(history.data?.partial || history.data?.complete === false) && (
            <Alert severity="warning">
              限速历史不完整。
              {history.data.failedWorkers?.length
                ? `未获取到节点 ${history.data.failedWorkers.join("、")} 的记录。`
                : "部分节点未返回记录。"}
            </Alert>
          )}
          <Typography variant="body2" color="text.secondary">
            显示各节点最近 50
            条记录。自动降速与恢复改变运行上限，不覆盖已保存目标。旧记录未保存的调整前值和原因显示为未知。
          </Typography>
          {history.loading && !history.data && <LinearProgress />}
          {history.data?.adjustments?.length ? (
            <TableContainer component={Paper} variant="outlined">
              <Table size="small">
                <TableHead>
                  <TableRow>
                    <TableCell>时间</TableCell>
                    <TableCell>节点</TableCell>
                    <TableCell>类型</TableCell>
                    <TableCell>运行上限变化</TableCell>
                    <TableCell>阶段</TableCell>
                    <TableCell>依据</TableCell>
                    <TableCell>详情</TableCell>
                  </TableRow>
                </TableHead>
                <TableBody>
                  {[...history.data.adjustments]
                    .sort((a, b) => (b.timestamp ?? 0) - (a.timestamp ?? 0))
                    .map((item, index) => (
                      <TableRow
                        key={`${item.workerIp}-${item.timestamp}-${index}`}
                      >
                        <TableCell sx={{ whiteSpace: "nowrap" }}>
                          {item.timestampFormatted ||
                            (item.timestamp
                              ? new Date(item.timestamp).toLocaleString()
                              : "—")}
                        </TableCell>
                        <TableCell sx={{ fontFamily: "monospace" }}>
                          {item.workerIp}
                        </TableCell>
                        <TableCell>
                          {rateNames[item.type] || item.type}
                        </TableCell>
                        <TableCell sx={{ whiteSpace: "nowrap" }}>
                          {rateText(
                            item.previousValue,
                            item.type === "bandwidth",
                          )}{" "}
                          → {rateText(item.value, item.type === "bandwidth")}
                        </TableCell>
                        <TableCell>
                          <Chip
                            size="small"
                            label={adjustmentPhase(item)}
                            color={
                              item.reasonCode === "OSS_503"
                                ? "warning"
                                : item.reasonCode === "RECOVERED"
                                  ? "success"
                                  : "default"
                            }
                            variant="outlined"
                          />
                        </TableCell>
                        <TableCell sx={{ minWidth: 240 }}>
                          {adjustmentReason(item)}
                          {item.manualTarget != null && (
                            <Typography
                              variant="caption"
                              display="block"
                              color="text.secondary"
                            >
                              当时目标：
                              {rateText(
                                item.manualTarget,
                                item.type === "bandwidth",
                              )}
                            </Typography>
                          )}
                        </TableCell>
                        <TableCell sx={{ whiteSpace: "nowrap" }}>
                          {item.id &&
                          item.workerIp &&
                          item.detailsAvailable !== false ? (
                            <Button
                              component={RouterLink}
                              to={adjustmentDetailsPath(item.workerIp, item.id)}
                              size="small"
                            >
                              查看详情
                            </Button>
                          ) : (
                            <Typography
                              variant="caption"
                              color="text.secondary"
                            >
                              未记录详情
                            </Typography>
                          )}
                        </TableCell>
                      </TableRow>
                    ))}
                </TableBody>
              </Table>
            </TableContainer>
          ) : (
            !history.loading &&
            !history.error && (
              <EmptyState
                title={
                  history.data?.partial || history.data?.complete === false
                    ? "已响应节点暂无调整记录"
                    : "暂无调整记录"
                }
                description={
                  history.data?.partial || history.data?.complete === false
                    ? "其余节点的历史尚未获取，无法确认是否发生调节。"
                    : "限速调整记录生成后自动显示。"
                }
              />
            )
          )}
        </Stack>
      )}
      {inviteOpen && (
        <AgentInvitationDialog open onClose={() => setInviteOpen(false)} />
      )}
      <Dialog
        open={nodeEdit !== null}
        onClose={() => !nodeSaving && setNodeEdit(null)}
        fullWidth
        maxWidth="sm"
      >
        <DialogTitle>
          {nodeEdit?.original ? "编辑节点" : "添加节点"}
        </DialogTitle>
        <DialogContent>
          <Stack spacing={2.5} sx={{ pt: 1 }}>
            {nodeError && <Alert severity="error">{nodeError}</Alert>}
            <TextField
              label={nodeEdit?.agent ? "节点标识" : "节点地址"}
              required
              fullWidth
              value={nodeEdit?.address ?? ""}
              disabled={nodeSaving || !!nodeEdit?.master || !!nodeEdit?.agent}
              onChange={(event) =>
                setNodeEdit(
                  (value) => value && { ...value, address: event.target.value },
                )
              }
              helperText={
                nodeEdit?.agent
                  ? undefined
                  : `服务端口：${registry.data?.data?.port ?? "使用集群配置"}`
              }
            />
            <TextField
              label="节点名称"
              fullWidth
              value={nodeEdit?.label ?? ""}
              disabled={nodeSaving}
              slotProps={{ htmlInput: { maxLength: 80 } }}
              onChange={(event) =>
                setNodeEdit(
                  (value) => value && { ...value, label: event.target.value },
                )
              }
            />
            {nodeEdit?.goAgent && (
              <TextField
                label="本地根目录"
                multiline
                minRows={3}
                fullWidth
                value={nodeEdit.roots ?? ""}
                disabled={
                  nodeSaving ||
                  nodeEdit.roots === null ||
                  workers.stale ||
                  !workers.data?.workers?.[nodeEdit.original ?? ""]?.success
                }
                helperText={
                  nodeEdit.roots === null
                    ? "节点未返回根目录配置，暂时不能编辑。"
                    : "每行一个绝对路径。用于路径选择提示，不限制节点的文件访问权限。"
                }
                onChange={(event) =>
                  setNodeEdit(
                    (value) => value && { ...value, roots: event.target.value },
                  )
                }
              />
            )}
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button disabled={nodeSaving} onClick={() => setNodeEdit(null)}>
            取消
          </Button>
          <Button
            variant="contained"
            disabled={nodeSaving}
            onClick={() => void saveNodeEdit()}
          >
            {nodeSaving ? "正在保存…" : "保存节点"}
          </Button>
        </DialogActions>
      </Dialog>
      <Dialog
        open={nodeDelete !== null}
        onClose={() => !nodeSaving && setNodeDelete(null)}
        fullWidth
        maxWidth="sm"
      >
        <DialogTitle>移除节点 {nodeDelete?.address}</DialogTitle>
        <DialogContent>
          <Stack spacing={2}>
            {nodeError && <Alert severity="error">{nodeError}</Alert>}
            <Typography>
              移除此执行节点，不删除节点上的文件、对象或数据库。
            </Typography>
            <Alert severity="info">
              节点不可达、仍有任务记录或集群正在执行任务时，无法移除。
            </Alert>
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button disabled={nodeSaving} onClick={() => setNodeDelete(null)}>
            取消
          </Button>
          <Button
            color="error"
            variant="contained"
            disabled={nodeSaving}
            onClick={() => void deleteSelectedNode()}
          >
            {nodeSaving ? "正在检查…" : "确认移除"}
          </Button>
        </DialogActions>
      </Dialog>
      <Dialog
        open={edit !== null}
        onClose={() => !saving && setEdit(null)}
        fullWidth
        maxWidth="sm"
      >
        <DialogTitle>调整传输限速</DialogTitle>
        <DialogContent>
          <Stack spacing={2.5} sx={{ pt: 1 }}>
            {saveError && <Alert severity="error">{saveError}</Alert>}
            <TextField
              select
              fullWidth
              label="应用范围"
              slotProps={{
                select: { displayEmpty: true },
                inputLabel: { shrink: true },
              }}
              value={edit?.worker ?? ""}
              onChange={(event) =>
                setEdit(
                  (value) =>
                    value && {
                      ...value,
                      worker: event.target.value,
                      value: targetForEditor(event.target.value, value.type),
                    },
                )
              }
            >
              <MenuItem value="">全部执行节点</MenuItem>
              {entries.map(([ip]) => (
                <MenuItem key={ip} value={ip}>
                  {ip}
                </MenuItem>
              ))}
            </TextField>
            <TextField
              select
              fullWidth
              label="限制类型"
              value={edit?.type ?? "put"}
              onChange={(event) =>
                setEdit(
                  (value) =>
                    value && {
                      ...value,
                      type: event.target.value as RateType,
                      value: targetForEditor(
                        value.worker,
                        event.target.value as RateType,
                      ),
                    },
                )
              }
            >
              {rateTypes.map((type) => (
                <MenuItem key={type} value={type}>
                  {rateNames[type]}
                </MenuItem>
              ))}
            </TextField>
            <TextField
              autoFocus
              required
              fullWidth
              label={
                edit?.type === "bandwidth"
                  ? "带宽上限（Mbps）"
                  : "请求上限（次/秒）"
              }
              type="number"
              value={edit?.value ?? ""}
              onChange={(event) =>
                setEdit(
                  (value) => value && { ...value, value: event.target.value },
                )
              }
              slotProps={{ htmlInput: { min: 0, step: "any" } }}
              helperText="修改目标上限，0 表示不限速。保存后立即应用，节点重启后仍生效；临时降速不覆盖该目标。"
            />
            {!edit?.worker && (
              <Alert severity="info">
                设置逐一应用于全部执行节点，各节点使用相同上限。
              </Alert>
            )}
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button disabled={saving} onClick={() => setEdit(null)}>
            取消
          </Button>
          <Button
            variant="contained"
            disabled={saving}
            onClick={() => void saveRate()}
            startIcon={
              saving ? (
                <CircularProgress size={16} color="inherit" />
              ) : undefined
            }
          >
            应用设置
          </Button>
        </DialogActions>
      </Dialog>
    </Stack>
  );
}
