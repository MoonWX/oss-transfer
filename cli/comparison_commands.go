package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Object comparisons run on one Agent. The Master records which node holds a
// comparison and serves its results; these commands use the same Master API as
// the console.

// comparisonRowStatuses are the stored row statuses accepted by --status, in
// display order (comparison.RowStatuses on the Master and Agents).
var comparisonRowStatuses = []string{"DIFFERENT", "ETAG_MISMATCH", "MISSING_TARGET", "EXTRA_TARGET", "CHANGED_DURING_COMPARE", "ERROR", "BOTH_MISSING"}

const (
	comparisonDefaultPageRows = 50
	comparisonMaxPageRows     = 1000
	// comparisonMaxIDBytes is the Master's limit for comparison IDs.
	comparisonMaxIDBytes = 128
	// A stored row holds at most three 1 KiB keys and a 512-byte error detail,
	// even fully escaped far below this line limit.
	comparisonMaxJSONLLine = 1 << 20
	comparisonMaxSummary   = 1 << 20
)

// comparisonExportIdle aborts an export that receives no bytes for this long.
// Exports have no total deadline: the Master streams every stored row, which
// can take longer than the report download limit, and aborts the connection
// itself when it cannot continue. Tests shorten it.
var comparisonExportIdle = 5 * time.Minute

// comparisonExportText names an export in download errors.
var comparisonExportText = downloadText{
	wrongType:   "服务器未返回比对导出文件（Content-Type: %s）",
	temporary:   "创建导出临时文件失败: %w",
	interrupted: "导出文件下载中断: %w",
	short:       "导出文件下载不完整：预期 %d 字节，实际 %d 字节",
	save:        "保存导出文件失败: %w",
	publish:     "发布导出文件失败: %w",
	idle:        "导出文件下载中断：%s 内未收到数据",
}

func runObjectAdmin(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	switch action {
	case "compare":
		return comparisonCompare(client, args)
	case "list":
		return comparisonList(client, args)
	case "status":
		return comparisonStatus(client, args)
	case "export":
		return comparisonExport(client, args)
	case "resume", "cancel", "delete":
		id, err := comparisonIDArgument(args)
		if err != nil {
			return nil, err
		}
		path := comparisonPath(id)
		switch action {
		case "cancel":
			return client.Post(path+"/cancel", nil)
		case "delete":
			return client.Delete(path)
		}
		result, err := client.Post(path+"/resume", nil)
		return comparisonRoute(client, id, result, err, cliErrorf("当前 Master 不支持继续比对，请先升级 Master"))
	}
	return nil, asUsageError(cliErrorf("未知 object 子命令: %s", action))
}

func comparisonPath(id string) string { return "/comparisons/" + url.PathEscape(id) }

func validComparisonID(id string) error {
	if id == "" || len(id) > comparisonMaxIDBytes || strings.ContainsAny(id, "/\x00") {
		return asUsageError(cliErrorf("无效比对 ID: %q", id))
	}
	return nil
}

func comparisonIDArgument(args []string) (string, error) {
	o, err := adminOptions(args, nil, nil, 1)
	if err != nil {
		return "", asUsageError(err)
	}
	return o.Positionals[0], validComparisonID(o.Positionals[0])
}

// comparisonRoute explains a 404 from a Master that predates a comparison
// route: its catch-all answers "Unknown API endpoint". When the comparison
// itself still exists (or the route needs none), the Master needs an update.
func comparisonRoute(client *ApiClient, id string, result map[string]interface{}, err error, upgrade error) (map[string]interface{}, error) {
	var problem *apiHTTPError
	if err == nil || !errors.As(err, &problem) || problem.Status != http.StatusNotFound {
		return result, err
	}
	if id != "" {
		if _, probeErr := client.Get(comparisonPath(id) + "?limit=1"); probeErr != nil {
			return result, err
		}
	}
	return nil, upgrade
}

func comparisonStatusSet(statuses []string) map[string]bool {
	set := make(map[string]bool, len(statuses))
	for _, status := range statuses {
		set[status] = true
	}
	return set
}

