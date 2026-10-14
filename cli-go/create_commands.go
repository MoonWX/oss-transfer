package main

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Creation uses the same three request shapes and endpoints as CreateTransfer.tsx.
func runTaskCreate(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := parseCommandOptions(args, []string{
		"node", "binding", "prefix", "object-key", "mapping", "target-prefix", "target-key",
		"target-relative-path", "conflict", "distribution", "date-after", "date-before", "bandwidth-mbps", "request-id",
	}, []string{"overwrite", "balanced", "validate-only"})
	if err != nil {
		return nil, err
	}
	requestID, err := creationRequestIDOption(opts)
	if err != nil {
		return nil, err
	}
	if len(opts.Positionals) == 0 {
		return nil, cliErrorf("用法: task create oss-to-oss <源连接/桶> <目标连接/桶>，或 task create oss-to-local|local-to-oss <连接/桶> --binding 节点IP=/绝对路径")
	}
	direction := strings.ToUpper(strings.ReplaceAll(opts.Positionals[0], "-", "_"))
	if direction != "OSS_TO_OSS" && direction != "OSS_TO_LOCAL" && direction != "LOCAL_TO_OSS" {
		return nil, cliErrorf("迁移方向须为 oss-to-oss、oss-to-local 或 local-to-oss")
	}
	want := 2
	if direction == "OSS_TO_OSS" {
		want = 3
	}
	if len(opts.Positionals) != want {
		return nil, cliErrorf("%s 需要 %d 个连接/桶参数", opts.Positionals[0], want-1)
	}
	for flag, values := range opts.Values {
		if flag != "node" && flag != "binding" && len(values) > 1 {
			return nil, cliErrorf("--%s 只能填写一次", flag)
		}
	}
	row := map[string]interface{}{"distributionMode": "affinity"}
	if opts.Has("distribution") {
		row["distributionMode"] = opts.Value("distribution")
	}
	if opts.Has("balanced") {
		if opts.Has("distribution") && opts.Value("distribution") != "balanced" {
			return nil, cliErrorf("--balanced 与 --distribution affinity 冲突")
		}
		row["distributionMode"] = "balanced"
	}
	if row["distributionMode"] != "affinity" && row["distributionMode"] != "balanced" {
		return nil, cliErrorf("--distribution 须为 affinity 或 balanced")
	}
	conflict := "SKIP"
	if opts.Has("conflict") {
		conflict = strings.ToUpper(opts.Value("conflict"))
	}
	if opts.Has("overwrite") {
		if opts.Has("conflict") && conflict != "OVERWRITE" {
			return nil, cliErrorf("--overwrite 与 --conflict skip 冲突")
		}
		conflict = "OVERWRITE"
	}
	if conflict != "SKIP" && conflict != "OVERWRITE" {
		return nil, cliErrorf("--conflict 须为 skip 或 overwrite")
	}
	if opts.Has("prefix") && opts.Has("object-key") {
		return nil, cliErrorf("--prefix 与 --object-key 不能同时填写")
	}
	for _, flag := range []string{"prefix", "object-key", "target-key"} {
		if opts.Has(flag) && opts.Value(flag) == "" {
			return nil, cliErrorf("--%s 不能为空", flag)
		}
	}
	for flag, field := range map[string]string{"date-after": "dateAfter", "date-before": "dateBefore"} {
		if opts.Has(flag) {
			row[field] = opts.Value(flag)
		}
	}
	if opts.Has("bandwidth-mbps") {
		limit, err := strconv.ParseFloat(opts.Value("bandwidth-mbps"), 64)
		if err != nil || math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 0.1 || limit > 1000000 {
			return nil, cliErrorf("任务带宽须为 0.1–1,000,000 Mbps")
		}
		row["taskBandwidthLimitMbps"] = limit
	}
	alias, bucket, err := createParseOssEndpoint(opts.Positionals[1])
	if err != nil {
		return nil, err
	}
	if direction == "OSS_TO_OSS" {
		if opts.Has("binding") || opts.Has("target-relative-path") || opts.Has("validate-only") {
			return nil, cliErrorf("--binding、--target-relative-path 和 --validate-only 仅适用于本地迁移")
		}
		destAlias, destBucket, err := createParseOssEndpoint(opts.Positionals[2])
		if err != nil {
			return nil, err
		}
		row["sourceS3Alias"], row["sourceBucket"] = alias, bucket
		row["destS3Alias"], row["destBucket"] = destAlias, destBucket
		row["overwrite"] = conflict == "OVERWRITE"
		createCopyOption(row, opts, "prefix", "prefix")
		createCopyOption(row, opts, "object-key", "objectKey")
		mode := "PRESERVE"
		if opts.Has("mapping") {
			mode = strings.ToUpper(strings.ReplaceAll(opts.Value("mapping"), "-", "_"))
		} else if opts.Has("target-prefix") || opts.Has("target-key") {
			return nil, cliErrorf("请用 --mapping 指定目标命名方式")
		}
		mapping := map[string]interface{}{"mode": mode}
		createCopyOption(mapping, opts, "target-prefix", "targetPrefix")
		createCopyOption(mapping, opts, "target-key", "targetKey")
		if mode == "REPLACE_PREFIX" {
			mapping["sourcePrefix"] = opts.Value("prefix")
			if !opts.Has("target-prefix") {
				mapping["targetPrefix"] = ""
			}
		}
		row["keyMapping"] = mapping
		if err := createValidateRow(row, "buckets"); err != nil {
			return nil, err
		}
		nodes, err := createSelectNodes(client, opts.Values["node"])
		if err != nil {
			return nil, err
		}
		row["eligibleWorkerNodes"] = nodes
		return createSubmit(client, "buckets", []map[string]interface{}{row}, false, requestID)
	}
	if opts.Has("node") || opts.Has("mapping") {
		return nil, cliErrorf("本地迁移请用 --binding 绑定节点；--node 和 --mapping 仅适用于 OSS 间迁移")
	}
	bindings, err := createParseBindings(opts.Values["binding"])
	if err != nil {
		return nil, err
	}
	local := map[string]interface{}{"type": "LOCAL", "nodeBindings": bindings}
	oss := map[string]interface{}{"type": "OSS", "s3Alias": alias, "bucket": bucket}
	row["direction"], row["conflictPolicy"] = direction, conflict
	if direction == "OSS_TO_LOCAL" {
		if opts.Has("target-prefix") || opts.Has("target-key") {
			return nil, cliErrorf("下载的本地命名请使用 --target-relative-path（仅单对象）")
		}
		createCopyOption(oss, opts, "prefix", "prefix")
		createCopyOption(oss, opts, "object-key", "objectKey")
		createCopyOption(row, opts, "target-relative-path", "targetRelativePath")
		row["sourceEndpoint"], row["destinationEndpoint"] = oss, local
	} else {
		if opts.Has("prefix") || opts.Has("object-key") || opts.Has("target-relative-path") {
			return nil, cliErrorf("上传的源范围由 --binding 路径决定，目标请使用 --target-prefix 或 --target-key")
		}
		if opts.Has("target-prefix") && opts.Has("target-key") {
			return nil, cliErrorf("--target-prefix 与 --target-key 不能同时填写")
		}
		if opts.Has("target-prefix") && opts.Value("target-prefix") == "" {
			return nil, cliErrorf("--target-prefix 不能为空")
		}
		createCopyOption(oss, opts, "target-prefix", "prefix")
		createCopyOption(oss, opts, "target-key", "objectKey")
		row["sourceEndpoint"], row["destinationEndpoint"] = local, oss
	}
	if err := createValidateRow(row, "local"); err != nil {
		return nil, err
	}
	return createSubmit(client, "local", []map[string]interface{}{row}, opts.Has("validate-only"), requestID)
}

