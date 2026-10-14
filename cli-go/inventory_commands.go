package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const inventoryBatchSize = 128

type inventoryFile struct {
	name, snapshot, digest string
	count                  int64
}

// Snapshot every local manifest before contacting the Master. This bounds RAM,
// avoids reopening a changing source during upload, and prevents a malformed
// later file from leaving earlier files submitted accidentally.
func snapshotInventories(ctx context.Context, directory string) ([]inventoryFile, func(), error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, nil, cliErrorf("读取本机清单目录: %w", err)
	}
	work, err := os.MkdirTemp("", "oss-transfer-inventory-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(work) }
	if err = protectSessionPath(work, true); err != nil {
		cleanup()
		return nil, nil, err
	}
	files := []inventoryFile{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".incr") {
			continue
		}
		if err = ctx.Err(); err != nil {
			break
		}
		if !utf8.ValidString(entry.Name()) || len(entry.Name()) > 255 || strings.ContainsAny(entry.Name(), "\x00\r\n") {
			err = cliErrorf("清单文件名无效")
			break
		}
		info, e := entry.Info()
		if e != nil {
			err = e
			break
		}
		if !info.Mode().IsRegular() {
			err = cliErrorf("清单须为普通文件，不能是链接或目录: %s", entry.Name())
			break
		}
		file := inventoryFile{name: entry.Name(), snapshot: filepath.Join(work, fmt.Sprintf("%d.keys", len(files)))}
		file.count, file.digest, err = snapshotInventory(ctx, filepath.Join(directory, entry.Name()), file.snapshot, info)
		if err != nil {
			err = cliErrorf("清单 %s: %w", entry.Name(), err)
			break
		}
		files = append(files, file)
	}
	if err == nil && len(files) == 0 {
		err = cliErrorf("本机清单目录中没有 .incr 文件")
	}
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return files, cleanup, nil
}

func snapshotInventory(ctx context.Context, path, snapshot string, expected os.FileInfo) (int64, string, error) {
	input, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(info, expected) {
		return 0, "", cliErrorf("读取前清单文件已变更")
	}
	output, err := os.OpenFile(snapshot, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, "", err
	}
	defer output.Close()
	if err := protectSessionPath(snapshot, false); err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	writer := bufio.NewWriter(io.MultiWriter(output, hash))
	var count, total int64
	err = readInventory(ctx, input, func(key string) error {
		if count >= 10_000_000 || total+int64(len(key)+1) > 1<<30 {
			return cliErrorf("单个清单最多 1000 万条 Key、规范化文本最多 1 GiB")
		}
		if _, err := writer.WriteString(key + "\n"); err != nil {
			return err
		}
		count++
		total += int64(len(key) + 1)
		return nil
	})
	if err != nil {
		return 0, "", err
	}
	if err := writer.Flush(); err != nil {
		return 0, "", err
	}
	after, err := input.Stat()
	if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return 0, "", cliErrorf("读取期间清单文件已变更")
	}
	if err := output.Close(); err != nil {
		return 0, "", err
	}
	return count, hex.EncodeToString(hash.Sum(nil)), nil
}

func readInventory(ctx context.Context, input io.Reader, emit func(string) error) error {
	scanner := bufio.NewScanner(input)
	// A valid key has at most 1,024 UTF-8 bytes, plus CR/LF. Oversized lines
	// fail without allocating memory proportional to an arbitrary input line.
	scanner.Buffer(make([]byte, 4096), 4096)
	line := 0
	for scanner.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return err
		}
		key := scanner.Text() // ScanLines removes CR only as part of a line ending.
		if strings.TrimSpace(key) == "" {
			continue
		}
		if len(key) > 1024 || !utf8.ValidString(key) || strings.ContainsAny(key, "\x00\r\n") {
			return cliErrorf("第 %d 行不是有效的 UTF-8 Key（最多 1024 字节，不能含 NUL 或换行）", line)
		}
		if err := emit(key); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return cliErrorf("清单读取失败（第 %d 行附近）: %w", line+1, err)
	}
	return ctx.Err()
}

func inventoryRequestID(rootID, name string) string {
	digest := sha256.Sum256([]byte(rootID + "\x00" + name))
	return "inventory-" + hex.EncodeToString(digest[:])
}