// comparisonStatusFilter parses --status: comma-separated stored row statuses
// in any case. It returns each once, in display order.
func comparisonStatusFilter(value string) ([]string, error) {
	known, requested := comparisonStatusSet(comparisonRowStatuses), map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		status := strings.ToUpper(strings.TrimSpace(part))
		if !known[status] {
			return nil, asUsageError(cliErrorf("未知比对结果 %q；可选 %s", strings.TrimSpace(part), strings.Join(comparisonRowStatuses, ",")))
		}
		requested[status] = true
	}
	statuses := []string{}
	for _, status := range comparisonRowStatuses {
		if requested[status] {
			statuses = append(statuses, status)
		}
	}
	return statuses, nil
}

type comparisonPageOptions struct {
	id       string
	after    int64
	limit    int
	statuses []string
}

func parseComparisonPageOptions(args []string) (comparisonPageOptions, error) {
	o, err := adminOptions(args, []string{"after", "limit", "status"}, nil, 1)
	if err != nil {
		return comparisonPageOptions{}, asUsageError(err)
	}
	options := comparisonPageOptions{id: o.Positionals[0]}
	if err := validComparisonID(options.id); err != nil {
		return options, err
	}
	if o.Has("after") {
		options.after, err = strconv.ParseInt(o.Value("after"), 10, 64)
		if err != nil || options.after < 0 {
			return options, asUsageError(cliErrorf("--after 必须是非负整数"))
		}
	}
	if options.limit, err = adminInteger(o, "limit", comparisonDefaultPageRows, 1, comparisonMaxPageRows); err != nil {
		return options, asUsageError(err)
	}
	if o.Has("status") {
		if options.statuses, err = comparisonStatusFilter(o.Value("status")); err != nil {
			return options, err
		}
	}
	return options, nil
}

func comparisonStatus(client *ApiClient, args []string) (map[string]interface{}, error) {
	options, err := parseComparisonPageOptions(args)
	if err != nil {
		return nil, err
	}
	query := url.Values{"after": {strconv.FormatInt(options.after, 10)}, "limit": {strconv.Itoa(options.limit)}}
	if len(options.statuses) > 0 {
		query.Set("status", strings.Join(options.statuses, ","))
	}
	result, err := client.Get(comparisonPath(options.id) + "?" + query.Encode())
	if err != nil {
		return result, err
	}
	if _, page := responseData(result)["items"]; !page || getString(responseData(result), "status") == "" {
		return nil, cliErrorf("服务器未返回有效的比对结果")
	}
	// Masters before result filters ignore the parameter and return every
	// row. Never present such a page as the filtered result.
	allowed := comparisonStatusSet(options.statuses)
	for _, row := range objectList(responseData(result)["items"]) {
		if len(allowed) > 0 && !allowed[getString(row, "status")] {
			return nil, cliErrorf("当前 Master 不支持按结果筛选，请先升级 Master")
		}
	}
	return result, nil
}

func comparisonList(client *ApiClient, args []string) (map[string]interface{}, error) {
	if _, err := adminOptions(args, nil, nil, 0); err != nil {
		return nil, asUsageError(err)
	}
	result, err := client.Get("/comparisons")
	if err != nil {
		return comparisonRoute(client, "", result, err, cliErrorf("当前 Master 不支持列出比对，请先升级 Master"))
	}
	// Every entry must be a comparison; "items": null is an empty list.
	items, listed := responseData(result)["items"]
	entries, isList := items.([]interface{})
	if !listed || items != nil && (!isList || len(objectList(entries)) != len(entries)) {
		return nil, cliErrorf("服务器未返回有效的比对列表")
	}
	return result, nil
}