func runTaskImport(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := parseCommandOptions(args, []string{"file", "format", "request-id"}, []string{"balanced", "validate-only"})
	if err != nil {
		return nil, err
	}
	requestID, err := creationRequestIDOption(opts)
	if err != nil {
		return nil, err
	}
	for name, values := range opts.Values {
		if len(values) > 1 {
			return nil, cliErrorf("--%s 只能填写一次", name)
		}
	}
	if len(opts.Positionals) < 1 || len(opts.Positionals) > 2 {
		return nil, cliErrorf("用法: task import buckets|tasks|local <文件> [--format json|csv] [--validate-only]")
	}
	kind := opts.Positionals[0]
	if kind != "buckets" && kind != "tasks" && kind != "local" {
		return nil, cliErrorf("导入类型须为 buckets（存储桶迁移）、tasks（子任务）或 local（本地迁移）")
	}
	path := opts.Value("file")
	if len(opts.Positionals) == 2 {
		if opts.Has("file") {
			return nil, cliErrorf("文件只能通过位置参数或 --file 指定一次")
		}
		path = opts.Positionals[1]
	}
	if path == "" {
		return nil, cliErrorf("请指定导入文件")
	}
	if opts.Has("validate-only") && kind != "local" {
		return nil, cliErrorf("--validate-only 仅适用于本地迁移预检")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, cliErrorf("读取导入文件: %w", err)
	}
	format := opts.Value("format")
	if format == "" {
		format = "json"
		if strings.HasSuffix(strings.ToLower(path), ".csv") || strings.HasSuffix(strings.ToLower(path), ".txt") {
			format = "csv"
		}
	}
	var rows []map[string]interface{}
	switch format {
	case "json":
		if err := json.Unmarshal(data, &rows); err != nil {
			return nil, cliErrorf("JSON 须为任务对象数组: %w", err)
		}
	case "csv":
		if kind != "buckets" {
			return nil, cliErrorf("CSV 仅支持 buckets；tasks 和 local 请使用 JSON")
		}
		rows, err = createReadCSV(string(data))
		if err != nil {
			return nil, err
		}
	default:
		return nil, cliErrorf("--format 须为 json 或 csv")
	}
	if len(rows) == 0 {
		return nil, cliErrorf("任务数据必须是非空数组")
	}
	for i, row := range rows {
		if row == nil {
			return nil, cliErrorf("第 %d 项必须是任务对象", i+1)
		}
		if opts.Has("balanced") {
			row["distributionMode"] = "balanced"
		}
		if err := createValidateRow(row, kind); err != nil {
			return nil, cliErrorf("第 %d 项: %w", i+1, err)
		}
	}
	return createSubmit(client, kind, rows, opts.Has("validate-only"), requestID)
}

