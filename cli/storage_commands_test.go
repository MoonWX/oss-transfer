package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestStorageListShowsResolvedAddressingAndSignature(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		adminTestJSON(t, w, map[string]interface{}{"success": true, "configs": []interface{}{
			map[string]interface{}{"alias": "minio", "endpoint": "http://10.0.0.5:9000", "credentialsConfigured": true, "useV2SignerType": nil, "pathStyleEnable": nil, "pathStyleEffective": true},
			map[string]interface{}{"alias": "jd", "endpoint": "https://s3.cn-north-1.jdcloud-oss.com", "credentialsConfigured": true, "useV2SignerType": nil, "pathStyleEnable": nil, "pathStyleEffective": false},
			map[string]interface{}{"alias": "fixed", "endpoint": "https://s3.example.com", "credentialsConfigured": true, "useV2SignerType": true, "pathStyleEnable": true},
			map[string]interface{}{"alias": "old-master", "endpoint": "https://s3.example.com", "credentialsConfigured": true},
		}})
	}
	for locale, wants := range map[string][]string{
		"zh": {"签名版本", "寻址方式", "自动（Path-style）", "自动（Virtual-hosted）", "默认（V4）", "V2", "Path-style", "自动\n"},
		"en": {"Signature", "Addressing", "Automatic (Path-style)", "Automatic (Virtual-hosted)", "Default (V4)", "Automatic\n"},
		"ja": {"署名バージョン", "アドレス指定", "自動（Path-style）", "既定（V4）", "自動\n"},
	} {
		code, out, stderr := runMockCLI(t, []string{"--lang", locale, "storage", "list"}, handler)
		for _, want := range wants {
			if code != 0 || !strings.Contains(out, want) {
				t.Fatalf("%s: missing %q: %d %s %s", locale, want, code, out, stderr)
			}
		}
		if strings.Contains(out, "继承") || strings.Contains(out, "Inherit") {
			t.Fatalf("%s: unexplained inherit label: %s", locale, out)
		}
	}
}

func TestStorageTestReportsTheBucketLevelCheck(t *testing.T) {
	t.Setenv("CLI_TEST_ACCESS", "access")
	t.Setenv("CLI_TEST_SECRET", "secret")
	var query string
	handler := func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		if r.URL.Path != "/api/master/s3-config/scoped/buckets" {
			t.Fatalf("unexpected request %s", r.URL)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "buckets": []interface{}{"only-bucket"}, "pathStyle": true, "pathStyleAuto": true, "checkedBucket": "only-bucket"})
	}
	code, out, stderr := runMockCLI(t, []string{"storage", "test", "scoped", "--bucket", "only-bucket"}, handler)
	if code != 0 || query != "bucket=only-bucket" {
		t.Fatalf("%d %q %s %s", code, query, out, stderr)
	}
	for _, want := range []string{"存储桶（1 个）", "  only-bucket", "寻址方式: 自动（Path-style）", "存储桶级检查: 已列举存储桶 only-bucket 中的对象", "连接测试通过"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	code, out, stderr = runMockCLI(t, []string{"--lang", "en", "storage", "test", "scoped", "--json"}, handler)
	var result map[string]interface{}
	if code != 0 || json.Unmarshal([]byte(out), &result) != nil || result["checkedBucket"] != "only-bucket" || query != "" {
		t.Fatalf("json: %d %s %s", code, out, stderr)
	}
	// A bucket-scoped key: the listing is denied, which only warns.
	warned := func(w http.ResponseWriter, r *http.Request) {
		adminTestJSON(t, w, map[string]interface{}{"success": true, "buckets": []interface{}{}, "pathStyle": false, "pathStyleAuto": true,
			"warning": "该凭据无权列举存储桶（ListBuckets 被拒绝）", "warningDetail": "api error AccessDenied: raw"})
	}
	code, out, stderr = runMockCLI(t, []string{"storage", "test", "scoped"}, warned)
	if code != 0 || !strings.Contains(out, "警告: 该凭据无权列举存储桶") || !strings.Contains(out, "可用 --bucket 指定一个") || strings.Contains(out+stderr, "api error") {
		t.Fatalf("warning: %d %s %s", code, out, stderr)
	}
	code, out, stderr = runMockCLI(t, []string{"--verbose", "storage", "test", "scoped"}, warned)
	if code != 0 || !strings.Contains(stderr, "详情: api error AccessDenied: raw") {
		t.Fatalf("verbose warning: %d %s %s", code, out, stderr)
	}
	for _, args := range [][]string{{"storage", "test", "scoped", "--bucket", "a/b"}, {"storage", "add", "x", "--endpoint", "https://s3.example.com", "--access-key-env", "CLI_TEST_ACCESS", "--secret-key-env", "CLI_TEST_SECRET", "--path-style", "maybe"}} {
		if code, out, stderr := runMockCLI(t, args, func(http.ResponseWriter, *http.Request) { t.Fatal("usage error made a request") }); code != 2 {
			t.Fatalf("%v: %d %s %s", args, code, out, stderr)
		}
	}
}

