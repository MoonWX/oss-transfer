package main

import (
	"io"
	"regexp"
	"strings"
)

// Common storage failures in raw error text (a task's failure reason), as the
// Master classifies them (transferstore.ClassifyStorageMessage). The raw text
// stays visible; the summary is printed before it.
var (
	storageErrorCodes = []struct{ code, kind string }{
		{"InvalidAccessKeyId", "invalid_access_key"}, {"SignatureDoesNotMatch", "signature_mismatch"},
		{"NoSuchBucket", "no_such_bucket"}, {"AccessDenied", "access_denied"}, {"AllAccessDisabled", "access_denied"},
	}
	storageTransportErrors = []struct {
		pattern *regexp.Regexp
		kind    string
	}{
		{regexp.MustCompile(`(?i)no such host|temporary failure in name resolution|name or service not known|nodename nor servname|lookup \S+.*server misbehaving`), "dns"},
		{regexp.MustCompile(`(?i)connection refused|actively refused`), "connection_refused"},
		{regexp.MustCompile(`(?i)connection reset|forcibly closed|broken pipe`), "connection_reset"},
		{regexp.MustCompile(`(?i)i/o timeout|timeout awaiting|handshake timeout|client\.timeout exceeded|context deadline exceeded|connection timed out|operation timed out`), "timeout"},
		{regexp.MustCompile(`(?i)x509:|tls: failed to verify certificate|certificate signed by unknown authority|certificate is not valid|certificate has expired`), "tls"},
		{regexp.MustCompile(`(?:^|: )EOF\s*$|: EOF\b|unexpected EOF`), "eof"},
	}
	// Transport words alone are too general; classify only storage requests.
	storageErrorContext = regexp.MustCompile(`operation error S3|api error |dial tcp|lookup \S|https?://|request send failed`)
	storageLookupHost   = regexp.MustCompile(`lookup (\S+?)(?: on |: |$)`)
	storageDialHost     = regexp.MustCompile(`dial tcp (\[[^\]]+\]:\d+|[^\s:]+:\d+)`)
	storageURLHost      = regexp.MustCompile(`https?://([^/"\s?]+)`)
)

func storageErrorKind(text string) string {
	for _, entry := range storageErrorCodes {
		if strings.Contains(text, "api error "+entry.code) || strings.Contains(text, "<Code>"+entry.code+"</Code>") {
			return entry.kind
		}
	}
	if !storageErrorContext.MatchString(text) {
		return ""
	}
	for _, entry := range storageTransportErrors {
		if entry.pattern.MatchString(text) {
			return entry.kind
		}
	}
	return ""
}

func storageErrorHost(text string) string {
	for _, pattern := range []*regexp.Regexp{storageLookupHost, storageDialHost, storageURLHost} {
		if match := pattern.FindStringSubmatch(text); match != nil {
			return match[1]
		}
	}
	return "Endpoint"
}

// storageErrorSummary is a short translated cause and action for a raw storage
// error, or "" when the text is not a common storage failure.
func storageErrorSummary(out io.Writer, text string) string {
	host := storageErrorHost(text)
	switch storageErrorKind(text) {
	case "dns":
		return cliTextf(out, "无法解析主机名 %s。如果它是“存储桶名.Endpoint 主机名”，自建 S3 请运行 storage update <连接名> --path-style true；否则请检查 Endpoint 和执行节点的 DNS 设置。", host)
	case "eof":
		return cliTextf(out, "%s 关闭了连接（EOF）。请检查 Endpoint 地址、端口和协议（http 或 https）；自建 S3 请确认连接使用 Path-style。", host)
	case "connection_refused":
		return cliTextf(out, "%s 拒绝了连接，请检查 Endpoint 地址和端口，并确认存储服务已启动。", host)
	case "connection_reset":
		return cliTextf(out, "与 %s 的连接被重置，请检查 Endpoint 协议（http 或 https）、端口以及代理或防火墙。", host)
	case "timeout":
		return cliTextf(out, "连接 %s 超时，请检查执行节点到 Endpoint 的网络和防火墙。", host)
	case "tls":
		return cliTextf(out, "无法验证 %s 的 TLS 证书，请使用执行节点信任的证书，或确认 Endpoint 是否应使用 http。", host)
	case "access_denied":
		return cliText(out, "访问被拒绝：凭据没有执行此操作的权限，请检查存储桶权限。")
	case "no_such_bucket":
		return cliText(out, "存储桶不存在，请检查存储桶名称和 Endpoint。")
	case "invalid_access_key":
		return cliText(out, "Access Key 不存在，请检查存储连接的 Access Key。")
	case "signature_mismatch":
		return cliText(out, "签名不匹配，请检查存储连接的 Secret Key 和签名版本。")
	}
	return ""
}
