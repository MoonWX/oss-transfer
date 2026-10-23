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
  NodeDisplay & { address: string; unavailable?: boolean };
type Snapshot = {
  master: Software;
  agents: AgentSoftware[];
  latest?: { version: string; url?: string };
  available: boolean;
  checkedAt?: string;
  checking?: boolean;
  checkError?: string;
};
type Target = { address?: string; name: string; version: string };

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
  if (!info) return t("节点离线或当前版本不支持在线升级。");
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

export default function SoftwareUpdates() {
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
    if (!snapshot || firstCheck.current) return;
    firstCheck.current = true;
    if (
      (!snapshot.checkedAt ||
        Date.now() - Date.parse(snapshot.checkedAt) > 3600_000) &&
      !snapshot.checking &&
      !busyUpdate(snapshot.master.update)
    )
      void check();
  }, [snapshot, check]);
  async function start() {
    if (!target || starting) return;
    setStarting(true);
    setStartError("");
    try {
      const result = await api<{ data: Update }>(
        target.address
          ? `/api/master/software-updates/agents/${encodeURIComponent(target.address)}`
          : "/api/master/software-updates/master",
        { method: "POST", body: { version: target.version } },
      );
      generation.current++;
      setSnapshot(
        (current) =>
          current && {
            ...current,
            master: target.address
              ? current.master
              : { ...current.master, update: result.data },
            agents: current.agents.map((node) =>
              node.address === target.address
                ? { ...node, update: result.data }
                : node,
            ),
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
  return (
    <Stack gap={3}>
      <Stack
        direction="row"
        alignItems="center"
        justifyContent="space-between"
        gap={2}
      >
        <Typography variant="h6">{t("版本更新")}</Typography>
        <Button
          startIcon={<RefreshRounded />}
          disabled={checking || !!snapshot?.checking || masterBusy}
          onClick={() => void check()}
        >
          {checking || snapshot?.checking ? t("正在检测") : t("检测新版本")}
        </Button>
      </Stack>
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
      {master && (
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
                  disabled={!master.info?.supported || masterBusy || !!error}
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
          <Typography variant="h6" sx={{ mb: 1 }}>
            {t("执行节点")}
          </Typography>
          <Typography variant="body2" color="text.secondary" sx={{ mb: 2 }}>
            {t(
              "执行节点更新至当前 Master 版本。先更新 Master，再更新执行节点。",
            )}
          </Typography>
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
                      {busyUpdate(node.update) ? (
                        <Stack gap={1}>
                          <Typography variant="body2">
                            {phaseText(node.update)}
                          </Typography>
                          <LinearProgress />
                        </Stack>
                      ) : (
                        <Typography
                          variant="body2"
                          color={
                            node.update?.phase === "FAILED"
                              ? "error"
                              : "text.secondary"
                          }
                        >
                          {node.update?.phase === "FAILED"
                            ? node.update.error || phaseText(node.update)
                            : node.unavailable || !node.info?.supported
                              ? reason(node.info)
                              : node.info.version === version
                                ? t("版本一致")
                                : phaseText(node.update) || t("版本不一致")}
                        </Typography>
                      )}
                    </TableCell>
                    <TableCell align="right">
                      <Button
                        disabled={
                          !node.info?.supported ||
                          !newer(version, node.info?.version ?? "") ||
                          busyUpdate(node.update) ||
                          masterBusy ||
                          !!error
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
              "将安装 {version}，服务会短暂重启。安装器会备份当前版本，启动失败时回退。",
              { version: target?.version ?? "" },
            )}
          </Typography>
          {target?.address && (
            <Typography sx={{ mt: 1 }} color="text.secondary">
              {t("正在执行的子任务会中断，节点重新连接后自动恢复。")}
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
