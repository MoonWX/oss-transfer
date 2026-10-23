import { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  Box,
  Button,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  LinearProgress,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from "@mui/material";
import { RefreshRounded, SystemUpdateAltRounded } from "@mui/icons-material";
import { api } from "../lib/api";
import { t, useI18n } from "../lib/i18n";
import { displayNode, type NodeDisplay } from "../lib/nodeRegistry";

type SoftwareInfo = {
  role: string;
  version: string;
  commit?: string;
  platform?: string;
  supported: boolean;
  reason?: string;
};
type Update = {
  id?: string;
  version?: string;
  phase?: string;
  error?: string;
  updatedAt?: string;
};
type Software = { info: SoftwareInfo | null; update?: Update };
type AgentSoftware = Software &
  NodeDisplay & {
    address: string;
    unavailable?: boolean;
    upgradeReason?: string;
  };
type QueueItem = {
  address: string;
  phase: string;
  reason?: string;
  error?: string;
};
type UpdateQueue = {
  id?: string;
  version?: string;
  phase: string;
  items: QueueItem[];
};
type Snapshot = {
  queue?: UpdateQueue;
  master: Software;
  agents: AgentSoftware[];
  latest?: { version: string; url?: string };
  available: boolean;
  checkedAt?: string;
  checking?: boolean;
  checkError?: string;
};
type Target = {
  address?: string;
  allAgents?: boolean;
  name: string;
  version: string;
};

const busyUpdate = (update?: Update) =>
  ["QUEUED", "DOWNLOADING", "APPLYING"].includes(update?.phase ?? "");
function phaseText(update?: Update) {
  switch (update?.phase) {
    case "QUEUED":
      return t("更新已排队");
    case "DOWNLOADING":
      return t("正在下载更新");
    case "APPLYING":
      return t("正在安装更新");
    case "COMPLETED":
      return t("更新完成");
    case "FAILED":
      return t("更新失败");
    default:
      return "";
  }
}
function reason(info?: SoftwareInfo | null) {
  if (!info || info.reason === "OFFLINE")
    return t("节点离线，重新连接后可更新。");
  if (info.reason === "LEGACY")
    return t("此版本需先用安装脚本升级一次，之后可在这里更新。");
  if (info.reason === "CONTAINER" || info.reason === "container")
    return t("Docker 部署请更新镜像后重新创建容器。");
  if (!info.supported)
    return t("此安装方式暂不支持在线升级，请使用安装脚本升级。");
  return "";
}
function newer(target: string, current: string) {
  if (!/^\d+\.\d+\.\d+$/.test(target) || !/^\d+\.\d+\.\d+$/.test(current))
    return false;
  const a = target.split(".").map(Number),
    b = current.split(".").map(Number);
  for (let i = 0; i < 3; i++) if (a[i] !== b[i]) return a[i] > b[i];
  return false;
}

export default function SoftwareUpdates({
  agentsOnly = false,
}: {
  agentsOnly?: boolean;
}) {
  useI18n();
  const [snapshot, setSnapshot] = useState<Snapshot>();
  const [error, setError] = useState("");
  const [checkError, setCheckError] = useState("");
  const [checking, setChecking] = useState(false);
  const [target, setTarget] = useState<Target>();
  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState("");
  const inFlight = useRef(false);
  const mounted = useRef(true);
  const firstCheck = useRef(false);
  const generation = useRef(0);
  const refresh = useCallback(async (signal?: AbortSignal) => {
    if (inFlight.current) return;
    inFlight.current = true;
    const startedAt = generation.current;
    try {
      const result = await api<{ data: Snapshot }>(
        "/api/master/software-updates",
        { signal },
      );
      if (
        mounted.current &&
        !signal?.aborted &&
        startedAt === generation.current
      ) {
        setSnapshot(result.data);
        setError("");
      }
    } catch (e) {
      if (
        mounted.current &&
        !signal?.aborted &&
        startedAt === generation.current
      )
        setError(e instanceof Error ? e.message : String(e));
    } finally {
      inFlight.current = false;
    }
  }, []);
  const check = useCallback(async () => {
    setChecking(true);
    setCheckError("");
    try {
      await api("/api/master/software-updates/check", {
        method: "POST",
        body: {},
      });
      await refresh();
    } catch (e) {
      if (mounted.current)
        setCheckError(e instanceof Error ? e.message : String(e));
    } finally {
      if (mounted.current) setChecking(false);
    }
  }, [refresh]);
  useEffect(() => {
    mounted.current = true;
    const controller = new AbortController();
    void refresh(controller.signal);
    const timer = window.setInterval(() => {
      if (!document.hidden) void refresh(controller.signal);
    }, 3000);
    return () => {
      mounted.current = false;
      controller.abort();
      window.clearInterval(timer);
    };
  }, [refresh]);
  useEffect(() => {
    if (agentsOnly || !snapshot || firstCheck.current) return;
    firstCheck.current = true;
    if (
      (!snapshot.checkedAt ||
        Date.now() - Date.parse(snapshot.checkedAt) > 3600_000) &&
      !snapshot.checking &&
      !busyUpdate(snapshot.master.update)
    )
      void check();
  }, [snapshot, check, agentsOnly]);
  async function start() {
    if (!target || starting) return;
    setStarting(true);
    setStartError("");
    try {
      const agentUpdate = target.allAgents || !!target.address;
      const result = await api<{ data: Update | UpdateQueue }>(
        target.allAgents
          ? "/api/master/software-updates/agents"
          : target.address
            ? `/api/master/software-updates/agents/${encodeURIComponent(target.address)}`
            : "/api/master/software-updates/master",
        { method: "POST", body: { version: target.version } },
      );
      generation.current++;
      setSnapshot(
        (current) =>
          current && {
            ...current,
            ...(agentUpdate
              ? { queue: result.data as UpdateQueue }
              : {
                  master: { ...current.master, update: result.data as Update },
                }),
          },
      );
      setTarget(undefined);
      void refresh();
    } catch (e) {
      setStartError(e instanceof Error ? e.message : String(e));
    } finally {
      setStarting(false);
    }
  }
  const master = snapshot?.master;
  const masterBusy = busyUpdate(master?.update);
  const version = master?.info?.version ?? "";
  const queue = snapshot?.queue;
  const queueBusy = queue?.phase === "RUNNING";
  const eligible = (node: AgentSoftware) =>
    !!node.info?.supported &&
    !node.unavailable &&
    newer(version, node.info?.version ?? "") &&
    !busyUpdate(node.update);
  const eligibleCount = snapshot?.agents.filter(eligible).length ?? 0;
  const queuedItem = (node: AgentSoftware) =>
    queue?.items.find((item) => item.address === node.address);
  const nodeStatus = (node: AgentSoftware) => {
    const item = queuedItem(node);
    if (item?.phase === "UPDATING")
      return node.unavailable
        ? t("正在更新，等待节点重新连接…")
        : phaseText(node.update) || t("正在安装更新");
    if (item?.phase === "QUEUED") return t("更新已排队");
    if (item?.phase === "FAILED") return item.error || t("更新失败");
    if (item?.reason === "PREVIOUS_FAILED")
      return t("前一节点更新失败，尚未更新");
    if (node.unavailable) return t("节点离线，重新连接后可更新。");
    if (node.upgradeReason === "LEGACY")
      return t("此版本需先用安装脚本升级一次，之后可在这里更新。");
    if (!node.info?.supported) return reason(node.info);
    if (node.update?.phase === "FAILED")
      return node.update.error || t("更新失败");
    return node.info.version === version
      ? t("版本一致")
      : phaseText(node.update) || t("版本不一致");
  };
  return (
    <Stack gap={3}>
      {!agentsOnly && (
        <Stack
          direction="row"
          alignItems="center"
          justifyContent="space-between"
          gap={2}
        >
          <Typography variant="h6">{t("版本更新")}</Typography>
          <Button
            startIcon={<RefreshRounded />}
            disabled={
              checking || !!snapshot?.checking || masterBusy || queueBusy
            }
            onClick={() => void check()}
          >
            {checking || snapshot?.checking ? t("正在检测") : t("检测新版本")}
          </Button>
        </Stack>
      )}
      {!snapshot && !error && <LinearProgress aria-label={t("读取版本信息")} />}
      {error && (
        <Alert severity={masterBusy ? "info" : "warning"}>
          {masterBusy ? t("正在重新连接主节点…") : error}
        </Alert>
      )}
      {(checkError || snapshot?.checkError) && (
        <Alert severity="error">
          {checkError || t("版本检测失败，请重试。")}
        </Alert>
      )}
      {!agentsOnly && master && (
        <Paper variant="outlined" sx={{ p: 3 }}>
          <Stack
            direction={{ xs: "column", sm: "row" }}
            gap={2}
            justifyContent="space-between"
            alignItems={{ sm: "center" }}
          >
            <Box>
              <Typography variant="subtitle1">Master</Typography>
              <Typography variant="h5">{version || t("版本未知")}</Typography>
              <Typography color="text.secondary" variant="body2">
                {master.info?.platform}
              </Typography>
            </Box>
            {snapshot?.available && snapshot.latest ? (
              <Stack gap={1} alignItems={{ sm: "flex-end" }}>
                <Typography>
                  {t("新版本 {version}", { version: snapshot.latest.version })}
                </Typography>
                <Button
                  variant="contained"
                  startIcon={<SystemUpdateAltRounded />}
                  disabled={
                    !master.info?.supported ||
                    masterBusy ||
                    queueBusy ||
                    !!error
                  }
                  onClick={() => {
                    setStartError("");
                    setTarget({
                      name: "Master",
                      version: snapshot.latest!.version,
                    });
                  }}
                >
                  {t("更新 Master")}
                </Button>
              </Stack>
            ) : (
              snapshot?.latest && (
                <Chip
                  variant="outlined"
                  label={t("已是最新版本")}
                  color="success"
                />
              )
            )}
          </Stack>
          {reason(master.info) && (
            <Typography sx={{ mt: 2 }} color="text.secondary" variant="body2">
              {reason(master.info)}
            </Typography>
          )}
          {(master.info?.reason === "CONTAINER" ||
            master.info?.reason === "container") && (
            <Box component="pre" sx={{ whiteSpace: "pre-wrap", fontSize: 13 }}>
              docker compose pull{`\n`}docker compose up -d
            </Box>
          )}
          {phaseText(master.update) && (
            <Stack gap={1} sx={{ mt: 2 }}>
              <Typography>{phaseText(master.update)}</Typography>
              {masterBusy && <LinearProgress />}
              {master.update?.error && (
                <Alert severity="error">{master.update.error}</Alert>
              )}
            </Stack>
          )}
        </Paper>
      )}
      {!!snapshot?.agents.length && (
        <Box>
          <Stack
            direction={{ xs: "column", sm: "row" }}
            justifyContent="space-between"
            gap={2}
            sx={{ mb: 2 }}
          >
            <Box>
              <Typography variant="h6">{t("执行节点更新")}</Typography>
              <Typography variant="body2" color="text.secondary">
                {t("更新至 Master {version}，逐台重启并恢复任务。", {
                  version,
                })}
              </Typography>
            </Box>
            <Button
              variant="contained"
              startIcon={<SystemUpdateAltRounded />}
              disabled={!eligibleCount || queueBusy || masterBusy || !!error}
              onClick={() => {
                setStartError("");
                setTarget({ allAgents: true, name: t("待升级节点"), version });
              }}
            >
              {t("更新全部节点")}
            </Button>
          </Stack>
          {queueBusy && (
            <Alert severity="info" sx={{ mb: 2 }}>
              {t("节点正在逐台更新，关闭页面不影响进度。")}
            </Alert>
          )}
          {queue?.phase === "FAILED" && (
            <Alert severity="error" sx={{ mb: 2 }}>
              {t("节点更新已停止，请处理失败原因后重试。")}
            </Alert>
          )}
          <TableContainer component={Paper} variant="outlined">
            <Table aria-label={t("节点版本")}>
              <TableHead>
                <TableRow>
                  <TableCell>{t("执行节点")}</TableCell>
                  <TableCell>{t("当前版本")}</TableCell>
                  <TableCell>{t("状态")}</TableCell>
                  <TableCell />
                </TableRow>
              </TableHead>
              <TableBody>
                {snapshot.agents.map((node) => (
                  <TableRow key={node.address}>
                    <TableCell>{displayNode(node.address, node)}</TableCell>
                    <TableCell>{node.info?.version || t("版本未知")}</TableCell>
                    <TableCell>
                      <Stack gap={1}>
                        <Typography
                          variant="body2"
                          color={
                            queuedItem(node)?.phase === "FAILED" ||
                            node.update?.phase === "FAILED"
                              ? "error"
                              : "text.secondary"
                          }
                        >
                          {nodeStatus(node)}
                        </Typography>
                        {(queuedItem(node)?.phase === "UPDATING" ||
                          busyUpdate(node.update)) && <LinearProgress />}
                      </Stack>
                    </TableCell>
                    <TableCell align="right">
                      <Button
                        disabled={
                          !eligible(node) || queueBusy || masterBusy || !!error
                        }
                        onClick={() => {
                          setStartError("");
                          setTarget({
                            address: node.address,
                            name: displayNode(node.address, node),
                            version,
                          });
                        }}
                      >
                        {t("更新")}
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableContainer>
        </Box>
      )}
      {snapshot && !snapshot.agents.length && agentsOnly && (
        <Typography color="text.secondary">{t("暂无执行节点")}</Typography>
      )}
      <Dialog
        open={!!target}
        onClose={() => !starting && setTarget(undefined)}
        fullWidth
        maxWidth="xs"
      >
        <DialogTitle>
          {t("更新 {name}", { name: target?.name ?? "" })}
        </DialogTitle>
        <DialogContent>
          <Typography>
            {t(
              "将安装 {version}，服务会重启。安装器会备份当前版本，启动失败时回退。",
              { version: target?.version ?? "" },
            )}
          </Typography>
          {(target?.address || target?.allAgents) && (
            <Typography sx={{ mt: 1 }} color="text.secondary">
              {t("正在执行的子任务会中断，节点重新连接后自动恢复。")}
            </Typography>
          )}
          {target?.allAgents && (
            <Typography sx={{ mt: 1 }} color="text.secondary">
              {t(
                "共 {count} 个可升级节点。前一台恢复后再更新下一台，失败时停止。",
                { count: eligibleCount },
              )}
            </Typography>
          )}
          {startError && (
            <Alert severity="error" sx={{ mt: 2 }}>
              {startError}
            </Alert>
          )}
        </DialogContent>
        <DialogActions>
          <Button disabled={starting} onClick={() => setTarget(undefined)}>
            {t("取消")}
          </Button>
          <Button
            variant="contained"
            disabled={starting}
            onClick={() => void start()}
          >
            {starting ? t("正在提交") : t("确认更新")}
          </Button>
        </DialogActions>
      </Dialog>
    </Stack>
  );
}
