import { t as tr, getLocale } from "./i18n";
type RequestLimits = {
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
    ["LIST", limits.listQps],
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