// The old import-tasks command has a different CSV schema from the WebUI. Keep
// it explicit so a dateBefore or prefix can never silently become another field.
func runLegacyTaskImport(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := parseCommandOptions(args, []string{"request-id"}, []string{"balanced"})
	if err != nil {
		return nil, err
	}
	requestID, err := creationRequestIDOption(opts)
	if err != nil {
		return nil, err
	}
	if len(opts.Positionals) != 1 {
		return nil, cliErrorf("用法: import-tasks <文件> [--balanced]；旧格式为源连接/桶,目标连接/桶,dateAfter,dateBefore,prefix,overwrite")
	}
	data, err := os.ReadFile(opts.Positionals[0])
	if err != nil {
		return nil, cliErrorf("读取导入文件: %w", err)
	}
	r := csv.NewReader(strings.NewReader(string(data)))
	r.FieldsPerRecord = -1
	rows := []map[string]interface{}{}
	for line := 1; ; line++ {
		cells, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, cliErrorf("旧格式 CSV 第 %d 行: %w", line, err)
		}
		if len(cells) == 1 && strings.TrimSpace(cells[0]) == "" {
			continue
		}
		if len(cells) < 2 || len(cells) > 6 {
			return nil, cliErrorf("旧格式 CSV 第 %d 行须为2至6列: 源连接/桶,目标连接/桶,dateAfter,dateBefore,prefix,overwrite", line)
		}
		for len(cells) < 6 {
			cells = append(cells, "")
		}
		sa, sb, err := createParseOssEndpoint(cells[0])
		if err != nil {
			return nil, cliErrorf("旧格式 CSV 第 %d 行: %w", line, err)
		}
		da, db, err := createParseOssEndpoint(cells[1])
		if err != nil {
			return nil, cliErrorf("旧格式 CSV 第 %d 行: %w", line, err)
		}
		overwrite := strings.ToLower(strings.TrimSpace(cells[5]))
		if overwrite != "" && overwrite != "true" && overwrite != "false" {
			return nil, cliErrorf("旧格式 CSV 第 %d 行 overwrite 须为 true 或 false", line)
		}
		mode := "affinity"
		if opts.Has("balanced") {
			mode = "balanced"
		}
		row := map[string]interface{}{"sourceS3Alias": sa, "sourceBucket": sb, "destS3Alias": da, "destBucket": db, "overwrite": overwrite == "true", "distributionMode": mode}
		for index, field := range []string{"dateAfter", "dateBefore", "prefix"} {
			if cells[index+2] != "" {
				row[field] = cells[index+2]
			}
		}
		if err := createValidateRow(row, "buckets"); err != nil {
			return nil, cliErrorf("旧格式 CSV 第 %d 行: %w", line, err)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, cliErrorf("文件中没有迁移请求")
	}
	return createSubmit(client, "buckets", rows, false, requestID)
}

