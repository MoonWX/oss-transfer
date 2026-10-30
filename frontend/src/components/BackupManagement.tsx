import { useCallback, useEffect, useRef, useState } from "react";
import {
  Accordion,
  AccordionDetails,
  AccordionSummary,
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  LinearProgress,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  Typography,
} from "@mui/material";
import {
  DeleteSweepOutlined,
  ExpandMoreRounded,
  Inventory2Outlined,
  RefreshRounded,
} from "@mui/icons-material";
import { api, useNotify } from "../lib/api";
import { t, useI18n } from "../lib/i18n";
import { displayNode, type NodeDisplay } from "../lib/nodeRegistry";
import { formatBytes, formatNumber } from "./Common";

type Summary = {
  supported: boolean;
  reason?: "UNMANAGED" | "LEGACY" | "OFFLINE" | "BUSY" | "ERROR";
  count: number;
  bytes: number;
  reclaimableCount: number;
  reclaimableBytes: number;
};
type AgentSummary = Summary & NodeDisplay & { address: string };
type Snapshot = {
  master: Summary;
  agents: AgentSummary[];
};
type Target = Summary & { address?: string; name: string };

function unavailable(summary: Summary) {
  switch (summary.reason) {
    case "OFFLINE":
      return t("节点离线，无法读取备份。");
    case "LEGACY":
      return t("升级节点后可管理备份。");
    case "BUSY":
      return t("节点正在更新或清理备份，请稍后刷新。");
    case "ERROR":
      return t("读取失败，请重试。");
    default:
      return t("此安装未启用备份管理。");
  }
}

