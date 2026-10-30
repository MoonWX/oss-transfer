package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const taskControlBatchSize = 64

// An explicit page size lets the server distinguish pagination from an
// incomplete legacy snapshot. Follow its cursor even when filters empty a page.
func taskListPages(client *ApiClient, query url.Values, all bool) (map[string]interface{}, error) {
	if !query.Has("limit") {
		query.Set("limit", "100")
	}
	var result map[string]interface{}
	rows := []map[string]interface{}{}
	seenIDs := map[int]bool{}
	pages := 0
	metrics := map[string]int64{}
	displays := map[string]interface{}{}
	for {
		page, err := client.Get("/status?" + query.Encode())
		if result == nil {
			result = page
		}
		if err != nil {
			if result != nil {
				responseData(result)["partial"] = true
				responseData(result)["pageError"] = localizedCLIError(client.locale, err)
			}
			return result, err
		}
		data := responseData(page)
		items, valid := cliObjectRows(data["tasks"])
		if !valid {
			return result, cliErrorf("服务未返回有效的任务列表")
		}
		if !all {
			return page, nil
		}
		pages++
		for _, row := range items {
			id := getInt(row, "taskIndex")
			if id <= 0 || seenIDs[id] {
				responseData(result)["partial"] = true
				return result, cliErrorf("任务分页返回了无效或重复的任务 ID")
			}
			seenIDs[id] = true
			rows = append(rows, row)
		}
		for _, metric := range []string{"pendingTasks", "runningTasks", "listDoneTasks", "completedTasks", "failedTasks", "partialFailedTasks", "stoppedTasks", "totalPutObjectCount", "totalPutObjectSize", "totalFailedObjectCount"} {
			metrics[metric] += getInt64(data, metric)
		}
		if nodeDisplays, ok := data["nodeDisplays"].(map[string]interface{}); ok {
			for node, display := range nodeDisplays {
				displays[node] = display
			}
		}
		combined := responseData(result)
		combined["tasks"], combined["totalTasks"], combined["pages"] = rows, len(rows), pages
		combined["nodeDisplays"] = displays
		for metric, value := range metrics {
			combined[metric] = value
		}
		if incompleteResult(page) {
			combined["partial"] = true
			for _, field := range []string{"failedWorkers", "unreachableWorkers", "unavailableWorkers", "missingTaskIds"} {
				if value, exists := data[field]; exists {
					combined[field] = value
				}
			}
			return result, cliErrorf("任务列表不完整；本次未提交批量操作")
		}
		more := data["hasMore"] == true || data["truncated"] == true
		if !more {
			combined["partial"], combined["complete"], combined["hasMore"], combined["truncated"] = false, true, false, false
			delete(combined, "nextBeforeId")
			delete(combined, "message")
			return result, nil
		}
		next := getInt(data, "nextBeforeId")
		before, _ := strconv.ParseInt(query.Get("beforeId"), 10, 64)
		if next < 1 || before > 0 && int64(next) >= before {
			combined["partial"] = true
			return result, cliErrorf("任务分页游标未前进，已停止读取")
		}
		combined["nextBeforeId"] = next
		query.Set("beforeId", strconv.Itoa(next))
	}
}

type taskControlGroup struct {
	path string
	ids  []int
}

func selectedTaskSnapshots(client *ApiClient, ids []int) (map[int]map[string]interface{}, map[string]interface{}, error) {
	type lookup struct {
		result, row map[string]interface{}
		err         error
	}
	results := make([]lookup, len(ids))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(8, len(ids)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				results[index].result, results[index].row, results[index].err = taskByID(client, ids[index])
			}
		}()
	}
	for index := range ids {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	tasks := make(map[int]map[string]interface{}, len(ids))
	for index, lookup := range results {
		if lookup.err != nil {
			return nil, lookup.result, lookup.err
		}
		if incompleteResult(lookup.result) || incompleteResult(lookup.row) {
			return nil, lookup.result, cliErrorf("任务 #%d 的数据不完整；本次未执行操作", ids[index])
		}
		tasks[ids[index]] = lookup.row
	}
	return tasks, nil, nil
}