func createCopyOption(target map[string]interface{}, opts commandOptions, flag, field string) {
	if opts.Has(flag) {
		target[field] = opts.Value(flag)
	}
}

func createParseOssEndpoint(value string) (string, string, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", cliErrorf("连接/桶格式错误: %q，应为连接别名/存储桶", value)
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func createParseBindings(values []string) ([]interface{}, error) {
	if len(values) == 0 {
		return nil, cliErrorf("本地迁移至少需要一个 --binding 节点IP=/绝对路径")
	}
	bindings := make([]interface{}, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 || adminNodeAddress(strings.TrimSpace(parts[0])) != nil || !agentAbsolutePath(parts[1]) {
			return nil, cliErrorf("节点路径绑定格式错误: %q，应为节点=绝对路径", value)
		}
		ip := strings.TrimSpace(parts[0])
		if seen[ip] {
			return nil, cliErrorf("同一执行节点只能绑定一次: %s", ip)
		}
		seen[ip] = true
		bindings = append(bindings, map[string]interface{}{"executionNodeIp": ip, "localPath": parts[1]})
	}
	return bindings, nil
}

func createSelectNodes(client *ApiClient, requested []string) ([]string, error) {
	response, err := client.Get("/local-nodes")
	if err != nil {
		return nil, cliErrorf("获取执行节点: %w", err)
	}
	data, _ := response["data"].(map[string]interface{})
	items, _ := data["nodes"].([]interface{})
	available := map[string]bool{}
	all := []string{}
	for _, item := range items {
		node, _ := item.(map[string]interface{})
		ip, _ := node["executionNodeIp"].(string)
		if node["reachable"] == true && ip != "" && !available[ip] {
			available[ip] = true
			all = append(all, ip)
		}
	}
	if len(requested) == 0 {
		if len(all) == 0 {
			return nil, cliErrorf("没有可用执行节点，请检查节点状态")
		}
		return all, nil
	}
	selected := []string{}
	seen := map[string]bool{}
	for _, raw := range requested {
		for _, item := range strings.Split(raw, ",") {
			ip := strings.TrimSpace(item)
			if !available[ip] {
				return nil, cliErrorf("所选执行节点不可用或不在集群中: %s", ip)
			}
			if !seen[ip] {
				seen[ip] = true
				selected = append(selected, ip)
			}
		}
	}
	return selected, nil
}

func createReadCSV(data string) ([]map[string]interface{}, error) {
	r := csv.NewReader(strings.NewReader(data))
	r.FieldsPerRecord = -1
	rows := []map[string]interface{}{}
	for line := 1; ; line++ {
		cells, err := r.Read()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, cliErrorf("CSV 第 %d 行: %w", line, err)
		}
		if len(cells) == 1 && strings.TrimSpace(cells[0]) == "" {
			continue
		}
		if len(cells) < 2 || len(cells) > 5 {
			return nil, cliErrorf("CSV 第 %d 行须为2至5列: 源连接/桶,目标连接/桶,dateAfter,overwrite,distributionMode；其他字段请用 JSON", line)
		}
		for len(cells) < 5 {
			cells = append(cells, "")
		}
		sa, sb, err := createParseOssEndpoint(strings.TrimSpace(cells[0]))
		if err != nil {
			return nil, cliErrorf("CSV 第 %d 行: %w", line, err)
		}
		da, db, err := createParseOssEndpoint(strings.TrimSpace(cells[1]))
		if err != nil {
			return nil, cliErrorf("CSV 第 %d 行: %w", line, err)
		}
		overwrite := strings.ToLower(strings.TrimSpace(cells[3]))
		if overwrite != "" && overwrite != "true" && overwrite != "false" {
			return nil, cliErrorf("CSV 第 %d 行 overwrite 须为 true 或 false", line)
		}
		mode := strings.TrimSpace(cells[4])
		if mode == "" {
			mode = "affinity"
		}
		row := map[string]interface{}{"sourceS3Alias": sa, "sourceBucket": sb, "destS3Alias": da, "destBucket": db, "overwrite": overwrite == "true", "distributionMode": mode}
		if date := strings.TrimSpace(cells[2]); date != "" {
			row["dateAfter"] = date
		}
		rows = append(rows, row)
	}
}