func TestStorageFailureShowsOneSummaryAndDetailOnRequest(t *testing.T) {
	t.Setenv("CLI_TEST_ACCESS", "access")
	t.Setenv("CLI_TEST_SECRET", "secret")
	var body map[string]interface{}
	handler := func(w http.ResponseWriter, r *http.Request) {
		body = adminTestBody(t, r)
		w.WriteHeader(400)
		adminTestJSON(t, w, map[string]interface{}{"success": false,
			"message":       "存储验证失败：无法解析存储桶域名 src.seaweed。自建 S3（MinIO、SeaweedFS、Ceph 等）需要 Path-style：请把寻址方式改为 Path-style。",
			"detail":        `operation error S3: ListObjectsV2, Get "http://src.seaweed/?list-type=2": dial tcp: lookup src.seaweed: no such host`,
			"reasonCode":    "STORAGE_DNS",
			"pathStyleHint": true})
	}
	args := []string{"storage", "add", "seaweed", "--endpoint", "https://seaweed.example.com", "--access-key-env", "CLI_TEST_ACCESS", "--secret-key-env", "CLI_TEST_SECRET", "--path-style", "auto"}
	code, out, stderr := runMockCLI(t, args, handler)
	if code != 1 || out != "" || strings.Count(stderr, "无法解析存储桶域名") != 1 || strings.Contains(stderr, "no such host") ||
		!strings.Contains(stderr, "提示: 运行 oss-transfer-cli storage update seaweed --path-style true") || !strings.Contains(stderr, "--verbose") {
		t.Fatalf("summary: %d stdout=%q stderr=%q", code, out, stderr)
	}
	// The error comes first; where to find the raw detail follows it.
	if strings.Index(stderr, "错误:") > strings.Index(stderr, "--verbose") {
		t.Fatalf("hint before the error: %q", stderr)
	}
	if _, set := body["pathStyleEnable"]; !set || body["pathStyleEnable"] != nil {
		t.Fatalf("--path-style auto must clear the explicit choice: %#v", body)
	}
	code, _, stderr = runMockCLI(t, append([]string{"--verbose"}, args...), handler)
	if code != 1 || !strings.Contains(stderr, "详情: operation error S3: ListObjectsV2") {
		t.Fatalf("verbose: %d %q", code, stderr)
	}
	code, out, _ = runMockCLI(t, append(args, "--json"), handler)
	var result map[string]interface{}
	if code != 1 || json.Unmarshal([]byte(out), &result) != nil || result["reasonCode"] != "STORAGE_DNS" || !strings.Contains(result["detail"].(string), "no such host") {
		t.Fatalf("json: %d %s", code, out)
	}
}

func TestTaskShowExplainsStorageFailureReasons(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{`operation error S3: ListObjectsV2, exceeded maximum number of attempts, 3, https response error StatusCode: 0, RequestID: , HostID: , request send failed, Get "http://src-data.host.docker.internal:38333/?list-type=2": dial tcp: lookup src-data.host.docker.internal on 127.0.0.11:53: no such host`,
			"无法解析主机名 src-data.host.docker.internal。"},
		{`operation error S3: ListObjectsV2, exceeded maximum number of attempts, 3, request send failed, Get "http://src-bucket.ot-eval-native-s3:8333/?list-type=2": EOF`,
			"src-bucket.ot-eval-native-s3:8333 关闭了连接（EOF）"},
		{`operation error S3: ListBuckets, https response error StatusCode: 403, api error SignatureDoesNotMatch: signature`, "签名不匹配"},
		{`dial tcp 10.0.0.5:9000: connect: connection refused`, "10.0.0.5:9000 拒绝了连接"},
	} {
		var out strings.Builder
		renderTaskDetail(&out, map[string]interface{}{"taskIndex": 1, "status": "FAILED", "errorMessage": test.raw})
		if !strings.Contains(out.String(), "原因说明") || !strings.Contains(out.String(), test.want) || !strings.Contains(out.String(), test.raw) {
			t.Fatalf("%s\n%s", test.raw, out.String())
		}
	}
	for _, raw := range []string{"unexpected EOF", "AccessDenied: listing source bucket", "context deadline exceeded"} {
		var out strings.Builder
		renderTaskDetail(&out, map[string]interface{}{"taskIndex": 1, "status": "FAILED", "errorMessage": raw})
		if strings.Contains(out.String(), "原因说明") {
			t.Fatalf("%q was classified without storage context:\n%s", raw, out.String())
		}
	}
}
