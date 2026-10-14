import { t as tr, getLocale } from "./i18n";
export type RateType = "list" | "put" | "head" | "bandwidth";
export type TriggerRequest = {
  operation?: string | null;
  direction?: string | null;
  method?: string | null;
  taskId?: number | string | null;
  bucket?: string | null;
  key?: string | null;
  connectionAlias?: string | null;
  sourceBucket?: string | null;
  destinationBucket?: string | null;
  partNumber?: number | null;
  attempt?: number | null;
  statusCode?: number | null;
  errorCode?: string | null;
  requestId?: string | null;
  extendedRequestId?: string | null;
  message?: string | null;
  contextSource?: string | null;
  handlingStage?: string | null;
};
export type Adjustment = {
  workerIp?: string;
  detailsAvailable?: boolean | null;
  type: RateType;
  value: number;
  manual: boolean;
  timestamp?: number | null;
  timestampFormatted?: string | null;
  id?: string | null;
  processId?: string | null;
  sequence?: number | null;
  previousValue?: number | null;
  manualTarget?: number | null;
  previousManualTarget?: number | null;
  reasonCode?: string | null;
  consecutive503Count?: number | null;
  consecutive503Threshold?: number | null;
  factor?: number | null;
  minQps?: number | null;
  incidentId?: string | null;
  triggerRequest?: TriggerRequest | null;
  intervalMs?: number | null;
  increaseFactor?: number | null;
};
export type Rate = {
  limitQps?: number;
  currentQps?: number;
  limitBps?: number;
  currentBps?: number;
  manualLimitQps?: number;
  manualLimitBps?: number;
  configuredLimitQps?: number;
  configuredLimitBps?: number;
  limitSource?: "CONFIG" | "PERSISTED_MANUAL";
  state?: "NORMAL" | "REDUCED" | "RECOVERING" | "RECOVERED" | "UNLIMITED";
  lastAdjustment?: Adjustment | null;
};
export type AutoAdjust = {
  processId: string;
  observedAt: number;
  sequence: number;
  events: Adjustment[];
  intervalMs: number;
  increaseFactor: number;
  reduceFactor: number;
  minQps: number;
  consecutive503Threshold: number;
  consecutive503Count: number;
  last503At?: number | null;
  nextRecoveryAt?: number | null;
  measurementIntervalMs?: number;
  lastMeasurementAt?: number | null;
};
export type RateLimitSnapshot = Partial<Record<RateType, Rate>> & {
  autoAdjust?: AutoAdjust;
};
export type Worker = {
  success: boolean;
  message?: string;
  queueStatus?: Record<string, number>;
  executionStatus?: {
    activeScans: number;
    maxConcurrentScans: number;
    activeChildren: number;
    maxConcurrentChildren: number;
    unfinishedChildren: number;
    maxUnfinishedChildren: number;
  };
  localStorage?: {
    advertisedLocalRoots?: string[];
  };
  rateLimit?: RateLimitSnapshot;
  rateLimitHistory?: Adjustment[];
  taskStats?: Record<string, number>;
  executionTaskStats?: {
    scope: "CURRENT_EXECUTION_SUBTASKS";
    totalTasks: number;
    pendingTasks: number;
    runningTasks: number;
    listDoneTasks: number;
    completedTasks: number;
    partialFailedTasks: number;
    failedTasks: number;
    stoppedTasks: number;
    unknownTasks: number;
  } | null;
};
export type WorkerStatusResponse = { workers: Record<string, Worker> };

export function activeRates(worker: Worker): [RateType, Rate][] {
  if (!worker.success) return [];
  return (["list", "put", "head", "bandwidth"] as RateType[])
    .map((type): [RateType, Rate] => [type, worker.rateLimit?.[type] ?? {}])
    .filter(
      ([, rate]) => rate.state === "REDUCED" || rate.state === "RECOVERING",
    );
}

const numeric = (value: number) =>
  value.toLocaleString(getLocale(), { maximumFractionDigits: 2 });
export function eventChange(event: Adjustment) {
  const unit = event.type === "bandwidth" ? "Mbps" : "QPS";
  const factor = event.type === "bandwidth" ? 1_000_000 : 1;
  const value = (n: number) =>
    n <= 0 ? tr("不限速") : `${numeric(n / factor)} ${unit}`;
  return `${event.type === "bandwidth" ? tr("带宽") : event.type.toUpperCase()} ${event.previousValue == null ? "—" : value(event.previousValue)} → ${value(event.value)}`;
}

export function describeRateEvents(
  ip: string,
  events: Adjustment[],
  worker: Worker,
) {
  const latest = new Map<RateType, Adjustment>();
  for (const event of events) latest.set(event.type, event);
  const last = events[events.length - 1];
  const stillLimited = activeRates(worker).length > 0;
  const manuallyChanged = [...latest.keys()].some(
    (type) =>
      worker.rateLimit?.[type]?.lastAdjustment?.reasonCode === "MANUAL_SET",
  );
  if (manuallyChanged)
    return tr("{value1} 自动调节后，上限已手动更新。", { value1: ip });
  if (last.reasonCode === "RECOVERED" && !stillLimited)
    return tr("{value1} 已恢复到目标上限。", { value1: ip });
  if (last.reasonCode === "RECOVERY_STEP" || last.reasonCode === "RECOVERED")
    return tr("{value1} 正在恢复请求上限。", { value1: ip });
  return tr("{value1} 可能遭遇存储服务限速，已自动降低请求上限。", {
    value1: ip,
  });
}

export function adjustmentLink(workerIp: string, eventId: string) {
  return `/nodes/adjustments?${new URLSearchParams({ workerIp, eventId })}`;
}