func taskControl(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	var flags []string
	if action == "retry" {
		flags = []string{"all", "yes"}
	}
	opts, err := parseCommandOptions(args, nil, flags)
	if err != nil {
		return nil, err
	}
	var ids []int
	var tasks map[int]map[string]interface{}
	if opts.Has("all") {
		if !opts.Has("yes") || len(opts.Positionals) > 0 {
			return nil, cliErrorf("重试所有失败任务请使用 task retry --all --yes，且不要指定任务 ID")
		}
		page, err := taskListPages(client, url.Values{"includeSubtasks": {"false"}}, true)
		if err != nil {
			return page, err
		}
		tasks = map[int]map[string]interface{}{}
		for _, row := range objectList(responseData(page)["tasks"]) {
			if status := getString(row, "status"); status == "FAILED" || status == "PARTIAL_FAILED" {
				id := getInt(row, "taskIndex")
				tasks[id], ids = row, append(ids, id)
			}
		}
	} else {
		ids, err = taskIDs(opts.Positionals)
		if err != nil {
			return nil, err
		}
		var result map[string]interface{}
		tasks, result, err = selectedTaskSnapshots(client, ids)
		if err != nil {
			return result, err
		}
	}
	groups := []taskControlGroup{}
	groupIndex := map[string]int{}
	generations := map[int]uint64{}
	for _, id := range ids {
		row := tasks[id]
		status := getString(row, "status")
		exploring := row["ossTaskGroup"] == true && (row["creationStatus"] == "QUEUED" || row["creationStatus"] == "RUNNING")
		path, allowed := "/"+action, false
		switch action {
		case "stop":
			allowed = exploring || status == "RUNNING" || status == "LIST_DONE" || status == "PENDING" || status == "WAITING" || status == "RESUMING" || status == "RECOVERABLE"
		case "resume", "retry":
			allowed = !exploring && (status == "STOPPED" || status == "FAILED" || status == "PARTIAL_FAILED")
		case "start":
			allowed = !localTask(row) && row["autoStart"] != true && status == "PENDING" && !exploring
		case "delete":
			allowed = status != "RUNNING" && status != "LIST_DONE" && status != "STOPPING" && status != "WAITING" && status != "RESUMING" && status != "RECOVERABLE" && !exploring
			path = "/clear"
		}
		if !allowed || status == "" {
			return nil, cliErrorf("任务 #%d 当前状态 %s 不允许 %s；本次未执行操作", id, status, action)
		}
		if action == "resume" || action == "retry" {
			generationText := fmt.Sprint(row["generation"])
			if number, ok := row["generation"].(float64); ok {
				generationText = strconv.FormatFloat(number, 'f', -1, 64)
			}
			generation, err := strconv.ParseUint(generationText, 10, 64)
			if err != nil || generation == 0 || generation > 1<<53-1 {
				return nil, cliErrorf("任务 #%d 的执行版本无效；请刷新或升级主节点后重试", id)
			}
			generations[id] = generation
		}
		if localTask(row) && (path == "/stop" || path == "/resume") {
			path = "/local-transfers" + path
		}
		index, exists := groupIndex[path]
		if !exists {
			index = len(groups)
			groupIndex[path] = index
			groups = append(groups, taskControlGroup{path: path})
		}
		groups[index].ids = append(groups[index].ids, id)
	}
	return taskControlBatches(client, action, groups, generations)
}