func comparisonCompare(client *ApiClient, args []string) (map[string]interface{}, error) {
	fields := map[string]string{"source-bucket": "sourceBucket", "source-key": "sourceKey", "dest-bucket": "destBucket", "dest-key": "destKey"}
	o, err := adminOptions(args, []string{"source-bucket", "source-key", "dest-bucket", "dest-key", "source-prefix", "dest-prefix", "scope", "mode"}, nil, 0)
	if err != nil {
		return nil, asUsageError(err)
	}
	if o.Has("scope") || o.Has("mode") || o.Has("source-prefix") || o.Has("dest-prefix") {
		scope, mode := o.Value("scope"), o.Value("mode")
		if scope == "" {
			scope = "prefix"
			if o.Has("source-key") || o.Has("dest-key") {
				scope = "object"
			}
		}
		if mode == "" {
			mode = "metadata"
		}
		if scope != "prefix" && scope != "object" || mode != "metadata" && mode != "content" {
			return nil, asUsageError(cliErrorf("比对范围须为 prefix 或 object，模式须为 metadata 或 content"))
		}
		if scope == "prefix" && (o.Has("source-key") || o.Has("dest-key")) || scope == "object" && (o.Has("source-prefix") || o.Has("dest-prefix")) {
			return nil, asUsageError(cliErrorf("范围比对使用 --source-prefix/--dest-prefix，单对象使用 --source-key/--dest-key"))
		}
		source, dest := o.Value("source-prefix"), o.Value("dest-prefix")
		if scope == "object" {
			source, dest = o.Value("source-key"), o.Value("dest-key")
			if source == "" || dest == "" {
				return nil, asUsageError(cliErrorf("单对象比对须同时指定源和目标 Key"))
			}
		}
		for _, flag := range []string{"source-bucket", "dest-bucket"} {
			parts := strings.Split(o.Value(flag), "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, asUsageError(cliErrorf("--%s 必须使用 连接名称/Bucket 格式", flag))
			}
		}
		return client.Post("/comparisons", map[string]interface{}{"sourceBucket": o.Value("source-bucket"), "destBucket": o.Value("dest-bucket"), "sourcePrefix": source, "destPrefix": dest, "mode": mode, "scope": scope})
	}
	// Without range options keep the synchronous single-object metadata check.
	query := url.Values{}
	for flag, field := range fields {
		value := o.Value(flag)
		if value == "" {
			return nil, asUsageError(cliErrorf("对象比对需要 --%s", flag))
		}
		if strings.HasSuffix(flag, "bucket") {
			parts := strings.Split(value, "/")
			if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
				return nil, asUsageError(cliErrorf("--%s 必须使用 连接名称/Bucket 格式", flag))
			}
		}
		query.Set(field, value)
	}
	return client.Get("/object-metadata?" + query.Encode())
}

// comparisonExport downloads every stored row of a comparison. The file is
// verified completely before it is published, never replacing a file.
func comparisonExport(client *ApiClient, args []string) (map[string]interface{}, error) {
	o, err := adminOptions(args, []string{"output", "format", "status"}, nil, 1)
	if err != nil {
		return nil, asUsageError(err)
	}
	id, output := o.Positionals[0], o.Value("output")
	if err := validComparisonID(id); err != nil {
		return nil, err
	}
	if strings.TrimSpace(output) == "" {
		return nil, asUsageError(cliErrorf("导出需要 --output <文件路径>"))
	}
	format := "zip"
	if o.Has("format") {
		format = strings.ToLower(o.Value("format"))
	}
	if format != "zip" && format != "jsonl" {
		return nil, asUsageError(cliErrorf("--format 必须为 zip 或 jsonl"))
	}
	statuses := []string{}
	if o.Has("status") {
		if statuses, err = comparisonStatusFilter(o.Value("status")); err != nil {
			return nil, err
		}
	}
	query := url.Values{"format": {format}}
	if len(statuses) > 0 {
		query.Set("status", strings.Join(statuses, ","))
	}
	path := comparisonPath(id) + "/export?" + query.Encode()
	upgrade := cliErrorf("当前 Master 不支持导出比对结果，请先升级 Master")
	result := map[string]interface{}{"success": true, "id": id, "format": format, "output": output, "statusFilter": statuses}
	if format == "zip" {
		var archive comparisonArchive
		spec := comparisonDownload("application/zip", []string{"application/zip", "application/octet-stream", "application/x-zip-compressed", ""}, func(file string) (err error) {
			archive, err = inspectComparisonArchive(file)
			return err
		})
		if err := client.download(http.MethodGet, path, nil, output, spec); err != nil {
			_, err = comparisonRoute(client, id, nil, err, upgrade)
			return nil, err
		}
		complete, _ := archive.summary["complete"].(bool)
		result["summary"] = archive.summary
		result["complete"] = complete
		result["incompleteReasons"] = cliStringList(archive.summary["incompleteReasons"])
		result["exportedRows"] = archive.exportedRows
		result["retryKeys"] = archive.retryKeys
	} else {
		// JSONL holds rows only. Read the comparison first: a COMPLETED
		// comparison no longer changes, so its counts decide whether the rows
		// exported afterwards are complete.
		page, err := client.Get(comparisonPath(id) + "?limit=1")
		if err != nil {
			return nil, err
		}
		progress := responseData(page)
		if getString(progress, "status") == "" {
			return nil, cliErrorf("服务器未返回有效的比对结果")
		}
		var rows int64
		spec := comparisonDownload("application/x-ndjson", []string{"application/x-ndjson", "application/jsonl"}, func(file string) (err error) {
			rows, err = verifyComparisonJSONL(file, statuses)
			return err
		})
		if err := client.download(http.MethodGet, path, nil, output, spec); err != nil {
			_, err = comparisonRoute(client, id, nil, err, upgrade)
			return nil, err
		}
		complete, reasons := comparisonExportCompleteness(progress, rows, len(statuses) > 0)
		result["status"] = getString(progress, "status")
		result["storedRows"] = getInt64(progress, "rows")
		result["exportedRows"] = rows
		result["complete"] = complete
		result["incompleteReasons"] = reasons
	}
	result["message"] = client.text("比对结果已导出")
	return result, nil
}

