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
  | "eof"
  | "wrong_endpoint";

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

// S3 (PermanentRedirect) and Alibaba Cloud OSS (AccessDenied, EC
// 0003-00001403) say this when a bucket must be reached through another
// endpoint, usually the one of its region. OSS lists the buckets of every
// region, so this is not a permission problem. An OSS reply without a body,
// such as that to HEAD, says it by its EC alone, kept in the storage response
// diagnostic.
const endpointMismatch =
  /must be addressed using the specified endpoint|\[storage response: [^\]]*\bec=0003-00001403\b/i;
// The endpoint the reply named, kept in the storage response diagnostic.
const diagnosticEndpoint =
  /\[storage response: [^\]]*\bendpoint=([A-Za-z0-9._-]+)/;

// The SDK reports an error it models (NoSuchBucket) as "<code>: <message>"
// after the response's request IDs, without the "api error" prefix.
const modeledCode = /HostID: [^,]*, (\w+): /;

// Transport words alone are too general (a node's own request can time out):
// only text that comes from a storage request is classified.
const storageContext =
  /operation error S3|api error |dial tcp|lookup \S|https?:\/\/|request send failed/;

export function storageErrorKind(
  text: string | null | undefined,
): StorageErrorKind | undefined {
  if (!text) return undefined;
  if (endpointMismatch.test(text)) return "wrong_endpoint";
  const modeled = modeledCode.exec(text)?.[1];
  for (const [code, kind] of errorCodes)
    if (
      text.includes(`api error ${code}`) ||
      text.includes(`<Code>${code}</Code>`) ||
      modeled === code
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

// An object failure begins with its stage and quoted key (the Agent's
// execution.ObjectErrorCause); a file-system cause may precede them.
const objectFailureStage =
  /(?:^|: )(?:open source|read source|reconcile source|reconcile legacy result|check destination|write destination|verify write) "(?:[^"\\]|\\.)*": /;

/** An object failure's text without its stage and quoted key. */
export function objectErrorCause(text: string) {
  const match = objectFailureStage.exec(text);
  if (!match) return text;
  const rest = text.slice(match.index + match[0].length);
  return match.index === 0 ? rest : `${text.slice(0, match.index)}: ${rest}`;
}

// Codes that ask a client to slow down (transferstore's throttleErrorCodes).
const throttleCodes = new Set([
  "SlowDown",
  "Throttling",
  "ThrottlingException",
  "ThrottledException",
  "RequestThrottled",
  "RequestThrottledException",
  "RequestLimitExceeded",
  "TooManyRequests",
  "TooManyRequestsException",
]);

/**
 * A short localized cause for the error of a failed object, as the Master's
 * task summary classifies it. The object key is left out, so none of its
 * words is taken for the cause. Unlike a connection check, these requests
 * belong to a running task: a host that answered the node before and then
 * closed or reset connections had an outage, which the Agent retried ten
 * times. Only a host that never answered fails after the ordinary three
 * attempts, and only then is the Endpoint the likely cause.
 */
export function objectErrorSummary(
  text: string | null | undefined,
): string | undefined {
  if (!text) return undefined;
  const cause = objectErrorCause(text);
  const code = (/api error (\w+)/.exec(cause) ?? modeledCode.exec(cause))?.[1];
  const status = Number(/StatusCode: (\d{3})\b/.exec(cause)?.[1] ?? 0);
  if (code === "InvalidObjectState" && cause.includes("S3: GetObject"))
    return t("源对象为归档存储，需先解冻再重试。");
  if ((code && throttleCodes.has(code)) || status === 429)
    return t("存储服务持续限流，请调低任务的请求上限后重试。");
  const kind = storageErrorKind(cause);
  const host = storageErrorHost(cause) ?? "Endpoint";
  const attempts = Number(
    /exceeded maximum number of attempts, (\d+)/.exec(cause)?.[1] ?? 0,
  );
  if (kind === "eof" && !(attempts > 0 && attempts <= 3))
    return t(
      "{host} 在传输中关闭了连接（EOF）。可在存储恢复后重试失败对象；若反复出现，请检查存储前端代理或负载均衡的超时设置。",
      { host },
    );
  if (kind === "connection_reset")
    return t(
      "与 {host} 的连接在传输中被重置。可在存储恢复后重试失败对象；若反复出现，请检查存储前端代理、负载均衡或防火墙的设置。",
      { host },
    );
  return storageErrorSummary(cause);
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
    case "wrong_endpoint": {
      const endpoint = diagnosticEndpoint.exec(text)?.[1];
      return endpoint
        ? t(
            "存储桶需要通过 Endpoint {endpoint} 访问，这通常是该存储桶所在地域的 Endpoint。如需迁移此存储桶，请使用该 Endpoint 创建存储连接。",
            { endpoint },
          )
        : t(
            "存储桶需要通过另一个 Endpoint 访问，通常是该存储桶所在地域的 Endpoint。如需迁移此存储桶，请使用该地域的 Endpoint 创建存储连接。",
          );
    }
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