export default function BackupManagement({
  agentsOnly = false,
  updateBusy = false,
}: {
  agentsOnly?: boolean;
  updateBusy?: boolean;
}) {
  useI18n();
  const notify = useNotify();
  const [expanded, setExpanded] = useState(false);
  const [snapshot, setSnapshot] = useState<Snapshot>();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [target, setTarget] = useState<Target>();
  const [cleaning, setCleaning] = useState(false);
  const [cleanupError, setCleanupError] = useState("");
  const mounted = useRef(true);
  const loadingRef = useRef(false);
  const cleaningRef = useRef(false);
  const controller = useRef<AbortController | null>(null);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      controller.current?.abort();
    };
  }, []);

  const refresh = useCallback(async () => {
    if (loadingRef.current) return;
    loadingRef.current = true;
    controller.current = new AbortController();
    setLoading(true);
    try {
      const response = await api<{ data: Snapshot }>(
        "/api/master/software-updates/backups",
        {
          signal: controller.current.signal,
          timeoutMs: 120000,
        },
      );
      if (mounted.current) {
        setSnapshot(response.data);
        setTarget((current) => {
          if (!current) return current;
          const summary = current.address
            ? response.data.agents.find(
                (node) => node.address === current.address,
              )
            : response.data.master;
          return summary
            ? { ...current, ...summary }
            : { ...current, supported: false };
        });
        setError("");
      }
    } catch (e) {
      if (mounted.current && !controller.current.signal.aborted)
        setError(e instanceof Error ? e.message : String(e));
    } finally {
      loadingRef.current = false;
      if (mounted.current) setLoading(false);
    }
  }, []);

  async function clean() {
    if (!target || cleaningRef.current || updateBusy) return;
    cleaningRef.current = true;
    setCleaning(true);
    setCleanupError("");
    try {
      const response = await api<{
        data: { removedCount: number; freedBytes: number };
      }>(
        target.address
          ? `/api/master/software-updates/backups/agents/${encodeURIComponent(target.address)}/prune`
          : "/api/master/software-updates/backups/master/prune",
        { method: "POST", body: {} },
      );
      if (!mounted.current) return;
      setTarget(undefined);
      notify(
        t("已清理 {count} 份备份，释放 {size}。", {
          count: formatNumber(response.data.removedCount),
          size: formatBytes(response.data.freedBytes),
        }),
      );
      await refresh();
    } catch (e) {
      if (mounted.current) {
        setCleanupError(e instanceof Error ? e.message : String(e));
        await refresh();
      }
    } finally {
      cleaningRef.current = false;
      if (mounted.current) setCleaning(false);
    }
  }

  const nodes: Target[] = snapshot
    ? [
        ...(agentsOnly ? [] : [{ ...snapshot.master, name: "Master" }]),
        ...snapshot.agents.map((node) => ({
          ...node,
          name: displayNode(node.address, node),
        })),
      ]
    : [];

  return (
    <>
      <Accordion
        disableGutters
        elevation={0}
        expanded={expanded}
        onChange={(_, open) => {
          setExpanded(open);
          if (open && !snapshot) void refresh();
        }}
        sx={{
          border: "1px solid",
          borderColor: "divider",
          borderRadius: "16px !important",
          "&:before": { display: "none" },
        }}
      >
        <AccordionSummary expandIcon={<ExpandMoreRounded />} sx={{ px: 3 }}>
          <Stack direction="row" alignItems="center" gap={1.5}>
            <Inventory2Outlined color="action" />
            <Typography fontWeight={600}>{t("升级备份")}</Typography>
          </Stack>
        </AccordionSummary>
        <AccordionDetails sx={{ px: { xs: 2, sm: 3 }, pb: 3 }}>
          <Stack gap={2}>
            <Stack
              direction={{ xs: "column", sm: "row" }}
              alignItems={{ sm: "center" }}
              justifyContent="space-between"
              gap={1}
            >
              {snapshot && (
                <Typography variant="body2" color="text.secondary">
                  {t(
                    "升级成功后删除本次备份；失败或缺少完成标记的备份会保留。",
                  )}
                </Typography>
              )}
              <Button
                startIcon={<RefreshRounded />}
                aria-label={t("刷新备份")}
                disabled={loading || cleaning}
                onClick={() => void refresh()}
                sx={{
                  alignSelf: { xs: "flex-end", sm: "auto" },
                  flexShrink: 0,
                }}
              >
                {t("刷新")}
              </Button>
            </Stack>
            {loading && <LinearProgress aria-label={t("读取备份信息")} />}
            {error && <Alert severity="error">{error}</Alert>}
            {snapshot && (
              <TableContainer>
                <Table size="small" aria-label={t("升级备份")}>
                  <TableHead>
                    <TableRow>
                      <TableCell>{t("节点")}</TableCell>
                      <TableCell align="right">{t("备份数量")}</TableCell>
                      <TableCell align="right">{t("占用空间")}</TableCell>
                      <TableCell align="right">{t("可清理")}</TableCell>
                      <TableCell />
                    </TableRow>
                  </TableHead>
                  <TableBody>
                    {nodes.map((node) => (
                      <TableRow key={node.address ?? "master"}>
                        <TableCell sx={{ minWidth: 120 }}>
                          {node.name}
                        </TableCell>
                        {node.supported ? (
                          <>
                            <TableCell align="right">
                              {formatNumber(node.count)}
                            </TableCell>
                            <TableCell
                              align="right"
                              sx={{ whiteSpace: "nowrap" }}
                            >
                              {formatBytes(node.bytes)}
                            </TableCell>
                            <TableCell
                              align="right"
                              sx={{ whiteSpace: "nowrap" }}
                            >
                              {formatBytes(node.reclaimableBytes)}
                            </TableCell>
                          </>
                        ) : (
                          <TableCell colSpan={3}>
                            <Typography variant="body2" color="text.secondary">
                              {unavailable(node)}
                            </Typography>
                          </TableCell>
                        )}
                        <TableCell align="right">
                          <Button
                            startIcon={<DeleteSweepOutlined />}
                            disabled={
                              !node.supported ||
                              !node.reclaimableCount ||
                              loading ||
                              cleaning ||
                              updateBusy ||
                              !!error
                            }
                            onClick={() => {
                              setCleanupError("");
                              setTarget(node);
                            }}
                            sx={{ whiteSpace: "nowrap" }}
                          >
                            {t("清理备份")}
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                    {!nodes.length && (
                      <TableRow>
                        <TableCell colSpan={5}>{t("暂无执行节点")}</TableCell>
                      </TableRow>
                    )}
                  </TableBody>
                </Table>
              </TableContainer>
            )}
          </Stack>
        </AccordionDetails>
      </Accordion>
      <Dialog
        open={!!target}
        onClose={() => !cleaning && setTarget(undefined)}
        fullWidth
        maxWidth="xs"
      >
        <DialogTitle>
          {t("清理 {name} 的备份？", { name: target?.name ?? "" })}
        </DialogTitle>
        <DialogContent>
          <Typography>
            {t("将删除 {count} 份备份，释放约 {size}。", {
              count: formatNumber(target?.reclaimableCount),
              size: formatBytes(target?.reclaimableBytes),
            })}
          </Typography>
          {cleanupError && (
            <Alert severity="error" sx={{ mt: 2 }}>
              {cleanupError}
            </Alert>
          )}
        </DialogContent>
        <DialogActions>
          <Button disabled={cleaning} onClick={() => setTarget(undefined)}>
            {t("取消")}
          </Button>
          <Button
            variant="contained"
            disabled={
              cleaning ||
              updateBusy ||
              !!error ||
              !target?.supported ||
              !target.reclaimableCount
            }
            onClick={() => void clean()}
          >
            {cleaning ? t("正在清理") : t("确认清理")}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}