func comparisonDownload(accept string, mediaTypes []string, verify func(string) error) downloadSpec {
	return downloadSpec{accept: accept, mediaTypes: mediaTypes, verify: verify, idle: comparisonExportIdle, text: comparisonExportText}
}

// comparisonExportCompleteness applies the Master's summary.json rule to a
// JSONL export: the comparison COMPLETED, every difference was stored and,
// without a filter, every stored row was exported.
func comparisonExportCompleteness(progress map[string]interface{}, exported int64, filtered bool) (bool, []string) {
	reasons := []string{}
	if status := getString(progress, "status"); status != "COMPLETED" {
		reasons = append(reasons, "NOT_COMPLETED:"+status)
	}
	if progress["rowsTruncated"] == true {
		reasons = append(reasons, "ROWS_TRUNCATED:"+comparisonTruncatedReason(progress))
	}
	rows := getInt64(progress, "rows")
	if rows != getInt64(progress, "compared")-getInt64(progress, "matched") || !filtered && exported != rows {
		reasons = append(reasons, "ROW_COUNT_MISMATCH")
	}
	return len(reasons) == 0, reasons
}

// comparisonTruncatedReason names why rows were not stored. Masters before
// truncation reasons only report the legacy fixed limit.
func comparisonTruncatedReason(data map[string]interface{}) string {
	reason := getString(data, "truncatedReason")
	if reason == "" && getInt64(data, "resultLimit") > 0 {
		reason = "LEGACY_LIMIT"
	}
	return reason
}

// comparisonArchive is what the CLI checked in an exported ZIP.
type comparisonArchive struct {
	summary      map[string]interface{}
	exportedRows int64
	retryKeys    int64
}

var comparisonArchiveEntries = []string{"differences.csv", "retry-source-keys.incr", "summary.json"}

// inspectComparisonArchive reads every entry, so archive/zip verifies each
// checksum, and checks that the CSV and retry list hold what summary.json
// reports.
func inspectComparisonArchive(path string) (comparisonArchive, error) {
	var result comparisonArchive
	archive, err := zip.OpenReader(path)
	if err != nil {
		return result, cliErrorf("下载内容不是完整的 ZIP 文件: %w", err)
	}
	defer archive.Close()
	seen := map[string]bool{}
	for _, entry := range archive.File {
		if seen[entry.Name] {
			return result, cliErrorf("导出文件包含重复的 %s", entry.Name)
		}
		seen[entry.Name] = true
		if err := inspectComparisonEntry(entry, &result); err != nil {
			return result, err
		}
	}
	for _, name := range comparisonArchiveEntries {
		if !seen[name] {
			return result, cliErrorf("导出文件缺少 %s", name)
		}
	}
	exported, exportedOK := comparisonInteger(result.summary["exportedRows"])
	retry, retryOK := comparisonInteger(result.summary["retryKeys"])
	if _, ok := result.summary["complete"].(bool); !ok || !exportedOK || !retryOK {
		return result, cliErrorf("summary.json 缺少导出统计")
	}
	if exported != result.exportedRows {
		return result, cliErrorf("导出文件不一致：differences.csv 有 %d 行，summary.json 记录 %d 行", result.exportedRows, exported)
	}
	if retry != result.retryKeys {
		return result, cliErrorf("导出文件不一致：retry-source-keys.incr 有 %d 个 Key，summary.json 记录 %d 个", result.retryKeys, retry)
	}
	return result, nil
}

