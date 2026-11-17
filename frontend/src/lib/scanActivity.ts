import { formatDate, formatNumber as formatDecimal, t } from "./i18n";
import { formatNumber } from "../components/Common";

// What the scanning node last reported about a running scan (Master's
// scanActivity). Totals count from zero in each run of the scan; durations
// were measured by the scanning node when it replied.
export type ScanActivity = {
  state: "LIVE" | "STALE" | "UNSUPPORTED";
  observedAt?: number;
  ageMillis?: number;
  mode?: "SORTED" | "STREAMING";
  namesRead?: number;
  examined?: number;
  alreadyFound?: number;
  vanished?: number;
  directory?: {
    key: string;
    reading: boolean;
    names: number;
    position: number;
    /** Entries examined in it; Masters before the field omit it. */
    examined?: number;
    elapsedMillis: number;
  } | null;
  lookupMeanMillis?: number | null;
  oldestCallMillis?: number;
  waitingFor?: "" | "LOCAL_OPERATION_LIMIT" | "COMMIT";
  idleMillis?: number;
};

// A source call is reported as unanswered, and a scan without progress as
// idle, only past these times: shorter waits are ordinary storage latency.
const UNANSWERED_AFTER = 5_000;
const IDLE_AFTER = 30_000;

/** A duration such as "3 min 5 s". */
export function formatElapsed(milliseconds: number) {
  const total = Math.max(0, Math.floor(milliseconds / 1000));
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (days) return t("{days} 天 {hours} 小时", { days, hours });
  if (hours) return t("{hours} 小时 {minutes} 分", { hours, minutes });
  if (minutes) return t("{minutes} 分 {seconds} 秒", { minutes, seconds });
  return t("{seconds} 秒", { seconds });
}

const hasCounts = (activity?: ScanActivity | null): activity is ScanActivity =>
  !!activity && activity.state !== "UNSUPPORTED";

/**
 * The task list's label for a running scan, or "" when the scanning node
 * reports nothing. Both totals only grow during a run, so the label moves
 * while a large directory is still being read.
 */
export function scanListLabel(activity?: ScanActivity | null): string {
  if (!activity) return "";
  if (!hasCounts(activity)) return t("扫描中");
  const values = {
    names: formatNumber(activity.namesRead),
    examined: formatNumber(activity.examined),
  };
  return activity.state === "STALE"
    ? t("扫描中 · 已读取 {names} · 已检查 {examined} · {age}前", {
        ...values,
        age: formatElapsed(activity.ageMillis ?? 0),
      })
    : t("扫描中 · 已读取 {names} · 已检查 {examined}", values);
}

function directoryName(key: string) {
  return key ? t("目录“{path}”", { path: key }) : t("源目录");
}

function formatMillis(milliseconds: number) {
  return formatDecimal(milliseconds, {
    maximumFractionDigits: milliseconds < 1 ? 2 : milliseconds < 10 ? 1 : 0,
  });
}

/** The lines of the scan progress block, in display order. */
export function scanActivityLines(activity?: ScanActivity | null): string[] {
  if (!activity) return [];
  if (!hasCounts(activity)) return [t("扫描节点版本较旧，不提供实时扫描进度")];
  const lines = [
    t(
      "本轮扫描已读取 {names} 个名称，已检查 {examined} 项（扫描重启后从零计）",
      {
        names: formatNumber(activity.namesRead),
        examined: formatNumber(activity.examined),
      },
    ),
  ];
  const directory = activity.directory;
  if (directory) {
    const dir = directoryName(directory.key);
    if (directory.reading && activity.mode === "STREAMING")
      lines.push(
        t("正在读取并检查{dir}：已读取 {n} 个名称，已检查 {x} 项，用时 {d}", {
          dir,
          n: formatNumber(directory.names),
          x: formatNumber(directory.examined),
          d: formatElapsed(directory.elapsedMillis),
        }),
      );
    else if (directory.reading)
      lines.push(
        t(
          "正在读取{dir}中的名称：已读取 {n} 个，用时 {d}。读完全部名称后才会开始检查和迁移其中的文件。",
          {
            dir,
            n: formatNumber(directory.names),
            d: formatElapsed(directory.elapsedMillis),
          },
        ),
      );
    else if (directory.names > 0)
      lines.push(
        t("正在检查{dir}：第 {x} 项，共 {y} 项", {
          dir,
          x: formatNumber(directory.position),
          y: formatNumber(directory.names),
        }),
      );
  }
  if (Number(activity.alreadyFound) > 0)
    lines.push(
      t("其中 {n} 项此前已发现，未再次检查", {
        n: formatNumber(activity.alreadyFound),
      }),
    );
  if (Number(activity.vanished) > 0)
    lines.push(
      t("{n} 项在检查前已被删除，已跳过", {
        n: formatNumber(activity.vanished),
      }),
    );
  if (activity.lookupMeanMillis != null)
    lines.push(
      t("条目查询平均耗时 {ms} 毫秒", {
        ms: formatMillis(activity.lookupMeanMillis),
      }),
    );
  const unanswered = Number(activity.oldestCallMillis) >= UNANSWERED_AFTER;
  if (unanswered)
    lines.push(
      t("源存储已有 {d} 未响应", {
        d: formatElapsed(Number(activity.oldestCallMillis)),
      }),
    );
  if (activity.waitingFor === "LOCAL_OPERATION_LIMIT")
    lines.push(t("正在等待本节点的本地文件操作限制"));
  if (activity.waitingFor === "COMMIT") lines.push(t("正在保存扫描结果"));
  if (
    !unanswered &&
    !activity.waitingFor &&
    Number(activity.idleMillis) >= IDLE_AFTER
  )
    lines.push(
      t("已有 {d} 没有新进展", {
        d: formatElapsed(Number(activity.idleMillis)),
      }),
    );
  if (activity.state === "STALE" && activity.observedAt)
    lines.push(
      t("扫描进度截至 {time}", { time: formatDate(activity.observedAt) }),
    );
  return lines;
}
