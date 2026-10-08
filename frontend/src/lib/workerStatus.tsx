import {
  createContext,
  useContext,
  useEffect,
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

type State = ReturnType<typeof useApi<WorkerStatusResponse>> & {
  receivedAt: number | null;
  now: number;
  stale: boolean;
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
      messages.push(describeRateEvents(ip, events, worker));
      reduced ||= activeRates(worker).some(
        ([, rate]) => rate.state === "REDUCED",
      );
      recovering ||= activeRates(worker).length > 0;
    }
    if (messages.length)
      notify(
        messages.join("；"),
        reduced ? "warning" : recovering ? "info" : "success",
      );
  }, [workers.data, notify]);
  const stale =
    !!workers.error ||
    receivedAt === null ||
    now - receivedAt > Math.max(30000, refreshMs * 2);
  return (
    <WorkerStatusContext.Provider
      value={{ ...workers, receivedAt, now, stale }}
    >
      {children}
    </WorkerStatusContext.Provider>
  );
}

export function useWorkerStatus() {
  const value = useContext(WorkerStatusContext);
  if (!value) throw new Error("Worker status provider is missing");
  return value;
}

export function RateAdjustmentBanner() {
  const { data, stale } = useWorkerStatus();
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
                查看详情
              </Button>
            }
          >
            {uncertain
              ? `${ip} 曾触发自动调节，当前状态待更新。`
              : reduced
                ? `${ip} 可能遭遇存储服务限速，已自动降低请求上限。`
                : `${ip} 正在恢复请求上限。`}
          </Alert>
        );
      })}
    </Stack>
  );
}
