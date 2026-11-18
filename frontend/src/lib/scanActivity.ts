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

// The directory part of a running scan: what the scanner does in it, where
// it is, and, while it examines entries whose number is known, how far it
// got. percent is null while the directory is still being read.
export type ScanDirectoryView = {
  label: string;
  path: string;
  detail: string;
  percent: number | null;
  note?: string;
};

export type ScanAlert = { text: string; warning: boolean };

export type ScanActivityView =
  | { supported: false; message: string }
  | {
      supported: true;
      stale: boolean;
      asOf?: string;
      namesRead: string;
      examined: string;
      directory?: ScanDirectoryView;
      notes: string[];
      footnote: string;
      alerts: ScanAlert[];
    };

function formatMillis(milliseconds: number) {
  return formatDecimal(milliseconds, {
    maximumFractionDigits: milliseconds < 1 ? 2 : milliseconds < 10 ? 1 : 0,
  });
}

function directoryView(activity: ScanActivity): ScanDirectoryView | undefined {
  const directory = activity.directory;
  if (!directory) return undefined;
  const path = directory.key || t("源目录");
  const elapsed = t("用时 {d}", { d: formatElapsed(directory.elapsedMillis) });
  if (directory.reading && activity.mode === "STREAMING")
    return {
      label: t("正在边读边检查的目录"),
      path,
      detail: elapsed,
      percent: null,
      note: t("本目录已读取 {n} 个名称，已检查 {x} 项。", {
        n: formatNumber(directory.names),
        x: formatNumber(directory.examined),
      }),
    };
  if (directory.reading)
    return {
      label: t("正在读取名称的目录"),
      path,
      detail: elapsed,
      percent: null,
      note: t(
        "本目录已读取 {n} 个名称。名称不超过 4,096 个的目录读完后才开始检查和迁移其中的文件，超过后改为边读边检查。",
        { n: formatNumber(directory.names) },
      ),
    };
  if (directory.names <= 0) return undefined;
  const position = Math.min(Math.max(directory.position, 0), directory.names);
  return {
    label: t("正在检查的目录"),
    path,
    detail: t("第 {x} 项，共 {y} 项", {
      x: formatNumber(position),
      y: formatNumber(directory.names),
    }),
    percent: (position / directory.names) * 100,
  };
}

function scanAlerts(activity: ScanActivity): ScanAlert[] {
  const alerts: ScanAlert[] = [];
  const unanswered = Number(activity.oldestCallMillis) >= UNANSWERED_AFTER;
  if (unanswered)
    alerts.push({
      text: t("源存储已有 {d} 未响应", {
        d: formatElapsed(Number(activity.oldestCallMillis)),
      }),
      warning: true,
    });
  if (activity.waitingFor === "LOCAL_OPERATION_LIMIT")
    alerts.push({ text: t("正在等待本节点的本地文件操作限制"), warning: true });
  if (activity.waitingFor === "COMMIT")
    alerts.push({ text: t("正在保存扫描结果"), warning: false });
  if (
    !unanswered &&
    !activity.waitingFor &&
    Number(activity.idleMillis) >= IDLE_AFTER
  )
    alerts.push({
      text: t("已有 {d} 没有新进展", {
        d: formatElapsed(Number(activity.idleMillis)),
      }),
      warning: true,
    });
  return alerts;
}

/**
 * What the scan progress block shows, or null without a report: an absent
 * report must not read as a scan that has done nothing.
 */
export function scanActivityView(
  activity?: ScanActivity | null,
): ScanActivityView | null {
  if (!activity) return null;
  if (!hasCounts(activity))
    return {
      supported: false,
      message: t("扫描节点版本较旧，不提供实时扫描进度"),
    };
  const notes: string[] = [];
  if (Number(activity.alreadyFound) > 0)
    notes.push(
      t("其中 {n} 项此前已发现，未再次检查", {
        n: formatNumber(activity.alreadyFound),
      }),
    );
  if (Number(activity.vanished) > 0)
    notes.push(
      t("{n} 项在检查前已被删除，已跳过", {
        n: formatNumber(activity.vanished),
      }),
    );
  const stale = activity.state === "STALE";
  return {
    supported: true,
    stale,
    asOf:
      stale && activity.observedAt
        ? t("截至 {time}（{age}前）", {
            time: formatDate(activity.observedAt),
            age: formatElapsed(activity.ageMillis ?? 0),
          })
        : undefined,
    namesRead: formatNumber(activity.namesRead),
    examined: formatNumber(activity.examined),
    directory: directoryView(activity),
    notes,
    footnote:
      activity.lookupMeanMillis != null
        ? t(
            "条目查询平均耗时 {ms} 毫秒。计数从本轮扫描开始，扫描重启后从零计。",
            { ms: formatMillis(activity.lookupMeanMillis) },
          )
        : t("计数从本轮扫描开始，扫描重启后从零计。"),
    alerts: scanAlerts(activity),
  };
}

/**
 * The task list's tooltip for a running scan, one line each; empty when the
 * scanning node reports nothing. The list itself only says that it scans:
 * its found count grows as the scan commits objects.
 */
export function scanListTips(activity?: ScanActivity | null): string[] {
  const view = scanActivityView(activity);
  if (!view) return [];
  if (!view.supported) return [view.message];
  const tips = [
    t("已读取 {names} 个名称，已检查 {examined} 项", {
      names: view.namesRead,
      examined: view.examined,
    }),
  ];
  if (view.directory)
    tips.push(
      t("{label}：{path}，{detail}", {
        label: view.directory.label,
        path: view.directory.path,
        detail: view.directory.detail,
      }),
    );
  tips.push(...view.alerts.map((alert) => alert.text));
  if (view.asOf) tips.push(view.asOf);
  return tips;
}