func createValidateRow(row map[string]interface{}, kind string) error {
	if value := row["inventoryManifestId"]; value != nil {
		if id, ok := value.(string); !ok || id != "" {
			return cliErrorf("清单任务请使用原 .incr 文件执行 import-inventory；普通任务 JSON 不包含完整 Key 清单")
		}
	}
	if value := row["taskBandwidthLimitMbps"]; value != nil {
		n, ok := value.(float64)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0.1 || n > 1000000 {
			return cliErrorf("taskBandwidthLimitMbps 须为 0.1–1,000,000 Mbps 的数值")
		}
	}
	if mode := row["distributionMode"]; mode != nil && mode != "affinity" && mode != "balanced" {
		return cliErrorf("distributionMode 须为 affinity 或 balanced")
	}
	var dates [2]time.Time
	for i, field := range []string{"dateAfter", "dateBefore"} {
		value := row[field]
		if value == nil {
			continue
		}
		s, ok := value.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return cliErrorf("%s 须为日期时间字符串", field)
		}
		s = strings.ReplaceAll(strings.TrimSpace(s), " ", "T")
		if len(s) == 16 {
			s += ":00"
		}
		parsed, err := time.Parse("2006-01-02T15:04:05", s)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339Nano, s)
		}
		if err != nil {
			return cliErrorf("%s 格式错误，应为 YYYY-MM-DDTHH:mm:ss 或 RFC3339", field)
		}
		dates[i] = parsed
		row[field] = s
	}
	if !dates[0].IsZero() && !dates[1].IsZero() && !dates[1].After(dates[0]) {
		return cliErrorf("结束时间必须晚于开始时间")
	}
	if kind == "local" {
		return createValidateLocalRow(row)
	}
	if dir := row["direction"]; dir != nil && dir != "OSS_TO_OSS" {
		return cliErrorf("本地迁移请求请使用 task import local")
	}
	for _, field := range []string{"sourceBucket", "destBucket"} {
		value, ok := row[field].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return cliErrorf("%s 不能为空", field)
		}
	}
	alias := func(field string) string {
		s, _ := row[field].(string)
		if strings.TrimSpace(s) == "" {
			return "default"
		}
		return strings.TrimSpace(s)
	}
	if strings.TrimSpace(row["sourceBucket"].(string)) == strings.TrimSpace(row["destBucket"].(string)) && alias("sourceS3Alias") == alias("destS3Alias") {
		return cliErrorf("源和目标不能是同一存储桶")
	}
	if value := row["overwrite"]; value != nil {
		if _, ok := value.(bool); !ok {
			return cliErrorf("overwrite 须为布尔值")
		}
	}
	return createValidateMapping(row, kind)
}

