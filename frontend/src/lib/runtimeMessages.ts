import { t } from "./i18n";

/**
 * Runtime messages the Master stores in English (internal/master task runtime).
 * Each source string below is a catalog entry; captured node names and causes
 * are inserted verbatim, and a cause that is itself a known message is
 * translated too.
 */
export const runtimeMessageRules: readonly {
  pattern: RegExp;
  message: string;
  values?: readonly ("node" | "reason")[];
}[] = [
  {
    pattern:
      /^Waiting for the execution lease of unavailable node (.+) to expire$/s,
    message: "等待不可用节点 {node} 的执行租约到期",
    values: ["node"],
  },
  {
    pattern:
      /^Waiting for scanning node (.+) to reconnect; existing child tasks remain assigned to their original nodes$/s,
    message: "等待扫描节点 {node} 重新连接；已分配的子任务仍保留在原节点",
    values: ["node"],
  },
  // Wording of Masters before v4.1.31, without the node name.
  {
    pattern: /^Waiting for an unavailable node's execution lease to expire$/,
    message: "等待不可用节点的执行租约到期",
  },
  {
    pattern:
      /^Waiting for the scanning node to reconnect; existing child tasks remain assigned to their original nodes$/,
    message: "等待扫描节点重新连接；已分配的子任务仍保留在原节点",
  },
  {
    pattern: /^Waiting for all task nodes to acknowledge recovery$/,
    message: "等待所有任务节点确认恢复",
  },
  {
    pattern: /^this node is permanently retired from the parent task$/,
    message: "该节点已永久退出此任务",
  },
  {
    pattern: /^task ownership on this node has been retired$/,
    message: "该节点在此任务中的执行权已收回",
  },
  {
    pattern: /^parent execution lease expired; a new lease ID is required$/,
    message: "节点的执行租约已过期，正在重新授权",
  },
  {
    pattern:
      /^Agent (.+) is unavailable; its assigned work remains on that node: (.+)$/s,
    message: "执行节点 {node} 不可用，已分配的工作仍保留在该节点：{reason}",
    values: ["node", "reason"],
  },
  {
    pattern: /^Agent (.+) is updating; its assigned work remains on that node$/,
    message: "执行节点 {node} 正在更新，已分配的工作仍保留在该节点",
    values: ["node"],
  },
  {
    pattern: /^Agent (.+) could not be fenced for removal: (.+)$/s,
    message: "无法隔离待移出的执行节点 {node}：{reason}",
    values: ["node", "reason"],
  },
  {
    pattern: /^Agent (.+) bandwidth: (.+)$/s,
    message: "执行节点 {node} 的限速未生效：{reason}",
    values: ["node", "reason"],
  },
  {
    pattern: /^Agent (.+) cannot verify the original shared directory: (.+)$/s,
    message: "执行节点 {node} 无法验证原共享目录：{reason}",
    values: ["node", "reason"],
  },
  {
    pattern: /^waiting for every task node to verify the shared directory$/,
    message: "等待所有任务节点验证共享目录",
  },
  {
    pattern: /^the original shared directory has not been verified$/,
    message: "原共享目录尚未验证",
  },
  {
    pattern: /^no shared directory verification node is available$/,
    message: "没有可用于验证共享目录的节点",
  },
  { pattern: /^Master is stopping$/, message: "Master 正在停止" },
  { pattern: /^context deadline exceeded$/, message: "请求超时" },
  { pattern: /^context canceled$/, message: "请求已取消" },
];

/** The translated text of a known runtime message, or undefined. */
export function knownRuntimeMessage(line: string): string | undefined {
  for (const rule of runtimeMessageRules) {
    const match = rule.pattern.exec(line);
    if (!match) continue;
    const values: Record<string, string> = {};
    rule.values?.forEach((name, index) => {
      const value = match[index + 1];
      values[name] =
        name === "reason" ? (knownRuntimeMessage(value) ?? value) : value;
    });
    return t(rule.message, values);
  }
  return undefined;
}

/** One problem per line, without repeats or blanks. */
export function runtimeMessageLines(message: string | null | undefined) {
  return [
    ...new Set(
      (message || "")
        .split("\n")
        .map((line) => line.trim())
        .filter(Boolean),
    ),
  ];
}

/**
 * Text for one runtime line: known messages are translated; any other English
 * text is introduced by a translated label so the alert never consists of an
 * untranslated sentence alone. Text that is already Chinese is shown as is.
 */
export function runtimeMessageText(line: string) {
  const known = knownRuntimeMessage(line);
  if (known !== undefined) return known;
  if (/\p{Script=Han}/u.test(line)) return line;
  return t("错误详情：{detail}", { detail: line });
}
