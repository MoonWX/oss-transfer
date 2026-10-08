export type OssSourceMode = "all" | "prefix" | "object";
export type OssKeyMappingMode =
  "PRESERVE" | "ADD_PREFIX" | "REPLACE_PREFIX" | "EXACT_KEY";
export type OssKeyMapping = {
  mode: OssKeyMappingMode;
  sourcePrefix?: string;
  targetPrefix?: string;
  targetKey?: string;
};

export const keyMappingLabels: Record<OssKeyMappingMode, string> = {
  PRESERVE: "保留原 Key",
  ADD_PREFIX: "添加目标前缀",
  REPLACE_PREFIX: "替换源前缀",
  EXACT_KEY: "指定目标 Key",
};

export function mappingModes(sourceMode: OssSourceMode): OssKeyMappingMode[] {
  return [
    "PRESERVE",
    "ADD_PREFIX",
    ...(sourceMode === "prefix" ? ["REPLACE_PREFIX" as const] : []),
    ...(sourceMode === "object" ? ["EXACT_KEY" as const] : []),
  ];
}

export function buildKeyMapping(
  sourceMode: OssSourceMode,
  sourceSelection: string,
  mode: OssKeyMappingMode,
  targetPrefix: string,
  targetKey: string,
): OssKeyMapping | undefined {
  if (!mappingModes(sourceMode).includes(mode))
    throw new Error("目标命名方式与迁移范围不匹配，请重新选择。");
  if (mode === "PRESERVE") return undefined;
  if (mode === "EXACT_KEY") {
    if (!targetKey) throw new Error("请填写目标对象 Key。");
    return { mode, targetKey };
  }
  if (mode === "ADD_PREFIX") {
    if (!targetPrefix) throw new Error("请填写目标对象前缀。");
    return { mode, targetPrefix };
  }
  if (!sourceSelection) throw new Error("替换源前缀需要填写源对象前缀。");
  return { mode, sourcePrefix: sourceSelection, targetPrefix };
}

export function mapDestinationKey(
  sourceKey: string,
  mapping?: OssKeyMapping | null,
): string {
  switch (mapping?.mode) {
    case "ADD_PREFIX": {
      const prefix = mapping.targetPrefix ?? "";
      return prefix + (prefix && !prefix.endsWith("/") ? "/" : "") + sourceKey;
    }
    case "REPLACE_PREFIX":
      return sourceKey.startsWith(mapping.sourcePrefix ?? "")
        ? (mapping.targetPrefix ?? "") +
            sourceKey.slice((mapping.sourcePrefix ?? "").length)
        : sourceKey;
    case "EXACT_KEY":
      return mapping.targetKey ?? "";
    default:
      return sourceKey;
  }
}

export function mappingExample(
  sourceMode: OssSourceMode,
  sourceSelection: string,
  mapping?: OssKeyMapping | null,
) {
  const sourceKey =
    sourceMode === "object"
      ? sourceSelection || "backups/example.txt"
      : sourceMode === "prefix"
        ? `${sourceSelection || "backups/"}example.txt`
        : "backups/example.txt";
  return `${JSON.stringify(sourceKey)} → ${JSON.stringify(mapDestinationKey(sourceKey, mapping))}`;
}

export function validateOssKeyMapping(
  row: Record<string, unknown>,
  kind: "buckets" | "tasks" = "buckets",
) {
  const optionalString = (value: unknown, field: string) => {
    if (value != null && typeof value !== "string")
      throw new Error(`${field} 必须是字符串。`);
  };
  optionalString(row.prefix, "prefix");
  optionalString(row.objectKey, "objectKey");
  const endpoint =
    row.sourceEndpoint &&
    typeof row.sourceEndpoint === "object" &&
    !Array.isArray(row.sourceEndpoint)
      ? (row.sourceEndpoint as Record<string, unknown>)
      : undefined;
  optionalString(endpoint?.prefix, "sourceEndpoint.prefix");
  optionalString(endpoint?.objectKey, "sourceEndpoint.objectKey");
  if (row.prefix && endpoint?.prefix && row.prefix !== endpoint.prefix)
    throw new Error("prefix 与 sourceEndpoint.prefix 必须完全一致。");
  if (
    row.objectKey &&
    endpoint?.objectKey &&
    row.objectKey !== endpoint.objectKey
  )
    throw new Error("objectKey 与 sourceEndpoint.objectKey 必须完全一致。");
  if (
    kind === "tasks" &&
    !row.prefix &&
    endpoint?.prefix &&
    endpoint.prefixFilterEnabled !== true
  )
    throw new Error(
      "子任务的 sourceEndpoint.prefix 需同时设置 prefixFilterEnabled:true，或改用顶层 prefix。",
    );
  const prefix = row.prefix || endpoint?.prefix;
  const objectKey = row.objectKey || endpoint?.objectKey;
  if (objectKey && prefix)
    throw new Error("objectKey 与 prefix 不能同时填写。");
  if (row.keyMapping == null) return;
  if (typeof row.keyMapping !== "object" || Array.isArray(row.keyMapping))
    throw new Error("keyMapping 必须是对象。");
  const mapping = row.keyMapping as Record<string, unknown>;
  const sourceMode = objectKey ? "object" : prefix ? "prefix" : "all";
  if (!mappingModes(sourceMode).includes(mapping.mode as OssKeyMappingMode))
    throw new Error("keyMapping.mode 与迁移范围不匹配或不受支持。");
  for (const field of ["sourcePrefix", "targetPrefix", "targetKey"])
    optionalString(mapping[field], `keyMapping.${field}`);
  if (mapping.mode === "ADD_PREFIX" && !mapping.targetPrefix)
    throw new Error("ADD_PREFIX 需要非空 targetPrefix。");
  if (mapping.mode === "EXACT_KEY" && !mapping.targetKey)
    throw new Error("EXACT_KEY 需要单个源 objectKey 和非空 targetKey。");
  if (mapping.mode === "REPLACE_PREFIX") {
    if (mapping.sourcePrefix !== prefix)
      throw new Error(
        "REPLACE_PREFIX 的 sourcePrefix 必须与源 prefix 完全一致。",
      );
  }
  const allowed =
    mapping.mode === "ADD_PREFIX"
      ? ["targetPrefix"]
      : mapping.mode === "REPLACE_PREFIX"
        ? ["sourcePrefix", "targetPrefix"]
        : mapping.mode === "EXACT_KEY"
          ? ["targetKey"]
          : [];
  for (const field of ["sourcePrefix", "targetPrefix", "targetKey"])
    if (mapping[field] && !allowed.includes(field))
      throw new Error(`${mapping.mode} 不使用 ${field}，请移除此字段。`);
}