func createValidateMapping(row map[string]interface{}, kind string) error {
	endpoint, _ := row["sourceEndpoint"].(map[string]interface{})
	selection := map[string]string{}
	for _, field := range []string{"prefix", "objectKey"} {
		for _, source := range []map[string]interface{}{row, endpoint} {
			if value := source[field]; value != nil {
				s, ok := value.(string)
				if !ok {
					return cliErrorf("%s 须为字符串", field)
				}
				if selection[field] != "" && s != "" && selection[field] != s {
					return cliErrorf("%s 与 sourceEndpoint.%s 必须完全一致", field, field)
				}
				if s != "" {
					selection[field] = s
				}
			}
		}
	}
	if kind == "tasks" && row["prefix"] != selection["prefix"] && selection["prefix"] != "" && endpoint["prefixFilterEnabled"] != true {
		return cliErrorf("子任务 sourceEndpoint.prefix 需同时设置 prefixFilterEnabled:true，或改用顶层 prefix")
	}
	prefix, key := selection["prefix"], selection["objectKey"]
	if prefix != "" && key != "" {
		return cliErrorf("prefix 与 objectKey 不能同时填写")
	}
	if row["keyMapping"] == nil {
		return nil
	}
	mapping, ok := row["keyMapping"].(map[string]interface{})
	if !ok {
		return cliErrorf("keyMapping 须为对象")
	}
	fields := map[string]string{}
	for _, field := range []string{"sourcePrefix", "targetPrefix", "targetKey"} {
		if mapping[field] != nil {
			value, ok := mapping[field].(string)
			if !ok {
				return cliErrorf("keyMapping.%s 须为字符串", field)
			}
			fields[field] = value
		}
	}
	allowed := map[string]bool{}
	switch mapping["mode"] {
	case "PRESERVE":
	case "ADD_PREFIX":
		if fields["targetPrefix"] == "" {
			return cliErrorf("ADD_PREFIX 需要非空 targetPrefix")
		}
		allowed["targetPrefix"] = true
	case "REPLACE_PREFIX":
		if prefix == "" || fields["sourcePrefix"] != prefix {
			return cliErrorf("REPLACE_PREFIX 的 sourcePrefix 必须与源 prefix 完全一致且非空")
		}
		allowed["sourcePrefix"], allowed["targetPrefix"] = true, true
	case "EXACT_KEY":
		if key == "" || fields["targetKey"] == "" {
			return cliErrorf("EXACT_KEY 需要单个源 objectKey 和非空 targetKey")
		}
		allowed["targetKey"] = true
	default:
		return cliErrorf("keyMapping.mode 须为 PRESERVE、ADD_PREFIX、REPLACE_PREFIX 或 EXACT_KEY")
	}
	for field, value := range fields {
		if value != "" && !allowed[field] {
			return cliErrorf("%v 不使用 %s", mapping["mode"], field)
		}
	}
	return nil
}

func createValidateLocalRow(row map[string]interface{}) error {
	dir := row["direction"]
	if dir != "OSS_TO_LOCAL" && dir != "LOCAL_TO_OSS" {
		return cliErrorf("direction 须为 OSS_TO_LOCAL 或 LOCAL_TO_OSS")
	}
	source, sourceOK := row["sourceEndpoint"].(map[string]interface{})
	dest, destOK := row["destinationEndpoint"].(map[string]interface{})
	if !sourceOK || !destOK {
		return cliErrorf("请提供 sourceEndpoint 和 destinationEndpoint 对象")
	}
	local, oss := dest, source
	if dir == "LOCAL_TO_OSS" {
		local, oss = source, dest
	}
	if local["type"] != "LOCAL" || oss["type"] != "OSS" {
		return cliErrorf("端点 type 与迁移方向不匹配")
	}
	bucket, _ := oss["bucket"].(string)
	alias, _ := oss["s3Alias"].(string)
	if strings.TrimSpace(bucket) == "" || strings.TrimSpace(alias) == "" {
		return cliErrorf("OSS 端点的 s3Alias 和 bucket 不能为空")
	}
	for _, field := range []string{"objectKey", "prefix"} {
		if value := oss[field]; value != nil {
			if _, ok := value.(string); !ok {
				return cliErrorf("OSS 端点的 %s 须为字符串", field)
			}
		}
	}
	key, _ := oss["objectKey"].(string)
	prefix, _ := oss["prefix"].(string)
	if key != "" && prefix != "" {
		return cliErrorf("OSS 端点 objectKey 与 prefix 不能同时填写")
	}
	if policy := row["conflictPolicy"]; policy != nil && policy != "SKIP" && policy != "OVERWRITE" {
		return cliErrorf("conflictPolicy 须为 SKIP 或 OVERWRITE")
	}
	bindings, ok := local["nodeBindings"].([]interface{})
	if !ok || len(bindings) == 0 {
		return cliErrorf("LOCAL 端点需提供非空 nodeBindings 数组")
	}
	seen := map[string]bool{}
	for _, item := range bindings {
		binding, ok := item.(map[string]interface{})
		if !ok {
			return cliErrorf("nodeBindings 每项须为节点路径对象")
		}
		ip, _ := binding["executionNodeIp"].(string)
		path, _ := binding["localPath"].(string)
		ip = strings.TrimSpace(ip)
		if adminNodeAddress(ip) != nil || !agentAbsolutePath(path) {
			return cliErrorf("每个节点绑定需有 executionNodeIp 和节点系统的绝对路径 localPath")
		}
		if seen[ip] {
			return cliErrorf("同一执行节点只能绑定一次: %s", ip)
		}
		seen[ip] = true
		binding["executionNodeIp"], binding["localPath"] = ip, path
	}
	if value := row["targetRelativePath"]; value != nil && value != "" {
		path, ok := value.(string)
		if !ok || strings.TrimSpace(path) == "" || dir != "OSS_TO_LOCAL" || key == "" {
			return cliErrorf("targetRelativePath 仅适用于单对象下载并须为非空相对路径")
		}
		if strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
			return cliErrorf("targetRelativePath 须为本地目录内的相对路径")
		}
		for _, part := range strings.Split(path, "/") {
			if part == ".." {
				return cliErrorf("targetRelativePath 不允许包含 ..")
			}
		}
	}
	if dir == "LOCAL_TO_OSS" {
		row["distributionMode"] = "affinity"
		if len(bindings) > 1 {
			row["distributionMode"] = "balanced"
		}
	}
	return nil
}

