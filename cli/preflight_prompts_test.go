package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// An Agent from before the write probe passes a download destination without
// checking that it can write there. The CLI must not report a plain pass.
func TestCreateDownloadSaysWhenWritingWasNotChecked(t *testing.T) {
	const unchecked = `{"success":true,"data":{"valid":true,"nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/data","valid":true,"pathType":"DIRECTORY","code":"LOCAL_PATH_WRITE_NOT_CHECKED","message":"执行节点版本较旧，未检查目录是否可写；升级该节点后重新预检即可检查"}]}}`
	client := creationTestClient(t, creationExchange{method: "POST", path: "/local-transfers/validate", response: unchecked})
	result, err := runTaskCreate(client, []string{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--validate-only"})
	if err != nil || result["submissionStatus"] != "validated" {
		t.Fatalf("%#v %v", result, err)
	}
	if message := getString(result, "message"); message != "路径预检通过，但执行节点 10.0.0.1 版本较旧，未检查目录是否可写；尚未创建迁移任务" {
		t.Fatalf("validated message %q", message)
	}

	var warnings bytes.Buffer
	client = creationTestClient(t, creationExchange{method: "POST", path: "/local-transfers/validate", response: unchecked},
		creationExchange{method: "POST", path: "/local-transfers", response: `{"success":true,"data":{"acceptedCount":1,"parentTaskIds":[42]}}`})
	client.errors = &warnings
	if result, err := runTaskCreate(client, []string{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data"}); err != nil || result["submissionStatus"] != "accepted" {
		t.Fatalf("%#v %v", result, err)
	}
	if !strings.Contains(warnings.String(), "警告：执行节点 10.0.0.1 版本较旧，未检查下载目录是否可写") {
		t.Fatalf("warnings %q", warnings.String())
	}

	// A destination the Agent proved writable passes as before.
	client = creationTestClient(t, creationExchange{method: "POST", path: "/local-transfers/validate", response: `{"success":true,"data":{"valid":true,"nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/data","valid":true,"pathType":"DIRECTORY","message":"目标目录可写，可用空间 1.0 TiB"}]}}`})
	result, err = runTaskCreate(client, []string{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--validate-only"})
	if err != nil || getString(result, "message") != "节点预检通过，尚未创建迁移任务" {
		t.Fatalf("%#v %v", result, err)
	}
}

// A least-privilege target key may write but not abort multipart uploads, and
// may be limited to one prefix. Neither makes the bucket "source only", and a
// denied abort does not fail the command.
func TestStorageTestPermissionScopeAndOptionalAbort(t *testing.T) {
	var bodies []map[string]interface{}
	handler := func(older bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/master/s3-config/team/permissions" {
				adminTestJSON(t, w, map[string]interface{}{"success": true, "buckets": []interface{}{"shared"}, "pathStyle": false, "checkedBucket": "shared"})
				return
			}
			body := adminTestBody(t, r)
			bodies = append(bodies, body)
			if older && body["prefix"] != nil {
				w.WriteHeader(http.StatusBadRequest)
				adminTestJSON(t, w, map[string]interface{}{"success": false, "message": "JSON 请求格式无效"})
				return
			}
			prefix, _ := body["prefix"].(string)
			probe := prefix + ".oss-transfer-permission-check/x"
			denied := map[string]interface{}{"status": "FAILED", "message": "访问被拒绝：凭据有效，但没有执行此请求的权限。"}
			with := func(row map[string]interface{}, values map[string]interface{}) map[string]interface{} {
				for key, value := range values {
					row[key] = value
				}
				return row
			}
			results := []interface{}{
				map[string]interface{}{"operation": "LIST", "status": "PASSED"},
				map[string]interface{}{"operation": "HEAD", "status": "PASSED", "key": "folder/"},
				map[string]interface{}{"operation": "GET", "status": "PASSED", "key": "folder/"},
			}
			response := map[string]interface{}{"success": true, "bucket": "shared", "write": true}
			if prefix == "" {
				results = append(results,
					with(map[string]interface{}{"operation": "PUT", "key": probe}, denied),
					map[string]interface{}{"operation": "HEAD_WRITTEN", "status": "SKIPPED", "reason": "PUT_FAILED"},
					with(map[string]interface{}{"operation": "MULTIPART", "key": probe + "-multipart"}, denied),
					map[string]interface{}{"operation": "MULTIPART_ABORT", "status": "SKIPPED", "reason": "MULTIPART_FAILED"})
			} else {
				results = append(results,
					map[string]interface{}{"operation": "PUT", "status": "PASSED", "key": probe},
					map[string]interface{}{"operation": "HEAD_WRITTEN", "status": "PASSED", "key": probe},
					map[string]interface{}{"operation": "MULTIPART", "status": "PASSED", "key": probe + "-multipart"},
					with(map[string]interface{}{"operation": "MULTIPART_ABORT", "key": probe + "-multipart"}, denied),
					map[string]interface{}{"operation": "DELETE_PROBE", "status": "PASSED", "key": probe})
				response["prefix"], response["leftoverUploadKey"], response["leftoverUploadId"] = prefix, probe+"-multipart", "upload-1"
			}
			response["results"] = results
			adminTestJSON(t, w, response)
		}
	}
	code, out, stderr := runMockCLI(t, []string{"storage", "test", "team", "--bucket", "shared", "--write"}, handler(false))
	if code != 1 || !strings.Contains(out, "结论：存储桶 shared 读取正常，但在存储桶根目录写入失败：可以作为迁移的源。如果密钥只能写入某个前缀，请加 --prefix 重新检查。") ||
		strings.Contains(out, "只能作为迁移的源") || !strings.Contains(out, "对象：folder/") || !strings.Contains(out, "[未通过] 分片上传（创建、上传分片）\n") ||
		!strings.Contains(out, "[跳过] 中止分片上传（AbortMultipartUpload，用于清理残留分片）：没有创建分片上传，无需中止") || !strings.Contains(stderr, "存储桶 shared：2 项权限检查未通过") {
		t.Fatalf("bucket root: %d %s %s", code, out, stderr)
	}
	code, out, stderr = runMockCLI(t, []string{"storage", "test", "team", "--bucket", "shared", "--write", "--prefix", "team-a/"}, handler(false))
	if code != 0 || !strings.Contains(out, "[未通过] 中止分片上传（AbortMultipartUpload，用于清理残留分片）") ||
		!strings.Contains(out, "结论：存储桶 shared 读取正常，也可以在前缀 team-a/ 下写入，可以作为迁移的源或写入此前缀的目标。") ||
		!strings.Contains(out, "注意：分片上传 team-a/.oss-transfer-permission-check/x-multipart（上传 ID：upload-1）未能中止") || !strings.Contains(out, "对象：team-a/.oss-transfer-permission-check/x") {
		t.Fatalf("prefix: %d %s %s", code, out, stderr)
	}
	if len(bodies) != 2 || bodies[0]["prefix"] != nil || bodies[1]["prefix"] != "team-a/" {
		t.Fatalf("requests %v", bodies)
	}
	code, out, stderr = runMockCLI(t, []string{"--lang", "en", "storage", "test", "team", "--bucket", "shared", "--write", "--prefix", "team-a/"}, handler(false))
	if code != 0 || !strings.Contains(out, "Result: bucket shared can be read and written below the prefix team-a/") || !strings.Contains(out, "could not be aborted") {
		t.Fatalf("prefix (en): %d %s %s", code, out, stderr)
	}
	// A Master from before the write prefix refuses the field.
	code, out, stderr = runMockCLI(t, []string{"storage", "test", "team", "--bucket", "shared", "--write", "--prefix", "team-a/"}, handler(true))
	if code == 0 || !strings.Contains(stderr, "较旧的 Master 不支持 --prefix") {
		t.Fatalf("older Master: %d %s %s", code, out, stderr)
	}
	for _, args := range [][]string{{"--bucket", "shared", "--prefix", "team-a/"}, {"--bucket", "shared", "--write", "--prefix", "bad\nprefix"}} {
		if code, _, stderr := runMockCLI(t, append([]string{"storage", "test", "team"}, args...), handler(false)); code != 2 || !strings.Contains(stderr, "--prefix") {
			t.Fatalf("%v: %d %s", args, code, stderr)
		}
	}
}

// An older Agent aborts within the multipart row, so that row keeps naming
// the abort.
func TestStorageTestNamesTheAbortOfAnOlderAgentsMultipartRow(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/master/s3-config/team/permissions" {
			adminTestJSON(t, w, map[string]interface{}{"success": true, "bucket": "shared", "write": true, "results": []interface{}{
				map[string]interface{}{"operation": "LIST", "status": "PASSED"},
				map[string]interface{}{"operation": "PUT", "status": "PASSED"},
				map[string]interface{}{"operation": "MULTIPART", "status": "FAILED", "message": "访问被拒绝"},
			}})
			return
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "buckets": []interface{}{"shared"}, "pathStyle": false, "checkedBucket": "shared"})
	}
	code, out, stderr := runMockCLI(t, []string{"storage", "test", "team", "--bucket", "shared", "--write"}, handler)
	if code != 1 || !strings.Contains(out, "[未通过] 分片上传（创建、上传分片、中止）") || !strings.Contains(out, "结论：存储桶 shared 有 1 项检查未通过。") {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
}
