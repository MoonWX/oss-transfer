import { t as tr } from "./i18n";
import { mapDestinationKey, type OssKeyMapping } from "./ossKeyMapping";

export type ConflictPolicy = "SKIP" | "OVERWRITE" | "OVERWRITE_CHANGED";

export const conflictPolicies: ConflictPolicy[] = [
  "SKIP",
  "OVERWRITE_CHANGED",
  "OVERWRITE",
];

export function isConflictPolicy(value: unknown): value is ConflictPolicy {
  return conflictPolicies.includes(value as ConflictPolicy);
}

/** The policy a task row stores; older rows only carry `overwrite`. */
export function taskConflictPolicy(task: Record<string, any>): string | null {
  if (typeof task.conflictPolicy === "string" && task.conflictPolicy)
    return task.conflictPolicy;
  if (task.overwrite == null) return null;
  return task.overwrite ? "OVERWRITE" : "SKIP";
}

/** The wizard's name for a policy; an unknown value is shown as is. */
export function conflictPolicyLabel(policy: string): string {
  switch (policy) {
    case "SKIP":
      return tr("跳过同名对象或文件");
    case "OVERWRITE":
      return tr("覆盖同名对象或文件");
    case "OVERWRITE_CHANGED":
      return tr("覆盖有变化的对象");
    default:
      return policy;
  }
}

/** What the selected policy does, for the wizard's helper text. */
export function conflictPolicyHelp(policy: ConflictPolicy): string {
  switch (policy) {
    case "SKIP":
      return tr(
        "目标已有同名对象或文件时直接跳过，不比较大小或内容；源端修改过的对象不会更新。",
      );
    case "OVERWRITE":
      return tr("覆盖会替换目标同名对象或文件，所有对象都会重新传输。");
    case "OVERWRITE_CHANGED":
      return tr(
        "目标不存在、大小不同、MD5 ETag 不同，或源端修改时间晚于目标时覆盖，其余跳过。不读取对象内容，分片上传的 ETag 不参与比较。",
      );
  }
}

/**
 * Object comparison prefilled with an OSS-to-OSS task's buckets and scope:
 * a single object compares its mapped key, otherwise the source prefix
 * (empty for the whole bucket) against the prefix it maps to. Local
 * transfers cannot be compared there and get null.
 */
export function taskComparisonLink(
  task: Record<string, any>,
  sourceObjectKey?: string,
  sourcePrefix?: string,
): string | null {
  if (task.direction && task.direction !== "OSS_TO_OSS") return null;
  if (!task.sourceBucket || !task.destBucket) return null;
  const bucket = (alias: unknown, name: string) =>
    alias ? `${alias}/${name}` : name;
  const mapping = task.keyMapping as OssKeyMapping | undefined;
  const query = new URLSearchParams({
    sourceBucket: bucket(task.sourceS3Alias, task.sourceBucket),
    destBucket: bucket(task.destS3Alias, task.destBucket),
  });
  if (sourceObjectKey) {
    query.set("sourceKey", sourceObjectKey);
    query.set("destKey", mapDestinationKey(sourceObjectKey, mapping));
  } else {
    const prefix = sourcePrefix || "";
    const destination = mapDestinationKey(prefix, mapping);
    if (prefix) query.set("sourcePrefix", prefix);
    if (destination) query.set("destPrefix", destination);
  }
  return `/tools?${query}`;
}
