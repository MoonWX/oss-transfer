import { t as tr, getLocale } from "./i18n";
export type RequestLimits = {
  scope?: string;
  listQps?: number;
  headQps?: number;
  putQps?: number;
  bandwidthBitsPerSecond?: number;
};

export function taskRequestLimitRows(
  limits?: RequestLimits,
): [string, string][] {
  if (!limits) return [];
  const scope = limits.scope === "PER_NODE" ? tr("每节点") : tr("任务总计");
  const rows: [string, string][] = [];
  const rates = [
    ["LIST / GET", limits.listQps],
    ["HEAD", limits.headQps],
    ["PUT", limits.putQps],
  ] as const;
  const configured = rates
    .filter(([, value]) => typeof value === "number" && value > 0)
    .map(
      ([name, value]) =>
        `${name} ${value!.toLocaleString(getLocale(), { maximumFractionDigits: 6 })}`,
    );
  if (configured.length)
    rows.push([
      tr("请求上限（{value1}）", { value1: scope }),
      tr("{value1} 次/秒", { value1: configured.join(" · ") }),
    ]);
  if (limits.bandwidthBitsPerSecond && limits.bandwidthBitsPerSecond > 0) {
    rows.push([
      tr("带宽上限（{value1}）", { value1: scope }),
      `${(limits.bandwidthBitsPerSecond / 1e6).toLocaleString(getLocale(), { maximumFractionDigits: 6 })} Mbps`,
    ]);
  }
  return rows;
}

export type TaskRequestLimitValues = {
  listQps: string;
  headQps: string;
  putQps: string;
};

export const emptyTaskRequestLimits = (): TaskRequestLimitValues => ({
  listQps: "",
  headQps: "",
  putQps: "",
});

export function parseTaskRequestQps(value: string): number | undefined {
  if (!value.trim()) return undefined;
  const parsed = Number(value);
  if (
    !Number.isFinite(parsed) ||
    parsed < 0 ||
    parsed > 1e9 ||
    (parsed > 0 && parsed < 1e-6)
  )
    throw new Error(tr("QPS 必须为 0 或 0.000001–1,000,000,000 之间的数值。"));
  return parsed;
}

export function parseTaskRequestLimits(
  values: TaskRequestLimitValues,
  scope = "CLUSTER",
): RequestLimits | undefined {
  const rates = {
    listQps: parseTaskRequestQps(values.listQps),
    headQps: parseTaskRequestQps(values.headQps),
    putQps: parseTaskRequestQps(values.putQps),
  };
  return Object.values(rates).every((value) => value === undefined)
    ? undefined
    : { scope, ...rates };
}

export function validateTaskRequestLimits(value: unknown): void {
  if (value == null) return;
  if (typeof value !== "object" || Array.isArray(value))
    throw new Error(tr("requestLimits 必须是对象。"));
  const limits = value as Record<string, unknown>;
  if (
    limits.scope != null &&
    !["", "CLUSTER", "PER_NODE"].includes(String(limits.scope))
  )
    throw new Error(tr("requestLimits.scope 必须为 CLUSTER 或 PER_NODE。"));
  for (const key of ["listQps", "headQps", "putQps"]) {
    const rate = limits[key];
    if (rate == null) continue;
    if (typeof rate !== "number")
      throw new Error(
        tr("QPS 必须为 0 或 0.000001–1,000,000,000 之间的数值。"),
      );
    parseTaskRequestQps(String(rate));
  }
}