func runInventoryImport(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := parseCommandOptions(args, []string{"node", "request-id"}, []string{"overwrite"})
	if err != nil {
		return nil, err
	}
	if len(opts.Positionals) != 3 {
		return nil, cliErrorf("用法: import-inventory <源连接/桶> <目标连接/桶> <本机清单目录> [--node 节点] [--overwrite] [--request-id 标识]")
	}
	rootID, err := creationRequestIDOption(opts)
	if err != nil {
		return nil, err
	}
	sourceAlias, sourceBucket, err := createParseOssEndpoint(opts.Positionals[0])
	if err != nil {
		return nil, err
	}
	destAlias, destBucket, err := createParseOssEndpoint(opts.Positionals[1])
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	nodes := []string{}
	for _, value := range opts.Values["node"] {
		for _, node := range strings.Split(value, ",") {
			node = strings.TrimSpace(node)
			if err := adminNodeAddress(node); err != nil {
				return nil, err
			}
			if !seen[node] {
				seen[node] = true
				nodes = append(nodes, node)
			}
		}
	}
	sort.Strings(nodes)
	ctx := client.requestContext
	if ctx == nil {
		ctx = context.Background()
	}
	files, cleanup, err := snapshotInventories(ctx, opts.Positionals[2])
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if rootID == "" {
		rootID, err = newCreationRequestID()
		if err != nil {
			return nil, err
		}
	}
	items := []interface{}{}
	taskIDs := []interface{}{}
	result := map[string]interface{}{"success": false, "requestId": rootID, "fileCount": len(files), "taskIds": taskIDs, "files": items, "submissionStatus": "submitting"}
	for _, file := range files {
		item := map[string]interface{}{"name": file.name, "keyCount": file.count, "requestId": inventoryRequestID(rootID, file.name), "state": "SUBMITTING"}
		items = append(items, item)
		result["files"] = items
		body := map[string]interface{}{"sourceS3Alias": sourceAlias, "sourceBucket": sourceBucket, "destS3Alias": destAlias, "destBucket": destBucket, "overwrite": opts.Has("overwrite"), "name": file.name, "contentSha256": file.digest}
		if len(nodes) > 0 {
			body["eligibleWorkerNodes"] = nodes
		}
		id, taskID, callErr := uploadInventory(client, file, body, item)
		if id != "" {
			item["importId"] = id
		}
		if callErr != nil {
			item["state"] = "UNCONFIRMED"
			result["submissionStatus"], result["partial"] = "unconfirmed", len(taskIDs) > 0
			result["taskCount"] = len(taskIDs)
			return result, cliErrorf("清单 %s 提交未完成或结果未确认: %w；保留原文件和参数，添加 --request-id %s 重试", file.name, callErr, rootID)
		}
		item["state"], item["taskId"] = "FINALIZED", taskID
		taskIDs = append(taskIDs, taskID)
		result["taskIds"] = taskIDs
	}
	result["success"], result["submissionStatus"], result["taskCount"] = true, "accepted", len(taskIDs)
	result["message"] = client.text("清单已提交；每个文件对应一个主任务")
	return result, nil
}

func uploadInventory(client *ApiClient, file inventoryFile, body, item map[string]interface{}) (string, int64, error) {
	created, err := client.PostCreation("/inventory-imports", body, item["requestId"].(string))
	if err != nil {
		return "", 0, err
	}
	data := responseData(created)
	id, _ := data["id"].(string)
	if id == "" || len(id) > 128 {
		return "", 0, cliErrorf("服务端未返回有效清单编号")
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return "", 0, cliErrorf("服务端清单编号无效")
		}
	}
	if taskID := getInt(data, "taskId"); taskID > 0 && data["state"] == "FINALIZED" {
		return id, int64(taskID), nil
	}
	input, err := os.Open(file.snapshot)
	if err != nil {
		return id, 0, err
	}
	defer input.Close()
	ctx := client.requestContext
	if ctx == nil {
		ctx = context.Background()
	}
	batch := 0
	uniqueCount := int64(0)
	keys := make([]string, 0, inventoryBatchSize)
	flush := func() error {
		if len(keys) == 0 {
			return nil
		}
		batch++
		response, e := client.Put(fmt.Sprintf("/inventory-imports/%s/batches/%d", id, batch), map[string]interface{}{"keys": keys})
		if e == nil {
			count, valid := cliNumeric(responseData(response)["uniqueCount"])
			if !valid || math.IsNaN(count) || math.IsInf(count, 0) || count < 0 || count > 10_000_000 || math.Trunc(count) != count {
				e = cliErrorf("服务端未返回有效清单去重数量")
			} else {
				uniqueCount = int64(count)
				item["uploadedBatches"] = batch
			}
		}
		keys = keys[:0]
		return e
	}
	err = readInventory(ctx, input, func(key string) error {
		keys = append(keys, key)
		if len(keys) == inventoryBatchSize {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return id, 0, err
	}
	finalized, err := client.Post("/inventory-imports/"+id+"/finalize", map[string]interface{}{"batchCount": batch, "uniqueCount": uniqueCount})
	if err != nil {
		return id, 0, err
	}
	data = responseData(finalized)
	taskID := getInt(data, "taskId")
	if taskID <= 0 || data["state"] != "FINALIZED" {
		return id, 0, cliErrorf("服务端未确认清单已封存及对应主任务 ID")
	}
	return id, int64(taskID), nil
}
