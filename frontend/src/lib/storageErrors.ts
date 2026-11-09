import { t } from "./i18n";

/**
 * Common storage failures in raw error text (task failures, exception
 * records), classified like the Master's transferstore.ClassifyStorageMessage.
 * Connection checks are localized by the Master; this covers text the Master
 * stores as it came from the storage service.
 */
export type StorageErrorKind =
  | "dns"
  | "connection_refused"
  | "connection_reset"
  | "timeout"
  | "tls"
  | "access_denied"
  | "no_such_bucket"
  | "invalid_access_key"
  | "signature_mismatch"
  | "eof";

const errorCodes: [string, StorageErrorKind][] = [
  ["InvalidAccessKeyId", "invalid_access_key"],
  ["SignatureDoesNotMatch", "signature_mismatch"],
  ["NoSuchBucket", "no_such_bucket"],
  ["AccessDenied", "access_denied"],
  ["AllAccessDisabled", "access_denied"],
];

const transport: [RegExp, StorageErrorKind][] = [
  [
    /no such host|temporary failure in name resolution|name or service not known|nodename nor servname|lookup \S+.*server misbehaving/i,
    "dns",
  ],
  [/connection refused|actively refused/i, "connection_refused"],
  [/connection reset|forcibly closed|broken pipe/i, "connection_reset"],
  [
    /i\/o timeout|timeout awaiting|handshake timeout|client\.timeout exceeded|context deadline exceeded|connection timed out|operation timed out/i,
    "timeout",
  ],
  [
    /x509:|tls: failed to verify certificate|certificate signed by unknown authority|certificate is not valid|certificate has expired/i,
    "tls",
  ],
  [/(?:^|: )EOF\s*$|: EOF\b|unexpected EOF/, "eof"],
];

// Transport words alone are too general (a node's own request can time out):
// only text that comes from a storage request is classified.
const storageContext =
  /operation error S3|api error |dial tcp|lookup \S|https?:\/\/|request send failed/;

export function storageErrorKind(
  text: string | null | undefined,
): StorageErrorKind | undefined {
  if (!text) return undefined;
  for (const [code, kind] of errorCodes)
    if (
      text.includes(`api error ${code}`) ||
      text.includes(`<Code>${code}</Code>`)
    )
      return kind;
  if (!storageContext.test(text)) return undefined;
  for (const [pattern, kind] of transport) if (pattern.test(text)) return kind;
  return undefined;
}

/** The host a storage error names: the failed lookup, dial or request URL. */
export function storageErrorHost(text: string) {
  return (
    /lookup (\S+?)(?: on |: |$)/.exec(text)?.[1] ??
    /dial tcp (\[[^\]]+\]:\d+|[^\s:]+:\d+)/.exec(text)?.[1] ??
    /https?:\/\/([^/"\s?]+)/.exec(text)?.[1]
  );
}

/** A short localized cause and action for a raw storage error, if known. */
export function storageErrorSummary(
  text: string | null | undefined,
): string | undefined {
  const kind = storageErrorKind(text);
  if (!kind || !text) return undefined;
  const host = storageErrorHost(text) ?? "Endpoint";
  switch (kind) {
    case "dns":
      return t(
        "无法解析主机名 {host}。如果它是“存储桶名.Endpoint 主机名”，说明连接使用了 Virtual-hosted 寻址，自建 S3（MinIO、SeaweedFS、Ceph 等）请把存储连接的寻址方式改为 Path-style；否则请检查 Endpoint 地址和执行节点的 DNS 设置。",
        { host },
      );
    case "eof":
      return t(
        "{host} 关闭了连接（EOF）。请检查 Endpoint 地址、端口和协议（http 或 https）；自建 S3 请确认存储连接的寻址方式为 Path-style。",
        { host },
      );
    case "connection_refused":
      return t(
        "{host} 拒绝了连接，请检查 Endpoint 地址和端口，并确认存储服务已启动。",
        { host },
      );
    case "connection_reset":
      return t(
        "与 {host} 的连接被重置，请检查 Endpoint 协议（http 或 https）、端口以及代理或防火墙。",
        { host },
      );
    case "timeout":
      return t("连接 {host} 超时，请检查执行节点到 Endpoint 的网络和防火墙。", {
        host,
      });
    case "tls":
      return t(
        "无法验证 {host} 的 TLS 证书，请使用执行节点信任的证书，或确认 Endpoint 是否应使用 http。",
        { host },
      );
    case "access_denied":
      return t("访问被拒绝：凭据没有执行此操作的权限，请检查存储桶权限。");
    case "no_such_bucket":
      return t("存储桶不存在，请检查存储桶名称和 Endpoint。");
    case "invalid_access_key":
      return t("Access Key 不存在，请检查存储连接的 Access Key。");
    case "signature_mismatch":
      return t("签名不匹配，请检查存储连接的 Secret Key 和签名版本。");
  }
}
