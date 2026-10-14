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
  const scope = limits.scope === "PER_NODE" ? "每节点" : "任务总计";
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
        `${name} ${value!.toLocaleString("zh-CN", { maximumFractionDigits: 6 })}`,
    );
  if (configured.length)
    rows.push([`请求上限（${scope}）`, `${configured.join(" · ")} 次/秒`]);
  if (limits.bandwidthBitsPerSecond && limits.bandwidthBitsPerSecond > 0) {
    rows.push([
      `带宽上限（${scope}）`,
      `${(limits.bandwidthBitsPerSecond / 1e6).toLocaleString("zh-CN", { maximumFractionDigits: 6 })} Mbps`,
    ]);
  }
  return rows;
}