// Batch size bounds one request, not the selection. Never replay a timed out
// request: report its IDs as uncertain and leave the remaining batches unsent.
func taskControlBatches(client *ApiClient, action string, groups []taskControlGroup, generations map[int]uint64) (map[string]interface{}, error) {
	ids := []int{}
	for _, group := range groups {
		ids = append(ids, group.ids...)
	}
	details := []interface{}{}
	batches := []interface{}{}
	reportedPartial := false
	failedIDs, uncertainIDs, unsubmittedIDs := []int{}, []int{}, []int{}
	accepted, ignored, processed := 0, 0, 0
	issues := []string{}
	stop := false
	for _, group := range groups {
		for start := 0; start < len(group.ids); start += taskControlBatchSize {
			batch := group.ids[start:min(start+taskControlBatchSize, len(group.ids))]
			if stop {
				unsubmittedIDs = append(unsubmittedIDs, batch...)
				continue
			}
			body := map[string]interface{}{"taskIds": batch}
			if len(generations) > 0 {
				guards := make(map[int]uint64, len(batch))
				for _, id := range batch {
					guards[id] = generations[id]
				}
				body["expectedGenerations"] = guards
			}
			result, err := client.Post(group.path, body)
			processed += len(batch)
			entry := map[string]interface{}{"taskIds": batch, "operation": action, "result": result}
			batches = append(batches, entry)
			if err != nil {
				message := localizedCLIError(client.locale, err)
				entry["error"] = message
				issues = append(issues, message)
				var responseErr *apiHTTPError
				hasHTTPStatus := errors.As(err, &responseErr)
				explicitRejection := result != nil && result["success"] == false && !incompleteResult(result) && (!hasHTTPStatus || responseErr.Status == http.StatusInternalServerError)
				definite := explicitRejection || hasHTTPStatus && responseErr.Status >= 400 && responseErr.Status < 500 && responseErr.Status != http.StatusRequestTimeout
				if definite {
					failedIDs = append(failedIDs, batch...)
					stop = hasHTTPStatus && (responseErr.Status == http.StatusUnauthorized || responseErr.Status == http.StatusForbidden || responseErr.Status >= 500)
				} else {
					uncertainIDs = append(uncertainIDs, batch...)
					stop = true
				}
				for _, id := range batch {
					details = append(details, map[string]interface{}{"taskId": id, "success": false, "error": message, "uncertain": !definite})
				}
				continue
			}
			data := responseData(result)
			reportedPartial = reportedPartial || incompleteResult(result)
			byID := map[int]map[string]interface{}{}
			for _, detail := range objectList(data["details"]) {
				byID[getInt(detail, "taskId")] = detail
			}
			aggregate := len(byID) == 0 && !incompleteResult(result) && getInt(data, "failed") == 0 && getInt(data, "accepted")+getInt(data, "ignored") == len(batch)
			if aggregate {
				accepted += getInt(data, "accepted")
				ignored += getInt(data, "ignored")
				continue
			}
			batchIssue := false
			for _, id := range batch {
				row := byID[id]
				if row != nil {
					details = append(details, row)
				} else {
					details = append(details, map[string]interface{}{"taskId": id, "uncertain": true})
				}
				switch {
				case row != nil && (row["success"] == false || getString(row, "error") != ""):
					failedIDs = append(failedIDs, id)
					batchIssue = true
				case row != nil && row["success"] == true:
					if row["ignored"] == true {
						ignored++
					} else {
						accepted++
					}
				default:
					uncertainIDs = append(uncertainIDs, id)
					stop, batchIssue = true, true
				}
			}
			if batchIssue || incompleteResult(result) || getInt(data, "failed") > 0 || action == "start" && legacyStartFailure.MatchString(getString(result, "message")) {
				issues = append(issues, client.text("部分任务未受理，请检查节点结果"))
			}
		}
	}
	result := map[string]interface{}{"success": len(issues) == 0, "operation": action, "taskIds": ids, "details": details, "batches": batches, "accepted": accepted, "ignored": ignored, "failed": len(failedIDs), "uncertain": len(uncertainIDs), "unsubmitted": len(unsubmittedIDs), "processed": processed, "failedIds": failedIDs, "uncertainIds": uncertainIDs, "unsubmittedIds": unsubmittedIDs, "partial": len(issues) > 0 && (accepted+ignored > 0 || len(uncertainIDs) > 0 || len(unsubmittedIDs) > 0 || reportedPartial), "message": client.text("操作请求已处理；以任务最新状态为准")}
	if len(issues) > 0 {
		return result, fmt.Errorf("%s", strings.Join(issues, "; "))
	}
	return result, nil
}

func taskProgress(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := adminOptions(args, []string{"child", "generation"}, nil, 1)
	if err != nil {
		return nil, err
	}
	id, err := taskID(opts.Positionals[0])
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	if opts.Has("child") {
		parent, number, ok := strings.Cut(strings.TrimPrefix(opts.Value("child"), "#"), "-")
		parentID, parentErr := taskID(parent)
		childNumber, childErr := taskID(number)
		if !ok || parentErr != nil || childErr != nil || parentID != id {
			return nil, cliErrorf("子任务编号应为 %d-序号，序号从 1 开始", id)
		}
		query.Set("childNumber", strconv.Itoa(childNumber))
	}
	if opts.Has("generation") {
		generation, err := strconv.ParseUint(opts.Value("generation"), 10, 64)
		if err != nil || generation == 0 {
			return nil, cliErrorf("--generation 必须为正整数")
		}
		query.Set("generation", strconv.FormatUint(generation, 10))
	}
	path := fmt.Sprintf("/tasks/%d/progress", id)
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return client.Get(path)
}

func taskCleanupStatus(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := parseCommandOptions(args, nil, nil)
	if err != nil {
		return nil, err
	}
	ids, err := taskIDs(opts.Positionals)
	if err != nil {
		return nil, err
	}
	rows := []map[string]interface{}{}
	result := map[string]interface{}{"success": true, "tasks": rows, "cleanupStatus": true}
	for start := 0; start < len(ids); start += 128 {
		batch := ids[start:min(start+128, len(ids))]
		page, err := client.Post("/cleanup-status", map[string]interface{}{"taskIds": batch})
		if err != nil {
			result["success"], result["partial"], result["unreadTaskIds"] = false, true, ids[start:]
			return result, err
		}
		items, valid := cliObjectRows(responseData(page)["tasks"])
		seen := map[int]bool{}
		for _, row := range items {
			seen[getInt(row, "taskId")] = true
		}
		for _, id := range batch {
			valid = valid && seen[id]
		}
		if !valid || len(items) != len(batch) || incompleteResult(page) {
			result["success"], result["partial"], result["unreadTaskIds"] = false, true, ids[start:]
			return result, cliErrorf("清理进度数据不完整")
		}
		rows = append(rows, items...)
		result["tasks"] = rows
	}
	return result, nil
}
