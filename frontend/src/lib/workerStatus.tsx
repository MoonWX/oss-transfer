import { t, useI18n, getLocale } from "./i18n";
import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Alert, Button, Stack } from "@mui/material";
import { Link as RouterLink, useLocation } from "react-router-dom";
import { useApi, useNotify } from "./api";
import { useRefreshPreference } from "./refreshPreference";
import {
  activeRates,
  adjustmentLink,
  describeRateEvents,
  type WorkerStatusResponse,
} from "./rateStatus";
import {
  displayNode,
  resolveNodeDisplay,
  mergeNodeDisplay,
  type NodeDisplay,
  type NodeLabels,
  type NodeRegistry,
} from "./nodeRegistry";

type State = ReturnType<typeof useApi<WorkerStatusResponse>> & {
  receivedAt: number | null;
  now: number;
  stale: boolean;
  nodeLabel: NodeLabels;
};
const WorkerStatusContext = createContext<State | null>(null);

export function WorkerStatusProvider({
  children,
  enabled,
}: {
  children: ReactNode;
  enabled: boolean;
}) {
  const { refreshMs } = useRefreshPreference();
  const workers = useApi<WorkerStatusResponse>(
    enabled ? "/api/master/worker-status" : null,
    refreshMs,
  );
  const registry = useApi<{ data: NodeRegistry }>(
    enabled ? "/api/master/nodes" : null,
    refreshMs,
  );
  const nodeLabel = useMemo<NodeLabels>(() => {
    const directory = new Map<string, NodeDisplay>();
    for (const [identity, worker] of Object.entries(
      workers.data?.workers ?? {},
    ))
      directory.set(identity, worker);
    for (const node of registry.data?.data?.nodes ?? []) {
      directory.set(
        node.address,
        mergeNodeDisplay(node, directory.get(node.address)),
      );
      if (node.id) directory.set(node.id, directory.get(node.address)!);
    }
    return (identity, display) =>
      resolveNodeDisplay(identity, directory.get(identity ?? ""), display);
  }, [registry.data, workers.data]);
  const notify = useNotify();
  const seen = useRef(
    new Map<string, { processId: string; sequence: number }>(),
  );
  const [receivedAt, setReceivedAt] = useState<number | null>(null);
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  useEffect(() => {
    if (!workers.data) return;
    setReceivedAt(Date.now());
    const messages: string[] = [];
    let reduced = false;
    let recovering = false;
    for (const [ip, worker] of Object.entries(workers.data.workers ?? {})) {
      const snapshot = worker.success
        ? worker.rateLimit?.autoAdjust
        : undefined;
      if (!snapshot) continue;
      const prior = seen.current.get(ip);
      seen.current.set(ip, {
        processId: snapshot.processId,
        sequence: snapshot.sequence,
      });
      // Initial and restarted processes establish a baseline. Persistent state is
      // shown in the banner; old history must not replay as new notifications.
      if (!prior || prior.processId !== snapshot.processId) continue;
      const events = (snapshot.events ?? []).filter(
        (event) =>
          !event.manual &&
          event.sequence != null &&
          event.sequence > prior.sequence,
      );
      if (!events.length) continue;
      messages.push(describeRateEvents(nodeLabel(ip, worker), events, worker));
      reduced ||= activeRates(worker).some(
        ([, rate]) => rate.state === "REDUCED",
      );
      recovering ||= activeRates(worker).length > 0;
    }
    if (messages.length)
      notify(
        messages.join(getLocale() === "en-US" ? "; " : "；"),
        reduced ? "warning" : recovering ? "info" : "success",
      );
  }, [workers.data, notify, nodeLabel]);
  const stale =
    !!workers.error ||
    receivedAt === null ||
    now - receivedAt > Math.max(30000, refreshMs * 2);
  return (
    <WorkerStatusContext.Provider
      value={{ ...workers, receivedAt, now, stale, nodeLabel }}
    >
      {children}
    </WorkerStatusContext.Provider>
  );
}

export function useNodeLabels(): NodeLabels {
  return useContext(WorkerStatusContext)?.nodeLabel ?? displayNode;
}

export function useWorkerStatus() {
  const value = useContext(WorkerStatusContext);
  if (!value) throw new Error("Worker status provider is missing");
  return value;
}

export function RateAdjustmentBanner() {
  useI18n();
  const { data, stale, nodeLabel } = useWorkerStatus();
  const { refreshSeconds } = useRefreshPreference();
  const { pathname } = useLocation();
  // Node cards and the event detail page already show the relevant status.
  if (pathname === "/nodes" || pathname.startsWith("/nodes/")) return null;
  const active = Object.entries(data?.workers ?? {}).filter(
    ([, worker]) => activeRates(worker).length > 0,
  );
  if (!active.length) return null;
  const uncertain = stale || refreshSeconds === 0;
  return (
    <Stack spacing={1} sx={{ mb: 3 }}>
      {active.map(([ip, worker]) => {
        const rates = activeRates(worker);
        const reduced = rates.some(([, rate]) => rate.state === "REDUCED");
        const latest = rates
          .map(([, rate]) => rate.lastAdjustment)
          .filter((event) => event?.id)
          .sort(
            (a, b) =>
              (b?.timestamp ?? 0) - (a?.timestamp ?? 0) ||
              (b?.sequence ?? 0) - (a?.sequence ?? 0),
          )[0];
        return (
          <Alert
            key={ip}
            severity="warning"
            sx={{
              "& .MuiAlert-message": { minWidth: 0, overflowWrap: "anywhere" },
              "& .MuiAlert-action": { alignItems: "center" },
            }}
            action={
              <Button
                size="small"
                component={RouterLink}
                to={
                  latest?.id && latest.detailsAvailable !== false
                    ? adjustmentLink(ip, latest.id)
                    : "/nodes"
                }
                color="inherit"
              >
                {t("查看详情")}
              </Button>
            }
          >
            {uncertain
              ? t("{p1} 曾触发自动调节，当前状态待更新。", {
                  p1: nodeLabel(ip, worker),
                })
              : reduced
                ? t("{p1} 可能遭遇存储服务限速，已自动降低请求上限。", {
                    p1: nodeLabel(ip, worker),
                  })
                : t("{p1} 正在恢复请求上限。", { p1: nodeLabel(ip, worker) })}
          </Alert>
        );
      })}
    </Stack>
  );
}
