import { t, useI18n, formatDate } from "../lib/i18n";
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
  InfoOutlined,
} from "@mui/icons-material";
import { api, useApi, useNotify } from "../lib/api";
import { useNodeLabels, useWorkerStatus } from "../lib/workerStatus";
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
import NodeDetailsDialog from "../components/NodeDetailsDialog";
import { EmptyState, PageHeader, formatNumber } from "../components/Common";
import {
  removeNode,
  saveNode,
  type NodeRegistry,
  type RegisteredNode,
  nodeName,
} from "../lib/nodeRegistry";
const queues = [
  { key: "list", label: "对象扫描" },
  { key: "put", label: "对象传输" },
  { key: "progress", label: "进度上报" },
  { key: "failedKey", label: "失败记录" },
  { key: "fallback", label: "降级记录" },
];
export default function NodesPage() {
  useI18n();
  const { refreshMs, refreshSeconds } = useRefreshPreference();
  const workers = useWorkerStatus();
  const nodeLabel = useNodeLabels();
  const registry = useApi<{
    data: NodeRegistry;
  }>("/api/master/nodes", refreshMs);
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
    children: string;
    objects: string;
    initialChildren?: number;
    initialObjects?: number;
    concurrencyEditable: boolean;
    online: boolean;
  } | null>(null);
  const [nodeDelete, setNodeDelete] = useState<RegisteredNode | null>(null);
  const [nodeSaving, setNodeSaving] = useState(false);
  const [nodeError, setNodeError] = useState<string | null>(null);
  const [inviteOpen, setInviteOpen] = useState(false);
  const [detailNode, setDetailNode] = useState<RegisteredNode | null>(null);
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
      message: t("节点未返回状态，请检查节点服务和网络。"),
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
    const worker = workers.data?.workers?.[node?.address ?? ""];
    const execution = worker?.executionStatus;
    setNodeEdit({
      original: node?.address ?? null,
      address: node?.address ?? "",
      label: node?.label ?? "",
      master: !!node?.master,
      agent: !!node?.agent,
      children: String(execution?.maxConcurrentChildren ?? ""),
      objects: String(execution?.objectsPerChild ?? ""),
      initialChildren: execution?.maxConcurrentChildren,
      initialObjects: execution?.objectsPerChild,
      concurrencyEditable:
        !!worker?.success && typeof execution?.objectsPerChild === "number",
      online: !!worker?.success,
    });
  };
  async function saveNodeEdit() {
    if (!nodeEdit) return;
    if (!nodeEdit.address.trim()) {
      setNodeError(t("请输入节点标识。"));
      return;
    }
    const children = Number(nodeEdit.children);
    const objects = Number(nodeEdit.objects);
    if (
      nodeEdit.concurrencyEditable &&
      (!Number.isInteger(children) ||
        children < 1 ||
        children > 64 ||
        !Number.isInteger(objects) ||
        objects < 1 ||
        objects > 32 ||
        children * objects > 256)
    ) {
      setNodeError(
        t("子任务并发为 1–64，对象并发为 1–32，两者乘积不能超过 256。"),
      );
      return;
    }
    setNodeSaving(true);
    setNodeError(null);
    try {
      await saveNode(nodeEdit.original, {
        address: nodeEdit.address.trim(),
        label: nodeEdit.label.trim(),
        ...(nodeEdit.concurrencyEditable &&
        (children !== nodeEdit.initialChildren ||
          objects !== nodeEdit.initialObjects)
          ? { maxConcurrentChildren: children, objectsPerChild: objects }
          : {}),
      });
      notify(
        nodeEdit.original ? t("节点设置已保存。") : t("节点已添加。"),
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
      notify(t("节点已移除。"), "success");
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
      : String(Math.max(0, target) / (type === "bandwidth" ? 1000000 : 1));
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
              value > 0 ? (type === "bandwidth" ? value / 1000000 : value) : 0,
            ),
    });
  };
  async function saveRate() {
    const qps = edit
      ? Number(edit.value) * (edit.type === "bandwidth" ? 1000000 : 1)
      : NaN;
    if (!edit || edit.value.trim() === "" || !Number.isFinite(qps) || qps < 0) {
      setSaveError(t("请输入大于或等于 0 的数值。"));
      return;
    }
    setSaving(true);
    setSaveError(null);
    try {
      await api(
        `/api/master/rate-limit/${edit.type}${edit.worker ? `/${encodeURIComponent(edit.worker)}` : ""}`,
        { method: "POST", body: { qps } },
      );
      notify(t("限速设置已提交，正在刷新节点状态。"), "success");
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
        title={t("执行节点")}
        description={t("查看节点状态和速率。")}
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
              {t("添加节点")}
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
              {t("调整限速")}
            </Button>
          </Stack>
        }
      />
      {workers.error && <Alert severity="error">{workers.error}</Alert>}
      {registry.error && (
        <Alert severity="error">
          {t("节点配置读取失败：{error}", { error: registry.error || "" })}
        </Alert>
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
              <Typography variant="h6">{t("节点列表")}</Typography>
              <Typography variant="body2" color="text.secondary">
                {workers.data
                  ? t("{value1} / {value2} 个节点正常响应", {
                      value1: reachable,
                      value2: entries.length,
                    })
                  : t("正在读取节点状态")}
              </Typography>
            </Box>
          </Stack>
          <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
            {workers.data && (
              <Chip
                size="small"
                label={
                  reachable === entries.length && entries.length > 0
                    ? t("节点连接正常")
                    : entries.length
                      ? t("部分节点不可达")
                      : t("尚未配置节点")
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
        aria-label={t("节点视图")}
        sx={{ borderBottom: 1, borderColor: "divider" }}
      >
        <Tab
          icon={<SpeedRounded fontSize="small" />}
          iconPosition="start"
          label={t("实时状态")}
        />
        <Tab
          icon={<HistoryRounded fontSize="small" />}
          iconPosition="start"
          label={t("限速历史")}
        />
      </Tabs>
      {workers.loading && !workers.data && <LinearProgress />}
      {tab === 0 && (
        <>
          {workers.data && entries.length === 0 && (
            <EmptyState
              title={t("暂无执行节点")}
              description={t("添加 Agent 后可查看节点状态。")}
            />
          )}
          <Box
            sx={{
              display: "grid",
              gridTemplateColumns: {
                xs: "minmax(0, 1fr)",
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
                      label: t("扫描并发"),
                      size: execution.activeScans,
                      capacity: execution.maxConcurrentScans,
                    },
                    {
                      key: "children",
                      label: t("子任务并发"),
                      size: execution.activeChildren,
                      capacity: execution.maxConcurrentChildren,
                    },
                    {
                      key: "unfinished",
                      label: t("未完成子任务"),
                      size: execution.unfinishedChildren,
                      capacity: execution.maxUnfinishedChildren,
                    },
                  ]
                : queues.map((queue) => ({
                    ...queue,
                    label: t(queue.label),
                    size: worker.queueStatus?.[`${queue.key}QueueSize`],
                    capacity: worker.queueStatus?.[`${queue.key}QueueCapacity`],
                  }));
              const visibleActivity = activity.filter(
                (
                  item,
                ): item is typeof item & {
                  size: number;
                  capacity: number;
                } =>
                  typeof item.size === "number" &&
                  Number.isFinite(item.size) &&
                  item.size >= 0 &&
                  typeof item.capacity === "number" &&
                  Number.isFinite(item.capacity) &&
                  item.capacity > 0,
              );
              return (
                <Paper
                  key={ip}
                  component="section"
                  aria-label={t("执行节点 {value1}", {
                    value1: nodeLabel(ip, registered ?? worker),
                  })}
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
                      <Typography
                        variant="h6"
                        sx={{ overflowWrap: "anywhere" }}
                      >
                        {nodeLabel(ip, registered ?? worker)}
                      </Typography>
                      <Typography variant="body2" color="text.secondary">
                        {registered?.master
                          ? t("主节点 · 执行节点")
                          : t("执行节点")}
                      </Typography>
                      {registered?.agent &&
                        !registered.online &&
                        registered.lastSeen && (
                          <Typography variant="caption" color="text.secondary">
                            {t("最后连接：{time}", {
                              time: formatDate(registered.lastSeen),
                            })}
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
                              ? t("在线")
                              : t("离线")
                            : worker.success
                              ? t("在线")
                              : t("无法连接")
                        }
                      />
                    </Stack>
                  </Stack>
                  {registered && (
                    <Stack
                      direction="row"
                      spacing={1}
                      flexWrap="wrap"
                      useFlexGap
                      sx={{ px: 2.5, pb: 2 }}
                    >
                      {registered.agent && (
                        <Button
                          size="small"
                          startIcon={<InfoOutlined />}
                          onClick={() => setDetailNode(registered)}
                        >
                          {t("节点详情")}
                        </Button>
                      )}
                      <Button
                        size="small"
                        startIcon={<EditOutlined />}
                        onClick={() => openNodeEditor(registered)}
                      >
                        {t("编辑节点")}
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
                          {t("移除节点")}
                        </Button>
                      )}
                    </Stack>
                  )}
                  {!worker.success ? (
                    <Alert severity="error" sx={{ mx: 2.5, mb: 2.5 }}>
                      {worker.message ||
                        t("节点未返回有效状态，请检查节点服务和网络。")}
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
                              {execution ? t("执行负载") : t("队列占用")}
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
                                          : t("{value1}队列占用", {
                                              value1: queue.label,
                                            })
                                      }
                                    />
                                  </Box>
                                );
                              })}
                            </Stack>
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
              {history.data.failedWorkers?.length
                ? t("限速历史不完整。未获取到节点 {nodes} 的记录。", {
                    nodes: history.data.failedWorkers
                      .map((ip) => nodeLabel(ip))
                      .join(", "),
                  })
                : t("限速历史不完整。部分节点未返回记录。")}
            </Alert>
          )}
          <Typography variant="body2" color="text.secondary">
            {t(
              "显示各节点最近 50 条记录。自动降速与恢复改变运行上限，不覆盖已保存目标。旧记录未保存的调整前值和原因显示为未知。",
            )}
          </Typography>
          {history.loading && !history.data && <LinearProgress />}
          {history.data?.adjustments?.length ? (
            <TableContainer component={Paper} variant="outlined">
              <Table size="small">
                <TableHead>
                  <TableRow>
                    <TableCell>{t("时间")}</TableCell>
                    <TableCell>{t("节点")}</TableCell>
                    <TableCell>{t("类型")}</TableCell>
                    <TableCell>{t("运行上限变化")}</TableCell>
                    <TableCell>{t("阶段")}</TableCell>
                    <TableCell>{t("依据")}</TableCell>
                    <TableCell>{t("详情")}</TableCell>
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
                          {item.timestamp != null
                            ? formatDate(item.timestamp)
                            : item.timestampFormatted || "—"}
                        </TableCell>
                        <TableCell sx={{ fontFamily: "monospace" }}>
                          {nodeLabel(item.workerIp, item)}
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
                              {t("当时目标：{limit}", {
                                limit: rateText(
                                  item.manualTarget,
                                  item.type === "bandwidth",
                                ),
                              })}
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
                              {t("查看详情")}
                            </Button>
                          ) : (
                            <Typography
                              variant="caption"
                              color="text.secondary"
                            >
                              {t("未记录详情")}
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
                    ? t("已响应节点暂无调整记录")
                    : t("暂无调整记录")
                }
                description={
                  history.data?.partial || history.data?.complete === false
                    ? t("其余节点的历史尚未获取，无法确认是否发生调节。")
                    : t("限速调整记录生成后自动显示。")
                }
              />
            )
          )}
        </Stack>
      )}
      {inviteOpen && (
        <AgentInvitationDialog
          open
          onClose={() => setInviteOpen(false)}
          onJoined={() => void refresh()}
        />
      )}
      {detailNode && (
        <NodeDetailsDialog
          node={detailNode}
          onClose={() => setDetailNode(null)}
        />
      )}
      <Dialog
        open={nodeEdit !== null}
        onClose={() => !nodeSaving && setNodeEdit(null)}
        fullWidth
        maxWidth="sm"
      >
        <DialogTitle>
          {nodeEdit?.original ? t("编辑节点") : t("添加节点")}
        </DialogTitle>
        <DialogContent>
          <Stack spacing={2.5} sx={{ pt: 1 }}>
            {nodeError && <Alert severity="error">{nodeError}</Alert>}
            {!nodeEdit?.agent && (
              <TextField
                label={nodeEdit?.agent ? t("节点标识") : t("节点地址")}
                required
                fullWidth
                value={nodeEdit?.address ?? ""}
                disabled={nodeSaving || !!nodeEdit?.master || !!nodeEdit?.agent}
                onChange={(event) =>
                  setNodeEdit(
                    (value) =>
                      value && { ...value, address: event.target.value },
                  )
                }
                helperText={
                  nodeEdit?.agent
                    ? undefined
                    : t("服务端口：{value1}", {
                        value1: registry.data?.data?.port ?? t("使用集群配置"),
                      })
                }
              />
            )}
            <TextField
              label={t("节点名称")}
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
            {nodeEdit?.agent &&
              (nodeEdit.concurrencyEditable ? (
                <Stack spacing={1.5}>
                  <Stack direction={{ xs: "column", sm: "row" }} spacing={2}>
                    <TextField
                      label={t("子任务并发")}
                      type="number"
                      fullWidth
                      value={nodeEdit.children}
                      disabled={nodeSaving}
                      slotProps={{ htmlInput: { min: 1, max: 64, step: 1 } }}
                      onChange={(event) =>
                        setNodeEdit(
                          (value) =>
                            value && { ...value, children: event.target.value },
                        )
                      }
                    />
                    <TextField
                      label={t("每个子任务的对象并发")}
                      type="number"
                      fullWidth
                      value={nodeEdit.objects}
                      disabled={nodeSaving}
                      slotProps={{ htmlInput: { min: 1, max: 32, step: 1 } }}
                      onChange={(event) =>
                        setNodeEdit(
                          (value) =>
                            value && { ...value, objects: event.target.value },
                        )
                      }
                    />
                  </Stack>
                  <Typography variant="body2" color="text.secondary">
                    {t("保存后立即生效；调低并发不会中断正在传输的对象。")}
                  </Typography>
                </Stack>
              ) : (
                <Typography variant="body2" color="text.secondary">
                  {nodeEdit.online
                    ? t("升级节点后可调整并发。")
                    : t("节点离线时无法调整并发。")}
                </Typography>
              ))}
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button disabled={nodeSaving} onClick={() => setNodeEdit(null)}>
            {t("取消")}
          </Button>
          <Button
            variant="contained"
            disabled={nodeSaving}
            onClick={() => void saveNodeEdit()}
          >
            {nodeSaving ? t("正在保存…") : t("保存节点")}
          </Button>
        </DialogActions>
      </Dialog>
      <Dialog
        open={nodeDelete !== null}
        onClose={() => !nodeSaving && setNodeDelete(null)}
        fullWidth
        maxWidth="sm"
      >
        <DialogTitle>
          {t("移除节点 {node}", {
            node: nodeDelete ? nodeName(nodeDelete) : "",
          })}
        </DialogTitle>
        <DialogContent>
          <Stack spacing={2}>
            {nodeError && <Alert severity="error">{nodeError}</Alert>}
            <Typography>
              {t("移除此执行节点，不删除节点上的文件、对象或数据库。")}
            </Typography>
            <Alert severity="info">
              {t("节点不可达、仍有任务记录或集群正在执行任务时，无法移除。")}
            </Alert>
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button disabled={nodeSaving} onClick={() => setNodeDelete(null)}>
            {t("取消")}
          </Button>
          <Button
            color="error"
            variant="contained"
            disabled={nodeSaving}
            onClick={() => void deleteSelectedNode()}
          >
            {nodeSaving ? t("正在检查…") : t("确认移除")}
          </Button>
        </DialogActions>
      </Dialog>
      <Dialog
        open={edit !== null}
        onClose={() => !saving && setEdit(null)}
        fullWidth
        maxWidth="sm"
      >
        <DialogTitle>{t("调整传输限速")}</DialogTitle>
        <DialogContent>
          <Stack spacing={2.5} sx={{ pt: 1 }}>
            {saveError && <Alert severity="error">{saveError}</Alert>}
            <TextField
              select
              fullWidth
              label={t("应用范围")}
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
              <MenuItem value="">{t("全部执行节点")}</MenuItem>
              {entries.map(([ip]) => (
                <MenuItem key={ip} value={ip}>
                  {nodeLabel(ip)}
                </MenuItem>
              ))}
            </TextField>
            <TextField
              select
              fullWidth
              label={t("限制类型")}
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
                  ? t("带宽上限（Mbps）")
                  : t("请求上限（次/秒）")
              }
              type="number"
              value={edit?.value ?? ""}
              onChange={(event) =>
                setEdit(
                  (value) => value && { ...value, value: event.target.value },
                )
              }
              slotProps={{ htmlInput: { min: 0, step: "any" } }}
              helperText={t(
                "修改目标上限，0 表示不限速。保存后立即应用，节点重启后仍生效；临时降速不覆盖该目标。",
              )}
            />
            {!edit?.worker && (
              <Alert severity="info">
                {t("设置逐一应用于全部执行节点，各节点使用相同上限。")}
              </Alert>
            )}
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button disabled={saving} onClick={() => setEdit(null)}>
            {t("取消")}
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
            {t("应用设置")}
          </Button>
        </DialogActions>
      </Dialog>
    </Stack>
  );
}