func createSubmit(client *ApiClient, kind string, rows []map[string]interface{}, validateOnly bool, requestID string) (map[string]interface{}, error) {
	path, countField := "/add-buckets", "addedCount"
	if kind == "tasks" {
		path, countField = "/import", "importedCount"
	}
	if kind == "local" {
		checked, err := client.Post("/local-transfers/validate", rows)
		if err != nil {
			return checked, err
		}
		if checked == nil {
			return nil, cliErrorf("服务端未返回节点预检结果，尚未提交创建请求")
		}
		data, _ := checked["data"].(map[string]interface{})
		valid := data["valid"] == true
		bindings, _ := data["nodeBindings"].([]interface{})
		// The response is flattened in request/binding order by MasterLocalTransferService.
		index := 0
		for _, row := range rows {
			endpoint := row["destinationEndpoint"].(map[string]interface{})
			if row["direction"] == "LOCAL_TO_OSS" {
				endpoint = row["sourceEndpoint"].(map[string]interface{})
			}
			requested := endpoint["nodeBindings"].([]interface{})
			for range requested {
				if index >= len(bindings) {
					valid = false
					continue
				}
				binding, ok := bindings[index].(map[string]interface{})
				index++
				if !ok || binding == nil {
					valid = false
					continue
				}
				if binding["valid"] != true {
					valid = false
				}
				if row["direction"] != "LOCAL_TO_OSS" {
					continue
				}
				destination := row["destinationEndpoint"].(map[string]interface{})
				key, _ := destination["objectKey"].(string)
				if len(requested) > 1 && binding["pathType"] == "FILE" {
					binding["valid"], binding["message"] = false, client.text("单文件上传请只保留一个执行节点")
					valid = false
				}
				if key != "" && binding["pathType"] != "FILE" {
					binding["valid"], binding["message"] = false, client.text("指定目标 Key 仅适用于单文件；目录上传请使用目标前缀")
					valid = false
				}
			}
		}
		if index != len(bindings) {
			valid = false
		}
		if data != nil {
			data["valid"] = valid
		}
		if !valid {
			checked["submissionStatus"] = "validation_failed"
			return checked, cliErrorf("节点预检未通过，请检查节点和本地路径；尚未提交创建请求")
		}
		if validateOnly {
			checked["submissionStatus"] = "validated"
			checked["message"] = client.text("节点预检通过，尚未创建迁移任务")
			return checked, nil
		}
		path, countField = "/local-transfers", "acceptedCount"
	}
	if requestID == "" {
		var err error
		requestID, err = newCreationRequestID()
		if err != nil {
			return nil, err
		}
	}
	result, err := client.PostCreation(path, rows, requestID)
	if result == nil {
		result = map[string]interface{}{}
	}
	result["requestId"] = requestID
	if err != nil {
		return result, creationSubmissionError(err, requestID)
	}
	data := result
	if kind == "local" {
		data, _ = result["data"].(map[string]interface{})
	}
	count, confirmed := data[countField].(float64)
	unknown, _ := data["unknownCount"].(float64)
	rejected, _ := data["rejectedCount"].(float64)
	result["requestedCount"] = len(rows)
	if data["partial"] == true || unknown > 0 || rejected > 0 || !confirmed || count != float64(len(rows)) {
		result["submissionStatus"] = "partial"
		return result, creationSubmissionError(cliErrorf("提交结果未完全确认；部分任务可能已受理，请核对返回的任务编号"), requestID)
	}
	result["submissionStatus"] = "accepted"
	result["submissionNote"] = client.text("任务已受理并自动执行；受理不代表迁移完成，请查看任务状态")
	return result, nil
}