func inspectComparisonEntry(entry *zip.File, result *comparisonArchive) error {
	reader, err := entry.Open()
	if err != nil {
		return cliErrorf("ZIP 文件校验失败: %w", err)
	}
	defer reader.Close()
	switch entry.Name {
	case "differences.csv":
		records := csv.NewReader(reader)
		records.ReuseRecord = true
		count := int64(-1) // the header
		for {
			if _, err := records.Read(); err == io.EOF {
				break
			} else if err != nil {
				return cliErrorf("ZIP 文件校验失败: %w", err)
			}
			count++
		}
		if count < 0 {
			return cliErrorf("导出文件缺少 %s", "differences.csv")
		}
		result.exportedRows = count
	case "retry-source-keys.incr":
		lines, err := countReaderLines(reader)
		if err != nil {
			return cliErrorf("ZIP 文件校验失败: %w", err)
		}
		result.retryKeys = lines
	case "summary.json":
		data, err := io.ReadAll(io.LimitReader(reader, comparisonMaxSummary+1))
		if err != nil {
			return cliErrorf("ZIP 文件校验失败: %w", err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var summary map[string]interface{}
		if len(data) > comparisonMaxSummary || decoder.Decode(&summary) != nil || summary == nil || decoder.More() {
			return cliErrorf("summary.json 无效")
		}
		result.summary = summary
	}
	// Reading to the end makes archive/zip verify the entry checksum.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return cliErrorf("ZIP 文件校验失败: %w", err)
	}
	return nil
}

// countReaderLines counts newline-terminated lines and a final unterminated one.
func countReaderLines(reader io.Reader) (int64, error) {
	buffer := make([]byte, 64*1024)
	var lines int64
	var last byte = '\n'
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			lines += int64(bytes.Count(buffer[:n], []byte{'\n'}))
			last = buffer[n-1]
		}
		if err == io.EOF {
			if last != '\n' {
				lines++
			}
			return lines, nil
		}
		if err != nil {
			return lines, err
		}
	}
}

func comparisonInteger(value interface{}) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		n, err := number.Int64()
		return n, err == nil
	case float64:
		return int64(number), number >= 0 && number == float64(int64(number))
	}
	return 0, false
}

// verifyComparisonJSONL checks an exported JSONL file: every line is a
// complete row, numbered in ascending order, with a requested status. A file
// that does not end with a newline was cut short.
func verifyComparisonJSONL(path string, statuses []string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, cliErrorf("读取导出文件失败: %w", err)
	}
	defer file.Close()
	allowed := comparisonStatusSet(statuses)
	reader := bufio.NewReaderSize(file, comparisonMaxJSONLLine)
	var rows, last int64
	for {
		line, err := reader.ReadSlice('\n')
		if err == io.EOF {
			if len(line) > 0 {
				return rows, cliErrorf("JSONL 导出不完整：最后一行缺少换行")
			}
			return rows, nil
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return rows, cliErrorf("读取导出文件失败: %w", err)
		}
		var row struct {
			Number int64  `json:"number"`
			Status string `json:"status"`
		}
		if err != nil || json.Unmarshal(line, &row) != nil || row.Number <= last || row.Status == "" || len(allowed) > 0 && !allowed[row.Status] {
			return rows, cliErrorf("JSONL 导出第 %d 行无效", rows+1)
		}
		last = row.Number
		rows++
	}
}
