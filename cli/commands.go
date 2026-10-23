package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
)

var (
	version   = "unknown"
	commit    = "unknown"
	branch    = "unknown"
	buildTime = "unknown"
)

func checkResponseError(result map[string]interface{}) bool {
	if success, ok := result["success"].(bool); ok && !success {
		message := "操作失败"
		if msg, ok := result["message"].(string); ok {
			message = msg
		}
		fmt.Println()
		fmt.Printf(" 错误: %s\n", message)
		return true
	}
	return false
}

func printVersion() {
	fmt.Printf("Version: %s\n", version)
	fmt.Printf("Branch: %s\n", branch)
	fmt.Printf("Commit: %s\n", commit)
	fmt.Printf("BuildTime: %s\n", buildTime)
}

func printUsage() { fmt.Print(cliHelp) }

func main() { os.Exit(runCLI(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func cmdListTask(client *ApiClient, args []string) (map[string]interface{}, error) {
	filterStatus := ""
	filterSource := ""
	filterDest := ""
	filterWorker := ""
	showAll := false

	i := 0
	for i < len(args) {
		if args[i] == "--status" && i+1 < len(args) {
			filterStatus = args[i+1]
			i += 2
		} else if args[i] == "--source" && i+1 < len(args) {
			filterSource = args[i+1]
			i += 2
		} else if args[i] == "--dest" && i+1 < len(args) {
			filterDest = args[i+1]
			i += 2
		} else if args[i] == "--worker" && i+1 < len(args) {
			filterWorker = args[i+1]
			i += 2
		} else if args[i] == "-a" {
			showAll = true
			i++
		} else {
			i++
		}
	}

	params := url.Values{}
	if filterSource != "" {
		params.Set("sourceBucket", filterSource)
	}
	if filterDest != "" {
		params.Set("destBucket", filterDest)
	}
	if filterStatus != "" {
		params.Set("status", filterStatus)
	}
	if filterWorker != "" {
		params.Set("workerNode", filterWorker)
	}

	queryStr := params.Encode()
	if queryStr != "" {
		queryStr = "?" + queryStr
	}

	result, err := client.Get("/status" + queryStr)
	if err != nil {
		return nil, err
	}

	printTaskTable(result, filterStatus, filterSource, filterDest, showAll)
	return nil, nil
}

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
	colorWhite  = "\033[37m"
)

func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		if r < 128 {
			width += 1
		} else {
			width += 2
		}
	}
	return width
}

func padRight(s string, totalWidth int) string {
	currentWidth := displayWidth(s)
	if currentWidth >= totalWidth {
		return s
	}
	return s + strings.Repeat(" ", totalWidth-currentWidth)
}

func printTaskTable(result map[string]interface{}, filterStatus, filterSource, filterDest string, showAll bool) {
	if checkResponseError(result) {
		return
	}

	tasks, ok := result["tasks"].([]interface{})
	if !ok || len(tasks) == 0 {
		fmt.Println()
		fmt.Println(" 没有任务")
		fmt.Println()
		printSummary(result)
		return
	}

	sort.Slice(tasks, func(i, j int) bool {
		taskI, okI := tasks[i].(map[string]interface{})
		taskJ, okJ := tasks[j].(map[string]interface{})
		if !okI || !okJ {
			return false
		}
		return getInt(taskI, "taskIndex") < getInt(taskJ, "taskIndex")
	})

	if showAll {
		printTaskTableDetail(tasks)
	} else {
		printTaskTableSimple(tasks)
	}
	fmt.Println()
	printSummary(result)
}

func printTaskTableSimple(tasks []interface{}) {
	fmt.Println()
	fmt.Printf(" %-8s %-30s %-30s %10s %10s %10s %8s %12s %10s %16s\n",
		"TaskID", "SourceBucket", "DestBucket", "Scanned", "Migrated", "Skipped", "Failed", "Status", "Duration", "WorkerNode")
	fmt.Println(" " + strings.Repeat("-", 146))

	for _, t := range tasks {
		task, ok := t.(map[string]interface{})
		if !ok {
			continue
		}

		taskIndex := getInt(task, "taskIndex")
		sourceBucket := getString(task, "sourceBucket")
		sourceAlias := getString(task, "sourceS3Alias")
		destBucket := getString(task, "destBucket")
		destAlias := getString(task, "destS3Alias")
		listCount := getInt(task, "listObjectCount")
		putCount := getInt(task, "putObjectCount")
		skipCount := getInt(task, "skippedObjectCount")
		failCount := getInt(task, "failedObjectCount")
		status := getString(task, "status")
		workerNode := getString(task, "workerNode")
		duration := getString(task, "duration")

		srcDisplay := sourceBucket
		if sourceAlias != "" {
			srcDisplay = sourceAlias + "/" + sourceBucket
		}
		destDisplay := destBucket
		if destAlias != "" {
			destDisplay = destAlias + "/" + destBucket
		}

		srcLines := wrapText(srcDisplay, 28)
		destLines := wrapText(destDisplay, 28)

		if len(workerNode) > 14 {
			workerNode = workerNode[:14] + "..."
		}

		statusDisplay, color := getStatusDisplay(status)

		maxLines := len(srcLines)
		if len(destLines) > maxLines {
			maxLines = len(destLines)
		}

		for i := 0; i < maxLines; i++ {
			srcLine := ""
			if i < len(srcLines) {
				srcLine = srcLines[i]
			}

			destLine := ""
			if i < len(destLines) {
				destLine = destLines[i]
			}

			if i == 0 {
				fmt.Printf(" %-8d %-30s %-30s %10d %10d %10d ",
					taskIndex, srcLine, destLine, listCount, putCount, skipCount)

				fmt.Print(colorRed)
				fmt.Printf("%8d ", failCount)
				fmt.Print(colorReset)

				fmt.Print(color)
				fmt.Printf("%-12s ", statusDisplay)
				fmt.Print(colorReset)

				fmt.Print(colorCyan)
				fmt.Printf("%-10s ", duration)
				fmt.Print(colorReset)

				fmt.Print(colorWhite)
				fmt.Printf("%-16s", workerNode)
				fmt.Println(colorReset)
			} else {
				fmt.Printf(" %-8s %-30s %-30s\n", "", srcLine, destLine)
			}
		}
	}

	fmt.Println(" " + strings.Repeat("-", 146))
}

func wrapText(text string, maxWidth int) []string {
	if len(text) <= maxWidth {
		return []string{text}
	}

	var lines []string
	for len(text) > maxWidth {
		lines = append(lines, text[:maxWidth])
		text = text[maxWidth:]
	}
	if len(text) > 0 {
		lines = append(lines, text)
	}
	return lines
}

func printTaskTableDetail(tasks []interface{}) {
	fmt.Println()

	for idx, t := range tasks {
		task, ok := t.(map[string]interface{})
		if !ok {
			continue
		}

		taskIndex := getInt(task, "taskIndex")
		sourceBucket := getString(task, "sourceBucket")
		sourceAlias := getString(task, "sourceS3Alias")
		destBucket := getString(task, "destBucket")
		destAlias := getString(task, "destS3Alias")
		startKey := getString(task, "startKey")
		endKey := getString(task, "endKey")
		status := getString(task, "status")
		listCount := getInt(task, "listObjectCount")
		listSize := getInt64(task, "listObjectSize")
		putCount := getInt(task, "putObjectCount")
		putSize := getInt64(task, "putObjectSize")
		skipCount := getInt(task, "skippedObjectCount")
		skipSize := getInt64(task, "skippedObjectSize")
		failCount := getInt(task, "failedObjectCount")
		failSize := getInt64(task, "failedObjectSize")
		workerNode := getString(task, "workerNode")
		duration := getString(task, "duration")
		dateAfter := getString(task, "dateAfter")
		dateBefore := getString(task, "dateBefore")
		overwrite := getBool(task, "overwrite")
		createTime := getString(task, "createTime")
		startTime := getString(task, "startTime")
		endTime := getString(task, "endTime")

		srcDisplay := sourceBucket
		if sourceAlias != "" {
			srcDisplay = sourceAlias + "/" + sourceBucket
		}
		destDisplay := destBucket
		if destAlias != "" {
			destDisplay = destAlias + "/" + destBucket
		}

		statusDisplay, color := getStatusDisplay(status)

		fmt.Printf(" [%d] TaskID: %d\n", idx+1, taskIndex)
		fmt.Println(" " + strings.Repeat("-", 80))

		fmt.Print(" 状态: ")
		fmt.Print(color)
		fmt.Printf("%s", statusDisplay)
		fmt.Print(colorReset)
		fmt.Printf("  |  耗时: %s  |  节点: %s\n", duration, workerNode)

		fmt.Printf(" 源Bucket: %s\n", srcDisplay)
		fmt.Printf(" 目标Bucket: %s\n", destDisplay)

		if startKey != "" || endKey != "" {
			fmt.Printf(" Key范围: %s ~ %s\n", startKey, endKey)
		}

		fmt.Println()
		fmt.Println(" 数据统计:")
		fmt.Printf("   %-12s %12s %20s\n", "类型", "对象数", "大小")
		fmt.Printf("   %-12s %12d %20s\n", "扫描对象", listCount, formatSize(listSize))
		fmt.Printf("   %-12s %12d %20s\n", "迁移成功", putCount, formatSize(putSize))
		fmt.Printf("   %-12s %12d %20s\n", "跳过对象", skipCount, formatSize(skipSize))
		if failCount > 0 {
			fmt.Print(colorRed)
			fmt.Printf("   %-12s %12d %20s\n", "失败对象", failCount, formatSize(failSize))
			fmt.Print(colorReset)
		} else {
			fmt.Printf("   %-12s %12d %20s\n", "失败对象", failCount, formatSize(failSize))
		}

		fmt.Println()
		fmt.Println(" 任务信息:")
		if dateAfter != "" || dateBefore != "" {
			fmt.Printf("   日期筛选: ")
			if dateAfter != "" && dateBefore != "" {
				fmt.Printf("%s ~ %s\n", dateAfter, dateBefore)
			} else if dateAfter != "" {
				fmt.Printf("%s 之后\n", dateAfter)
			} else {
				fmt.Printf("%s 之前\n", dateBefore)
			}
		}
		fmt.Printf("   覆盖已存在: %v\n", overwrite)
		if createTime != "" {
			fmt.Printf("   创建时间: %s\n", createTime)
		}
		if startTime != "" {
			fmt.Printf("   开始时间: %s\n", startTime)
		}
		if endTime != "" {
			fmt.Printf("   结束时间: %s\n", endTime)
		}

		fmt.Println()
	}
}

func printSummary(result map[string]interface{}) {
	pending := getInt(result, "pendingTasks")
	running := getInt(result, "runningTasks") + getInt(result, "listDoneTasks")
	completed := getInt(result, "completedTasks")
	partialFailed := getInt(result, "partialFailedTasks")
	failed := getInt(result, "failedTasks")
	total := getInt(result, "totalTasks")

	fmt.Println()
	fmt.Println(" " + strings.Repeat("-", 60))
	fmt.Println(" 任务统计")
	fmt.Println(" " + strings.Repeat("-", 60))
	fmt.Printf(" 总任务数: %d\n", total)
	fmt.Print(" 状态分布: ")
	if pending > 0 {
		fmt.Printf("%s待执行: %d%s  ", colorYellow, pending, colorReset)
	}
	if running > 0 {
		fmt.Printf("%s执行中: %d%s  ", colorBlue, running, colorReset)
	} else {
		fmt.Printf("执行中: %d  ", running)
	}
	if completed > 0 {
		fmt.Printf("%s已完成: %d%s  ", colorGreen, completed, colorReset)
	} else {
		fmt.Printf("已完成: %d  ", completed)
	}
	if partialFailed > 0 {
		fmt.Printf("%s部分失败: %d%s  ", colorPurple, partialFailed, colorReset)
	} else {
		fmt.Printf("部分失败: %d  ", partialFailed)
	}
	if failed > 0 {
		fmt.Printf("%s失败: %d%s", colorRed, failed, colorReset)
	} else {
		fmt.Printf("失败: %d", failed)
	}
	fmt.Println()

	fmt.Println()
	fmt.Println(" " + strings.Repeat("-", 60))
	fmt.Println(" 数据统计")
	fmt.Println(" " + strings.Repeat("-", 60))

	totalListCount := getInt(result, "totalListObjectCount")
	totalListSize := getInt64(result, "totalListObjectSize")
	totalPutCount := getInt(result, "totalPutObjectCount")
	totalPutSize := getInt64(result, "totalPutObjectSize")
	totalSkipCount := getInt(result, "totalSkippedObjectCount")
	totalSkipSize := getInt64(result, "totalSkippedObjectSize")
	totalFailCount := getInt(result, "totalFailedObjectCount")
	totalFailSize := getInt64(result, "totalFailedObjectSize")

	fmt.Printf(" %-16s %15s %15s\n", "类型", "对象数", "大小")
	fmt.Println(" " + strings.Repeat("-", 50))
	fmt.Printf(" %-16s %15d %15s\n", "扫描对象", totalListCount, formatSize(totalListSize))
	fmt.Printf(" %-16s %15d %15s\n", "迁移成功", totalPutCount, formatSize(totalPutSize))
	fmt.Printf(" %-16s %15d %15s\n", "跳过对象", totalSkipCount, formatSize(totalSkipSize))
	if totalFailCount > 0 {
		fmt.Print(colorRed)
		fmt.Printf(" %-16s %15d %15s\n", "失败对象", totalFailCount, formatSize(totalFailSize))
		fmt.Print(colorReset)
	} else {
		fmt.Printf(" %-16s %15d %15s\n", "失败对象", totalFailCount, formatSize(totalFailSize))
	}

	if totalListCount > 0 {
		successRate := float64(totalPutCount+totalSkipCount) / float64(totalListCount) * 100
		failRate := float64(totalFailCount) / float64(totalListCount) * 100

		fmt.Println()
		fmt.Printf(" 成功率(迁移+跳过): %.2f%%  失败率: %.2f%%\n", successRate, failRate)
	}
	fmt.Println(" " + strings.Repeat("-", 60))
}

func getStatusDisplay(status string) (string, string) {
	switch status {
	case "PENDING", "WAITING":
		return "WAITING", colorYellow
	case "RUNNING":
		return "RUNNING", colorBlue
	case "LIST_DONE":
		return "RUNNING", colorBlue
	case "COMPLETED":
		return "COMPLETED", colorGreen
	case "PARTIAL_FAILED":
		return "PARTIAL_FAIL", colorPurple
	case "FAILED":
		return "FAILED", colorRed
	default:
		return status, colorWhite
	}
}

func getInt(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok {
		switch val := v.(type) {
		case int:
			return val
		case int64:
			return int(val)
		case float64:
			return int(val)
		case json.Number:
			i, _ := val.Int64()
			return int(i)
		}
	}
	return 0
}

func getInt64(m map[string]interface{}, key string) int64 {
	if v, ok := m[key]; ok {
		switch val := v.(type) {
		case int64:
			return val
		case float64:
			return int64(val)
		case json.Number:
			i, _ := val.Int64()
			return i
		}
	}
	return 0
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func formatSize(bytes int64) string {
	if bytes == 0 {
		return "0 B"
	}

	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	size := float64(bytes)
	unitIndex := 0

	for size >= 1024 && unitIndex < len(units)-1 {
		size /= 1024
		unitIndex++
	}

	return strconv.FormatFloat(math.Round(size*10)/10, 'f', -1, 64) + " " + units[unitIndex]
}

func cmdWorkerStatus(client *ApiClient) (map[string]interface{}, error) {
	result, err := client.Get("/worker-status")
	if err != nil {
		return nil, err
	}
	printWorkerStatusTable(result)
	return nil, nil
}

func printWorkerStatusTable(result map[string]interface{}) {
	if checkResponseError(result) {
		return
	}

	workers, ok := result["workers"].(map[string]interface{})
	if !ok || len(workers) == 0 {
		fmt.Println()
		fmt.Println(" No workers found")
		return
	}

	fmt.Println()
	fmt.Print(colorYellow)
	fmt.Printf(" %-16s %-12s %10s %10s %10s %10s %10s",
		"WorkerIP", "State", "ListQ", "PutQ", "ProgressQ", "FailQ", "FallbackQ")
	fmt.Println(colorReset)
	fmt.Println(" " + strings.Repeat("-", 88))

	ips := make([]string, 0, len(workers))
	for ip := range workers {
		ips = append(ips, ip)
	}
	sort.Strings(ips)

	for _, ip := range ips {
		v := workers[ip]
		worker, ok := v.(map[string]interface{})
		if !ok {
			continue
		}

		queueStatus, _ := worker["queueStatus"].(map[string]interface{})
		success, _ := worker["success"].(bool)
		queue := func(key string) string {
			if !success {
				return "—"
			}
			return cliTaskCount(queueStatus, key)
		}

		color := colorGreen
		if !success {
			color = colorRed
		}

		fmt.Print(color)
		state := "online"
		if !success {
			state = "unavailable"
		}
		fmt.Printf(" %-16s %-12s %10s %10s %10s %10s %10s",
			ip, state, queue("listQueueSize"), queue("putQueueSize"), queue("progressQueueSize"), queue("failedKeyQueueSize"), queue("fallbackQueueSize"))
		fmt.Println(colorReset)
	}

	fmt.Println(" " + strings.Repeat("-", 88))
}

func getIntFromMap(m map[string]interface{}, key string) int {
	if m == nil {
		return 0
	}
	return getInt(m, key)
}

func cmdStart(client *ApiClient, args []string) (map[string]interface{}, error) {
	body := map[string]interface{}{}

	taskIds := parseTaskIds(args)
	if len(taskIds) > 0 {
		body["taskIds"] = taskIds
		fmt.Printf("启动指定迁移任务: %v\n", taskIds)
	} else {
		sourceBucket, destBucket, workerNode := parseStartFilters(args)
		if sourceBucket != "" {
			body["sourceBucket"] = sourceBucket
		}
		if destBucket != "" {
			body["destBucket"] = destBucket
		}
		if workerNode != "" {
			body["workerNode"] = workerNode
		}
		if sourceBucket != "" || destBucket != "" || workerNode != "" {
			fmt.Printf("启动匹配任务: sourceBucket=%s, destBucket=%s, workerNode=%s\n", sourceBucket, destBucket, workerNode)
		} else {
			fmt.Println("启动所有待执行任务...")
		}
	}

	return client.Post("/start", body)
}

func parseTaskIds(args []string) []interface{} {
	var taskIds []interface{}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--") {
			break
		}
		parsedIds := parseTaskIdArg(arg)
		taskIds = append(taskIds, parsedIds...)
	}
	if len(taskIds) == 0 {
		return nil
	}
	return taskIds
}

func parseStartFilters(args []string) (string, string, string) {
	var sourceBucket, destBucket, workerNode string
	for i := 0; i < len(args); i++ {
		if args[i] == "--source" && i+1 < len(args) {
			sourceBucket = args[i+1]
			i++
		} else if args[i] == "--dest" && i+1 < len(args) {
			destBucket = args[i+1]
			i++
		} else if args[i] == "--worker" && i+1 < len(args) {
			workerNode = args[i+1]
			i++
		}
	}
	return sourceBucket, destBucket, workerNode
}

func cmdClear(client *ApiClient, args []string) (map[string]interface{}, error) {
	permanent := false
	var taskIds []interface{}
	var sourceBucket, destBucket string

	i := 0
	for i < len(args) {
		if args[i] == "--permanent" {
			permanent = true
			i++
		} else if args[i] == "--source" && i+1 < len(args) {
			sourceBucket = args[i+1]
			i += 2
		} else if args[i] == "--dest" && i+1 < len(args) {
			destBucket = args[i+1]
			i += 2
		} else {
			parsedIds := parseTaskIdArg(args[i])
			taskIds = append(taskIds, parsedIds...)
			i++
		}
	}

	body := map[string]interface{}{}
	if len(taskIds) > 0 {
		body["taskIds"] = taskIds
	}
	if permanent {
		body["permanent"] = true
	}
	if sourceBucket != "" {
		body["sourceBucket"] = sourceBucket
	}
	if destBucket != "" {
		body["destBucket"] = destBucket
	}

	action := "标记删除"
	if permanent {
		action = "永久删除"
	}

	if len(taskIds) > 0 {
		fmt.Printf("%s指定任务: %v\n", action, taskIds)
	} else if sourceBucket != "" || destBucket != "" {
		fmt.Printf("%s匹配的任务: sourceBucket=%s, destBucket=%s\n", action, sourceBucket, destBucket)
	} else {
		fmt.Printf("%s所有已完成的任务...\n", action)
	}

	return client.Post("/clear", body)
}

func parseTaskIdArg(arg string) []interface{} {
	var taskIds []interface{}

	parts := strings.Split(arg, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) == 2 {
				start, err1 := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
				end, err2 := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
				if err1 == nil && err2 == nil && start <= end {
					for i := start; i <= end; i++ {
						taskIds = append(taskIds, i)
					}
				}
			}
		} else {
			if id, err := strconv.Atoi(part); err == nil {
				taskIds = append(taskIds, id)
			}
		}
	}

	return taskIds
}

func cmdListDeleted(client *ApiClient, args []string) (map[string]interface{}, error) {
	showAll := false
	for _, arg := range args {
		if arg == "-a" {
			showAll = true
			break
		}
	}

	fmt.Println("查询标删任务列表...")
	result, err := client.Get("/deleted-tasks")
	if err != nil {
		return nil, err
	}

	printDeletedTasks(result, showAll)
	return nil, nil
}

func printDeletedTasks(result map[string]interface{}, showAll bool) {
	if checkResponseError(result) {
		return
	}

	deletedTasks, ok := result["deletedTasks"].(map[string]interface{})
	if !ok || len(deletedTasks) == 0 {
		fmt.Println()
		fmt.Println(" 没有标删任务")
		fmt.Println()
		return
	}

	if showAll {
		printDeletedTasksDetail(deletedTasks, result)
	} else {
		printDeletedTasksSimple(deletedTasks, result)
	}
}

func printDeletedTasksSimple(deletedTasks map[string]interface{}, result map[string]interface{}) {
	fmt.Println()
	fmt.Printf(" %-16s %8s %30s %30s %12s\n",
		"WorkerIP", "TaskID", "SourceBucket", "DestBucket", "Status")
	fmt.Println(" " + strings.Repeat("-", 100))

	for workerIp, val := range deletedTasks {
		if strings.HasSuffix(workerIp, "Count") {
			continue
		}

		tasks, ok := val.([]interface{})
		if !ok {
			continue
		}

		for _, t := range tasks {
			task, ok := t.(map[string]interface{})
			if !ok {
				continue
			}

			taskIndex := getInt(task, "taskIndex")
			sourceBucket := getString(task, "sourceBucket")
			destBucket := getString(task, "destBucket")
			status := getString(task, "status")

			if len(sourceBucket) > 28 {
				sourceBucket = sourceBucket[:25] + "..."
			}
			if len(destBucket) > 28 {
				destBucket = destBucket[:25] + "..."
			}

			fmt.Printf(" %-16s %8d %30s %30s %12s\n",
				workerIp, taskIndex, sourceBucket, destBucket, status)
		}
	}

	fmt.Println(" " + strings.Repeat("-", 100))
	totalCount := getInt(result, "totalCount")
	fmt.Printf("\n 共计 %d 个标删任务\n", totalCount)
}

func printDeletedTasksDetail(deletedTasks map[string]interface{}, result map[string]interface{}) {
	fmt.Println()
	taskIdx := 0

	for workerIp, val := range deletedTasks {
		if strings.HasSuffix(workerIp, "Count") {
			continue
		}

		tasks, ok := val.([]interface{})
		if !ok {
			continue
		}

		for _, t := range tasks {
			task, ok := t.(map[string]interface{})
			if !ok {
				continue
			}

			taskIdx++
			taskIndex := getInt(task, "taskIndex")
			sourceBucket := getString(task, "sourceBucket")
			sourceAlias := getString(task, "sourceS3Alias")
			destBucket := getString(task, "destBucket")
			destAlias := getString(task, "destS3Alias")
			startKey := getString(task, "startKey")
			endKey := getString(task, "endKey")
			status := getString(task, "status")
			listCount := getInt(task, "listObjectCount")
			listSize := getInt64(task, "listObjectSize")
			putCount := getInt(task, "putObjectCount")
			putSize := getInt64(task, "putObjectSize")
			skipCount := getInt(task, "skippedObjectCount")
			skipSize := getInt64(task, "skippedObjectSize")
			failCount := getInt(task, "failedObjectCount")
			failSize := getInt64(task, "failedObjectSize")
			dateAfter := getString(task, "dateAfter")
			dateBefore := getString(task, "dateBefore")
			overwrite := getBool(task, "overwrite")
			createTime := getString(task, "createTime")
			startTime := getString(task, "startTime")
			endTime := getString(task, "endTime")

			srcDisplay := sourceBucket
			if sourceAlias != "" {
				srcDisplay = sourceAlias + "/" + sourceBucket
			}
			destDisplay := destBucket
			if destAlias != "" {
				destDisplay = destAlias + "/" + destBucket
			}

			statusDisplay, color := getStatusDisplay(status)

			fmt.Printf(" [%d] TaskID: %d\n", taskIdx, taskIndex)
			fmt.Println(" " + strings.Repeat("-", 80))

			fmt.Print(" 状态: ")
			fmt.Print(color)
			fmt.Printf("%s", statusDisplay)
			fmt.Print(colorReset)
			fmt.Printf("  |  节点: %s\n", workerIp)

			fmt.Printf(" 源Bucket: %s\n", srcDisplay)
			fmt.Printf(" 目标Bucket: %s\n", destDisplay)

			if startKey != "" || endKey != "" {
				fmt.Printf(" Key范围: %s ~ %s\n", startKey, endKey)
			}

			fmt.Println()
			fmt.Println(" 数据统计:")
			fmt.Printf("   %-12s %12s %20s\n", "类型", "对象数", "大小")
			fmt.Printf("   %-12s %12d %20s\n", "扫描对象", listCount, formatSize(listSize))
			fmt.Printf("   %-12s %12d %20s\n", "迁移成功", putCount, formatSize(putSize))
			fmt.Printf("   %-12s %12d %20s\n", "跳过对象", skipCount, formatSize(skipSize))
			if failCount > 0 {
				fmt.Print(colorRed)
				fmt.Printf("   %-12s %12d %20s\n", "失败对象", failCount, formatSize(failSize))
				fmt.Print(colorReset)
			} else {
				fmt.Printf("   %-12s %12d %20s\n", "失败对象", failCount, formatSize(failSize))
			}

			fmt.Println()
			fmt.Println(" 任务信息:")
			if dateAfter != "" || dateBefore != "" {
				fmt.Printf("   日期筛选: ")
				if dateAfter != "" && dateBefore != "" {
					fmt.Printf("%s ~ %s\n", dateAfter, dateBefore)
				} else if dateAfter != "" {
					fmt.Printf("%s 之后\n", dateAfter)
				} else {
					fmt.Printf("%s 之前\n", dateBefore)
				}
			}
			fmt.Printf("   覆盖已存在: %v\n", overwrite)
			if createTime != "" {
				fmt.Printf("   创建时间: %s\n", createTime)
			}
			if startTime != "" {
				fmt.Printf("   开始时间: %s\n", startTime)
			}
			if endTime != "" {
				fmt.Printf("   结束时间: %s\n", endTime)
			}

			fmt.Println()
		}
	}

	totalCount := getInt(result, "totalCount")
	fmt.Printf(" 共计 %d 个标删任务\n", totalCount)
}

func cmdPermanentDelete(client *ApiClient, args []string) (map[string]interface{}, error) {
	if len(args) == 0 {
		fmt.Println("错误: 请指定要永久删除的任务ID")
		fmt.Println("用法: oss-transfer-cli permanent-delete 1,2,3")
		fmt.Println("      oss-transfer-cli permanent-delete 1-3")
		os.Exit(1)
	}

	taskIds := parseTaskIdArg(args[0])

	if len(taskIds) == 0 {
		fmt.Println("错误: 无效的任务ID")
		os.Exit(1)
	}

	body := map[string]interface{}{"taskIds": taskIds}
	fmt.Printf("永久删除标删任务: %v\n", taskIds)
	return client.Post("/permanent-delete", body)
}

func cmdRetry(client *ApiClient, args []string) (map[string]interface{}, error) {
	body := map[string]interface{}{}

	if len(args) > 0 {
		taskIds := parseTaskIdArg(args[0])
		body["taskIds"] = taskIds
		fmt.Printf("重试指定任务: %v\n", taskIds)
	} else {
		fmt.Println("重试所有部分失败/失败的任务...")
	}

	return client.Post("/retry-task", body)
}

func cmdFailed(client *ApiClient, args []string) (map[string]interface{}, error) {
	body := map[string]interface{}{}

	for i := 0; i < len(args); i++ {
		if args[i] == "--taskIndex" && i+1 < len(args) {
			taskIndex, _ := strconv.Atoi(args[i+1])
			body["taskIndex"] = taskIndex
			i++
		} else if args[i] == "--limit" && i+1 < len(args) {
			limit, _ := strconv.Atoi(args[i+1])
			body["limit"] = limit
			i++
		}
	}

	result, err := client.Post("/failed", body)
	if err != nil {
		return nil, err
	}
	printFailedTable(result, "Failed Objects")
	return nil, nil
}

func printFailedTable(result map[string]interface{}, title string) {
	if checkResponseError(result) {
		return
	}

	var allObjects []map[string]interface{}

	if objects, ok := result["objects"].([]interface{}); ok {
		for _, obj := range objects {
			if o, ok := obj.(map[string]interface{}); ok {
				allObjects = append(allObjects, o)
			}
		}
	} else if fallbackObjects, ok := result["fallbackObjects"].(map[string]interface{}); ok {
		for key, val := range fallbackObjects {
			if strings.HasSuffix(key, "Count") {
				continue
			}
			if objects, ok := val.([]interface{}); ok {
				for _, obj := range objects {
					if o, ok := obj.(map[string]interface{}); ok {
						allObjects = append(allObjects, o)
					}
				}
			}
		}
	} else if failedObjects, ok := result["failedObjects"].(map[string]interface{}); ok {
		for key, val := range failedObjects {
			if strings.HasSuffix(key, "Count") {
				continue
			}
			if objects, ok := val.([]interface{}); ok {
				for _, obj := range objects {
					if o, ok := obj.(map[string]interface{}); ok {
						allObjects = append(allObjects, o)
					}
				}
			}
		}
	} else if failedKeys, ok := result["failedKeys"].(map[string]interface{}); ok {
		for key, val := range failedKeys {
			if strings.HasSuffix(key, "Count") {
				continue
			}
			if objects, ok := val.([]interface{}); ok {
				for _, obj := range objects {
					if o, ok := obj.(map[string]interface{}); ok {
						allObjects = append(allObjects, o)
					}
				}
			}
		}
	}

	if len(allObjects) == 0 {
		fmt.Println()
		fmt.Printf(" No %s\n", title)
		return
	}

	fmt.Println()
	fmt.Printf(" %-8s %-60s %-20s %-22s\n", "TaskID", "Key", "Error", "Timestamp")
	fmt.Println(" " + strings.Repeat("-", 114))

	for _, o := range allObjects {
		taskIndex := getInt(o, "taskIndex")
		key := getString(o, "key")
		errMsg := getString(o, "errorMessage")
		if errMsg == "" {
			errMsg = getString(o, "error")
		}
		timestamp := getString(o, "timestampFormatted")
		if timestamp == "" {
			timestamp = formatTimestamp(o)
		}

		if len(key) > 58 {
			key = key[:55] + "..."
		}
		if len(errMsg) > 18 {
			errMsg = errMsg[:15] + "..."
		}

		fmt.Print(colorRed)
		fmt.Printf(" %-8d %-60s %-20s %-22s", taskIndex, key, errMsg, timestamp)
		fmt.Println(colorReset)
	}

	fmt.Println(" " + strings.Repeat("-", 114))
	fmt.Printf(" Total: %d\n", len(allObjects))
}

func cmdClearFailed(client *ApiClient) (map[string]interface{}, error) {
	fmt.Println("删除失败对象记录...")
	return client.Post("/failed/clear", map[string]interface{}{})
}

func cmdFallback(client *ApiClient, args []string) (map[string]interface{}, error) {
	body := map[string]interface{}{}

	for i := 0; i < len(args); i++ {
		if args[i] == "--taskIndex" && i+1 < len(args) {
			taskIndex, _ := strconv.Atoi(args[i+1])
			body["taskIndex"] = taskIndex
			i++
		} else if args[i] == "--limit" && i+1 < len(args) {
			limit, _ := strconv.Atoi(args[i+1])
			body["limit"] = limit
			i++
		}
	}

	result, err := client.Post("/fallback", body)
	if err != nil {
		return nil, err
	}
	printFailedTable(result, "Fallback Objects")
	return nil, nil
}

func cmdClearFallback(client *ApiClient) (map[string]interface{}, error) {
	fmt.Println("删除降级迁移对象记录...")
	return client.Post("/fallback/clear", map[string]interface{}{})
}

func cmdAddTask(client *ApiClient, args []string) (map[string]interface{}, error) {
	if len(args) < 2 {
		fmt.Println("错误: 请指定完整的bucket对信息")
		fmt.Println("用法: oss-transfer-cli add-task <源S3别名/源bucket> <目标S3别名/目标bucket> [--dateAfter <日期>] [--dateBefore <日期>] [--prefix <前缀>] [--overwrite] [--balanced]")
		fmt.Println("示例: oss-transfer-cli add-task yunhai/src-bucket yunhai/dest-bucket")
		fmt.Println("      oss-transfer-cli add-task yunhai/src-bucket yunhai/dest-bucket --dateAfter 2026-06-01T00:00:00 --overwrite")
		fmt.Println("      oss-transfer-cli add-task yunhai/src-bucket yunhai/dest-bucket --dateAfter 2026-06-01T00:00:00 --dateBefore 2026-06-30T23:59:59")
		fmt.Println("      oss-transfer-cli add-task yunhai/src-bucket yunhai/dest-bucket --prefix photos/2026/")
		fmt.Println("      oss-transfer-cli add-task yunhai/src-bucket yunhai/dest-bucket --balanced")
		os.Exit(1)
	}

	srcParts := strings.SplitN(args[0], "/", 2)
	if len(srcParts) != 2 {
		fmt.Println("错误: 源bucket格式错误，应为: 别名/bucket名")
		fmt.Println("示例: yunhai/src-bucket")
		os.Exit(1)
	}

	destParts := strings.SplitN(args[1], "/", 2)
	if len(destParts) != 2 {
		fmt.Println("错误: 目标bucket格式错误，应为: 别名/bucket名")
		fmt.Println("示例: yunhai/dest-bucket")
		os.Exit(1)
	}

	pair := map[string]interface{}{
		"sourceS3Alias": srcParts[0],
		"sourceBucket":  srcParts[1],
		"destS3Alias":   destParts[0],
		"destBucket":    destParts[1],
	}

	i := 2
	for i < len(args) {
		if args[i] == "--dateAfter" && i+1 < len(args) {
			pair["dateAfter"] = args[i+1]
			i += 2
		} else if args[i] == "--dateBefore" && i+1 < len(args) {
			pair["dateBefore"] = args[i+1]
			i += 2
		} else if args[i] == "--prefix" && i+1 < len(args) {
			pair["prefix"] = args[i+1]
			i += 2
		} else if args[i] == "--overwrite" {
			pair["overwrite"] = true
			i++
		} else if args[i] == "--balanced" {
			pair["distributionMode"] = "balanced"
			i++
		} else {
			i++
		}
	}

	fmt.Printf("添加迁移任务: %s/%s -> %s/%s\n",
		pair["sourceS3Alias"], pair["sourceBucket"],
		pair["destS3Alias"], pair["destBucket"])
	if pair["distributionMode"] != nil {
		fmt.Printf("分发模式: %s\n", pair["distributionMode"])
	}

	body := []map[string]interface{}{pair}
	return client.Post("/add-buckets", body)
}

func cmdImportTasks(client *ApiClient, args []string) (map[string]interface{}, error) {
	if len(args) == 0 {
		fmt.Println("错误: 请指定文件路径")
		fmt.Println("格式: 每行一个bucket对，格式为 源S3别名/源bucket,目标S3别名/目标bucket,日期筛选(可选),覆盖(可选)")
		fmt.Println("选项: --balanced 使用均衡模式分发任务")
		os.Exit(1)
	}

	distributionMode := ""
	filePath := ""
	i := 0
	for i < len(args) {
		if args[i] == "--balanced" {
			distributionMode = "balanced"
			i++
		} else if filePath == "" {
			filePath = args[i]
			i++
		} else {
			i++
		}
	}

	if filePath == "" {
		fmt.Println("错误: 请指定文件路径")
		os.Exit(1)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, cliErrorf("打开文件失败: %w", err)
	}
	defer file.Close()

	var pairs []map[string]interface{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		pair, err := parseBucketPair(line)
		if err != nil {
			fmt.Printf("警告: 跳过格式错误的行: %s (%v)\n", line, err)
			continue
		}
		if distributionMode != "" {
			pair["distributionMode"] = distributionMode
		}
		pairs = append(pairs, pair)
	}

	if len(pairs) == 0 {
		return nil, cliErrorf("文件中没有有效的bucket对")
	}

	fmt.Printf("从文件导入 %d 个bucket对: %s\n", len(pairs), filePath)
	if distributionMode != "" {
		fmt.Printf("分发模式: %s\n", distributionMode)
	}
	return client.Post("/add-buckets", pairs)
}

func cmdRateLimit(client *ApiClient) (map[string]interface{}, error) {
	result, err := client.Get("/rate-limit")
	if err != nil {
		return nil, err
	}
	printRateLimitTable(result)
	return nil, nil
}

func printRateLimitTable(result map[string]interface{}) {
	if checkResponseError(result) {
		return
	}

	rateLimits, ok := result["rateLimits"].([]interface{})
	if !ok || len(rateLimits) == 0 {
		fmt.Println()
		fmt.Println(" No rate limit data")
		return
	}

	fmt.Println()
	fmt.Printf(" %-16s %16s %16s %16s %20s\n", "WorkerIP", "List(QPS)", "Put(QPS)", "Head(QPS)", "Bandwidth(Mbps)")
	fmt.Println(" " + strings.Repeat("-", 92))

	for _, rl := range rateLimits {
		r, ok := rl.(map[string]interface{})
		if !ok {
			continue
		}

		workerIp := getString(r, "workerIp")

		listLimits, _ := r["list"].(map[string]interface{})
		putLimits, _ := r["put"].(map[string]interface{})
		headLimits, _ := r["head"].(map[string]interface{})
		bwLimits, _ := r["bandwidth"].(map[string]interface{})

		listCurrent := getIntFromMap(listLimits, "currentQps")
		listLimit := getIntFromMap(listLimits, "limitQps")
		putCurrent := getIntFromMap(putLimits, "currentQps")
		putLimit := getIntFromMap(putLimits, "limitQps")
		headCurrent := getIntFromMap(headLimits, "currentQps")
		headLimit := getIntFromMap(headLimits, "limitQps")
		bwCurrent := getIntFromMap(bwLimits, "currentBps")
		bwLimit := getIntFromMap(bwLimits, "limitBps")

		bwCurrentMbps := float64(bwCurrent) / 1000000.0
		bwLimitMbps := float64(bwLimit) / 1000000.0

		fmt.Print(colorCyan)
		fmt.Printf(" %-16s %6d / %-6d %6d / %-6d %6d / %-6d %8.0f / %-8.0f",
			workerIp, listCurrent, listLimit, putCurrent, putLimit, headCurrent, headLimit, bwCurrentMbps, bwLimitMbps)
		fmt.Println(colorReset)
	}

	fmt.Println(" " + strings.Repeat("-", 92))
	fmt.Println()
}

func cmdRateLimitHistory(client *ApiClient, args []string) (map[string]interface{}, error) {
	limit := 20
	if len(args) >= 1 {
		limit, _ = strconv.Atoi(args[0])
	}
	result, err := client.Get(fmt.Sprintf("/rate-limit/history?limit=%d", limit))
	if err != nil {
		return nil, err
	}
	printRateLimitHistoryTable(result)
	return nil, nil
}

func printRateLimitHistoryTable(result map[string]interface{}) {
	if checkResponseError(result) {
		return
	}

	adjustments, ok := result["adjustments"].([]interface{})
	if !ok || len(adjustments) == 0 {
		fmt.Println()
		fmt.Println(" No rate limit history")
		return
	}

	fmt.Println()
	fmt.Printf(" %-16s %-10s %-12s %-10s %-22s\n", "WorkerIP", "Type", "Value", "Manual", "Timestamp")
	fmt.Println(" " + strings.Repeat("-", 78))

	for _, adj := range adjustments {
		a, ok := adj.(map[string]interface{})
		if !ok {
			continue
		}

		workerIp := getString(a, "workerIp")
		rateType := getString(a, "type")
		value := getFloat(a, "value")
		manual := getBool(a, "manual")
		timestamp := formatTimestamp(a)

		manualStr := "auto"
		if manual {
			manualStr = "manual"
		}

		fmt.Print(colorCyan)
		fmt.Printf(" %-16s %-10s %-12.0f %-10s %-22s", workerIp, rateType, value, manualStr, timestamp)
		fmt.Println(colorReset)
	}

	fmt.Println(" " + strings.Repeat("-", 78))
	fmt.Printf(" Total: %d\n", len(adjustments))
}

func formatTimestamp(a map[string]interface{}) string {
	if ts := getString(a, "timestamp"); ts != "" {
		if len(ts) > 19 {
			return ts[:19]
		}
		return ts
	}
	if ts := a["timestamp"]; ts != nil {
		switch v := ts.(type) {
		case float64:
			return formatUnixTime(int64(v))
		case int64:
			return formatUnixTime(v)
		}
	}
	return ""
}

func formatUnixTime(unix int64) string {
	t := time.Unix(unix/1000, 0)
	return t.Format("2006-01-02 15:04:05")
}

func getFloat(m map[string]interface{}, key string) float64 {
	if v, ok := m[key]; ok {
		switch val := v.(type) {
		case float64:
			return val
		case int:
			return float64(val)
		}
	}
	return 0
}

func getBool(m map[string]interface{}, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func cmdSetRateLimit(client *ApiClient, args []string) (map[string]interface{}, error) {
	if len(args) < 2 {
		fmt.Println("用法: oss-transfer-cli set-rate-limit <type> <value> [ip]")
		fmt.Println()
		fmt.Println("类型 (type):")
		fmt.Println("  list       - 列出对象限速 (QPS)")
		fmt.Println("  put        - 上传对象限速 (QPS)")
		fmt.Println("  head       - HEAD请求限速 (QPS)")
		fmt.Println("  bandwidth  - 带宽限速 (bps)")
		fmt.Println()
		fmt.Println("值 (value):")
		fmt.Println("  支持单位后缀: K(千), M(百万), G(十亿)")
		fmt.Println("  示例: 3000, 1K, 500M, 1G, 1000M")
		fmt.Println()
		fmt.Println("示例:")
		fmt.Println("  oss-transfer-cli set-rate-limit list 100")
		fmt.Println("  oss-transfer-cli set-rate-limit put 500")
		fmt.Println("  oss-transfer-cli set-rate-limit bandwidth 1G")
		fmt.Println("  oss-transfer-cli set-rate-limit put 1000 10.226.157.132")
		os.Exit(1)
	}

	rateType := args[0]
	valueStr := args[1]
	qps := parseValueWithUnit(valueStr)
	body := map[string]interface{}{"qps": qps}

	if len(args) >= 3 {
		workerIp := args[2]
		fmt.Printf("设置节点 %s 的 %s 限速为 %s (%.0f)...\n", workerIp, rateType, valueStr, qps)
		return client.Post(fmt.Sprintf("/rate-limit/%s/%s", rateType, workerIp), body)
	}

	fmt.Printf("设置所有节点的 %s 限速为 %s (%.0f)...\n", rateType, valueStr, qps)
	return client.Post(fmt.Sprintf("/rate-limit/%s", rateType), body)
}

func parseValueWithUnit(valueStr string) float64 {
	if len(valueStr) == 0 {
		return 0
	}

	lastChar := strings.ToUpper(valueStr[len(valueStr)-1:])
	multiplier := 1.0

	switch lastChar {
	case "K":
		multiplier = 1000
		valueStr = valueStr[:len(valueStr)-1]
	case "M":
		multiplier = 1000000
		valueStr = valueStr[:len(valueStr)-1]
	case "G":
		multiplier = 1000000000
		valueStr = valueStr[:len(valueStr)-1]
	}

	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		fmt.Printf("警告: 无法解析值 '%s', 使用 0\n", valueStr)
		return 0
	}

	return value * multiplier
}

func cmdListS3Config(client *ApiClient) (map[string]interface{}, error) {
	result, err := client.Get("/s3-config")
	if err != nil {
		return nil, err
	}
	printS3ConfigTable(result)
	return nil, nil
}

func printS3ConfigTable(result map[string]interface{}) {
	if checkResponseError(result) {
		return
	}

	configs, ok := result["configs"].([]interface{})
	if !ok || len(configs) == 0 {
		fmt.Println()
		fmt.Println(" No S3 configurations found")
		return
	}

	fmt.Println()
	for _, cfg := range configs {
		c, ok := cfg.(map[string]interface{})
		if !ok {
			continue
		}

		alias := getString(c, "alias")
		endpoint := getString(c, "endpoint")

		fmt.Printf("%s\n", alias)
		fmt.Printf("  URL       : %s\n", endpoint)
		fmt.Printf("  API       : s3v4\n")
		fmt.Println()
	}

	fmt.Printf(" Total: %d\n", len(configs))
}

func cmdSetS3Config(client *ApiClient, args []string) (map[string]interface{}, error) {
	if len(args) < 4 {
		fmt.Println("用法: oss-transfer-cli set-s3-config <alias> <endpoint> <accessKey> <secretKey> [--no-validate] [--v2-signer] [--path-style]")
		fmt.Println("  --no-validate: 跳过连接校验")
		fmt.Println("  --v2-signer: 使用v2签名")
		fmt.Println("  --path-style: 使用路径风格访问")
		os.Exit(1)
	}

	validate := true
	useV2Signer := false
	pathStyle := false

	for i := 0; i < len(args); i++ {
		if args[i] == "--no-validate" {
			validate = false
		} else if args[i] == "--v2-signer" {
			useV2Signer = true
		} else if args[i] == "--path-style" {
			pathStyle = true
		}
	}

	body := map[string]interface{}{
		"alias":     args[0],
		"endpoint":  args[1],
		"accessKey": args[2],
		"secretKey": args[3],
		"validate":  validate,
	}

	if useV2Signer {
		body["useV2SignerType"] = true
	}
	if pathStyle {
		body["pathStyleEnable"] = true
	}

	fmt.Printf("设置S3配置: %s -> %s", args[0], args[1])
	if !validate {
		fmt.Printf(" (跳过校验)")
	}
	if useV2Signer {
		fmt.Printf(" (v2签名)")
	}
	if pathStyle {
		fmt.Printf(" (路径风格)")
	}
	fmt.Println()
	return client.Post("/s3-config", body)
}

func cmdRemoveS3Config(client *ApiClient, args []string) (map[string]interface{}, error) {
	if len(args) < 1 {
		fmt.Println("用法: oss-transfer-cli remove-s3-config <alias>")
		os.Exit(1)
	}

	fmt.Printf("删除S3配置: %s\n", args[0])
	return client.Delete("/s3-config/" + args[0])
}

type SourceObject struct {
	Name    string
	Updated time.Time
	Size    int64
	Md5Hash string
}

func cmdDiffObjects(args []string, outputs ...io.Writer) error {
	out := offlineCLIOutput(outputs)
	if len(args) < 3 {
		return cliErrorf("用法: diff-objects <源目录> <有序目标清单> <输出目录>；详见 help tools")
	}

	srcDir := args[0]
	destManifest := args[1]
	outputDir := args[2]
	mode := "sort-merge"
	falsePositiveRate := 0.01

	totalStartTime := time.Now()

	for i := 3; i < len(args); i++ {
		if args[i] == "--mode" && i+1 < len(args) {
			mode = args[i+1]
			i++
		} else if args[i] == "--false-positive" && i+1 < len(args) {
			rate, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil || rate <= 0 || rate >= 1 {
				return cliErrorf("--false-positive 必须大于 0 且小于 1: %s", args[i+1])
			}
			falsePositiveRate = rate
			i++
		}
	}

	if mode != "bloom" && mode != "sort-merge" {
		return cliErrorf("不支持的模式: %s，请使用 bloom 或 sort-merge", mode)
	}

	fmt.Fprintf(out, cliText(out, "提取增量对象...\n"))
	fmt.Fprintf(out, cliText(out, "  源清单目录: %s\n"), srcDir)
	fmt.Fprintf(out, cliText(out, "  目标清单: %s\n"), destManifest)
	fmt.Fprintf(out, cliText(out, "  输出目录: %s\n"), outputDir)
	fmt.Fprintf(out, cliText(out, "  模式: %s\n"), mode)

	srcDirInfo, err := os.Stat(srcDir)
	if err != nil {
		return cliErrorf("源目录不存在: %v", err)
	}
	if !srcDirInfo.IsDir() {
		return cliErrorf("源清单路径不是目录: %s", srcDir)
	}

	err = os.MkdirAll(outputDir, 0755)
	if err != nil {
		return cliErrorf("创建输出目录失败: %v", err)
	}

	destInfo, err := os.Stat(destManifest)
	if err != nil {
		return cliErrorf("目标清单文件不存在: %v", err)
	}

	if mode == "bloom" {
		destFileSize := destInfo.Size()
		lineCount, err := countLines(destManifest)
		if err != nil {
			return cliErrorf("统计目标清单行数失败: %v", err)
		}

		avgKeySize := int(destFileSize) / lineCount
		if avgKeySize < 50 {
			avgKeySize = 50
		}

		fmt.Fprintf(out, cliText(out, "  目标对象数: %d\n"), lineCount)

		bitsPerKey := -math.Log(falsePositiveRate) / (math.Ln2 * math.Ln2)
		memSize := int(float64(lineCount) * bitsPerKey / 8)
		fmt.Fprintf(out, cliText(out, "\n预计资源使用:"))
		fmt.Fprintf(out, cliText(out, "  内存: ~%s\n"), formatSize(int64(memSize)))
		fmt.Fprintf(out, cliText(out, "  磁盘: ~0\n"))
		fmt.Fprintf(out, cliText(out, "  准确率: %.4f%% \n"), (1-falsePositiveRate)*100)

		return cmdDiffObjectsBloom(srcDir, destManifest, outputDir, lineCount, falsePositiveRate, totalStartTime, out)
	} else {
		fmt.Fprintf(out, cliText(out, "\n预计资源使用:"))
		fmt.Fprintf(out, cliText(out, "  内存: ~10*单个源清单大小\n"))
		fmt.Fprintf(out, cliText(out, "  磁盘: ~0\n"))
		fmt.Fprintf(out, cliText(out, "  准确率: 100%%\n"))

		return cmdDiffObjectsSortMerge(srcDir, destManifest, outputDir, "", totalStartTime, out)
	}
}

func countLines(filename string) (int, error) {
	file, err := os.Open(filename)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	count := 0
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024*1024)
	scanner.Buffer(buf, 1024*1024*1024)

	for scanner.Scan() {
		if scanner.Text() != "" {
			count++
		}
	}

	return count, scanner.Err()
}

func cmdDiffObjectsBloom(srcDir, destManifest, outputDir string, expectedCount int, falsePositiveRate float64, totalStartTime time.Time, outputs ...io.Writer) error {
	out := offlineCLIOutput(outputs)
	fmt.Fprintln(out, cliText(out, "\n[1/3] 构建目标bucket对象布隆过滤器..."))
	buildStartTime := time.Now()
	filter, totalCount, err := buildBloomFilterWithCount(destManifest, expectedCount, falsePositiveRate)
	if err != nil {
		return cliErrorf("构建布隆过滤器失败: %v", err)
	}
	buildElapsed := time.Since(buildStartTime)
	fmt.Fprintf(out, cliText(out, "  已加载 %d 个目标对象到布隆过滤器，误判率: %.4f%%，耗时: %s\n"), totalCount, falsePositiveRate*100, buildElapsed)

	processStartTime := time.Now()
	err = processSourceManifests(srcDir, outputDir, func(key string) bool {
		return filter.Test([]byte(key))
	}, out)
	processElapsed := time.Since(processStartTime)
	totalElapsed := time.Since(totalStartTime)

	fmt.Fprintf(out, cliText(out, "\n  构建过滤器耗时: %s\n"), buildElapsed)
	fmt.Fprintf(out, cliText(out, "  筛选增量对象耗时: %s\n"), processElapsed)
	fmt.Fprintf(out, cliText(out, "  总耗时: %s\n"), totalElapsed)

	return err
}

func buildBloomFilterWithCount(filename string, expectedCount int, falsePositiveRate float64) (*bloom.BloomFilter, int, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	filter := bloom.NewWithEstimates(uint(expectedCount), falsePositiveRate)
	count := 0

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024*1024)
	scanner.Buffer(buf, 1024*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			key := strings.SplitN(line, " ", 2)[0]
			filter.Add([]byte(key))
			count++
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}

	return filter, count, nil
}

func cmdDiffObjectsSortMerge(srcDir, destManifest, outputDir, customTmpDir string, totalStartTime time.Time, outputs ...io.Writer) error {
	out := offlineCLIOutput(outputs)
	fmt.Fprintln(out, cliText(out, "\n[1/4] 遍历源bucket对象清单目录..."))
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return cliErrorf("读取源目录失败: %v", err)
	}

	var srcFiles []string
	var totalSrcSize int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileName := entry.Name()
		if strings.HasSuffix(fileName, ".csv") || strings.HasSuffix(fileName, ".CSV") {
			info, _ := entry.Info()
			totalSrcSize += info.Size()
			srcFiles = append(srcFiles, srcDir+"/"+fileName)
		}
	}

	fmt.Fprintf(out, cliText(out, "  找到 %d 个CSV文件，总大小: %s\n"), len(srcFiles), formatSize(totalSrcSize))

	destFile, err := os.Open(destManifest)
	if err != nil {
		return cliErrorf("打开目标清单失败: %v", err)
	}
	destStat, _ := destFile.Stat()
	destFileSize := destStat.Size()
	fmt.Fprintf(out, cliText(out, "  目标清单大小: %s\n"), formatSize(destFileSize))
	destFile.Close()

	sortedDir := "./sorted-src/"
	os.RemoveAll(sortedDir)
	err = os.MkdirAll(sortedDir, 0755)
	if err != nil {
		return cliErrorf("创建排序目录失败: %v", err)
	}

	fmt.Fprintln(out, cliText(out, "\n[2/4] 并发排序源清单并写入磁盘..."))

	type SortResult struct {
		Index      int
		SortedFile string
		KeyCount   int
		Err        error
	}

	sem := make(chan struct{}, 300)
	sortResults := make(chan SortResult, len(srcFiles))

	var sortWg sync.WaitGroup
	for idx := range srcFiles {
		sortWg.Add(1)
		go func(index int) {
			defer sortWg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			srcFile := srcFiles[index]
			sortedFile, keyCount, err := sortSourceManifestToDisk(srcFile, sortedDir)
			sortResults <- SortResult{
				Index:      index,
				SortedFile: sortedFile,
				KeyCount:   keyCount,
				Err:        err,
			}
		}(idx)
	}

	go func() {
		sortWg.Wait()
		close(sortResults)
	}()

	sortedFiles := make(map[int]string)
	totalKeys := 0
	sortedCount := 0
	for result := range sortResults {
		sortedCount++
		if result.Err != nil {
			return cliErrorf("排序源清单失败 %s: %v", srcFiles[result.Index], result.Err)
		}
		sortedFiles[result.Index] = result.SortedFile
		totalKeys += result.KeyCount
		if sortedCount%10 == 0 || sortedCount == len(srcFiles) {
			elapsed := time.Since(totalStartTime).Seconds()
			fmt.Fprintf(out, cliText(out, "  已排序 %d/%d 个文件，累计对象: %d，耗时: %.1fs\n"), sortedCount, len(srcFiles), totalKeys, elapsed)
		}
	}

	sortElapsed := time.Since(totalStartTime)
	fmt.Fprintf(out, cliText(out, "  排序完成，总对象数: %d，耗时: %s\n"), totalKeys, sortElapsed)

	mergeStartTime := time.Now()
	fmt.Fprintln(out, cliText(out, "\n[3/4] 并发归并筛选增量对象..."))
	type ProcessResult struct {
		FileName string
		Count    int
		Err      error
	}

	results := make(chan ProcessResult, len(srcFiles))

	var wg sync.WaitGroup
	mergeSem := make(chan struct{}, 300)
	for idx := range srcFiles {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			mergeSem <- struct{}{}
			defer func() { <-mergeSem }()

			srcFile := srcFiles[index]
			sortedFile := sortedFiles[index]
			fileName := filepath.Base(srcFile)
			outputFile := outputDir + "/" + fileName + ".incr"

			count, err := diffSortedFileWithDest(sortedFile, destManifest, outputFile)
			results <- ProcessResult{
				FileName: fileName,
				Count:    count,
				Err:      err,
			}
		}(idx)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	totalIncrCount := 0
	fileCount := 0
	errorCount := 0
	lastPrintTime := time.Now()

	for result := range results {
		fileCount++
		if result.Err != nil {
			errorCount++
			fmt.Fprintf(out, cliText(out, "  [错误] %s: %v\n"), result.FileName, result.Err)
		} else {
			totalIncrCount += result.Count
			now := time.Now()
			if now.Sub(lastPrintTime) >= 3*time.Second || fileCount%10 == 0 || fileCount == len(srcFiles) {
				elapsed := time.Since(mergeStartTime).Seconds()
				percent := float64(fileCount) / float64(len(srcFiles)) * 100
				fmt.Fprintf(out, cliText(out, "  进度: %d/%d (%.1f%%)，累计增量对象: %d，耗时: %.1fs\n"),
					fileCount, len(srcFiles), percent, totalIncrCount, elapsed)
				lastPrintTime = now
			}
		}
	}

	mergeElapsed := time.Since(mergeStartTime)

	fmt.Fprintln(out, cliText(out, "\n[4/4] 清理临时文件..."))
	os.RemoveAll(sortedDir)

	fmt.Fprintln(out, cliText(out, "\n处理完成!"))
	fmt.Fprintf(out, cliText(out, "  处理文件数: %d\n"), fileCount)
	fmt.Fprintf(out, cliText(out, "  成功文件数: %d\n"), fileCount-errorCount)
	fmt.Fprintf(out, cliText(out, "  总增量对象: %d\n"), totalIncrCount)
	if errorCount > 0 {
		fmt.Fprintf(out, cliText(out, "  失败文件数: %d\n"), errorCount)
	}
	fmt.Fprintf(out, cliText(out, "  排序耗时: %s\n"), sortElapsed)
	fmt.Fprintf(out, cliText(out, "  归并耗时: %s\n"), mergeElapsed)
	fmt.Fprintf(out, cliText(out, "  总耗时: %s\n"), time.Since(totalStartTime))

	return nil
}

type SrcObject struct {
	Key     string
	Updated time.Time
}

func sortSourceManifestInMemory(srcFile string) ([]SrcObject, int, error) {
	inFile, err := os.Open(srcFile)
	if err != nil {
		return nil, 0, err
	}
	defer inFile.Close()

	var objects []SrcObject
	scanner := bufio.NewScanner(inFile)
	buf := make([]byte, 0, 64*1024*1024)
	scanner.Buffer(buf, 1024*1024*1024)

	isFirstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if isFirstLine && strings.HasPrefix(line, "project,bucket,name,") {
			isFirstLine = false
			continue
		}
		isFirstLine = false

		parts := strings.SplitN(line, ",", 5)
		if len(parts) >= 4 {
			key := parts[2]
			updatedStr := parts[3]
			updated, err := time.Parse(time.RFC3339Nano, updatedStr)
			if err != nil {
				updated = time.Time{}
			}
			objects = append(objects, SrcObject{Key: key, Updated: updated})
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}

	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Key < objects[j].Key
	})

	return objects, len(objects), nil
}

func sortSourceManifestToDisk(srcFile, sortedDir string) (string, int, error) {
	inFile, err := os.Open(srcFile)
	if err != nil {
		return "", 0, err
	}
	defer inFile.Close()

	var objects []SrcObject
	scanner := bufio.NewScanner(inFile)
	buf := make([]byte, 0, 64*1024*1024)
	scanner.Buffer(buf, 1024*1024*1024)

	isFirstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if isFirstLine && strings.HasPrefix(line, "project,bucket,name,") {
			isFirstLine = false
			continue
		}
		isFirstLine = false

		parts := strings.SplitN(line, ",", 5)
		if len(parts) >= 4 {
			key := parts[2]
			updatedStr := parts[3]
			updated, err := time.Parse(time.RFC3339Nano, updatedStr)
			if err != nil {
				updated = time.Time{}
			}
			objects = append(objects, SrcObject{Key: key, Updated: updated})
		}
	}

	if err := scanner.Err(); err != nil {
		return "", 0, err
	}

	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Key < objects[j].Key
	})

	fileName := filepath.Base(srcFile)
	sortedFile := sortedDir + fileName + ".sorted"
	outFile, err := os.Create(sortedFile)
	if err != nil {
		return "", 0, err
	}
	defer outFile.Close()

	writer := bufio.NewWriterSize(outFile, 64*1024*1024)
	for _, obj := range objects {
		writer.WriteString(obj.Key)
		if !obj.Updated.IsZero() {
			writer.WriteString("\t")
			writer.WriteString(obj.Updated.Format(time.RFC3339Nano))
		}
		writer.WriteString("\n")
	}
	writer.Flush()

	return sortedFile, len(objects), nil
}

func diffSortedFileWithDest(sortedFile, destManifest, outputFile string) (int, error) {
	srcFile, err := os.Open(sortedFile)
	if err != nil {
		return 0, err
	}
	defer srcFile.Close()

	destFile, err := os.Open(destManifest)
	if err != nil {
		return 0, err
	}
	defer destFile.Close()

	outFile, err := os.Create(outputFile)
	if err != nil {
		return 0, err
	}
	defer outFile.Close()

	srcScanner := bufio.NewScanner(srcFile)
	srcBuf := make([]byte, 0, 64*1024*1024)
	srcScanner.Buffer(srcBuf, 1024*1024*1024)

	destScanner := bufio.NewScanner(destFile)
	destBuf := make([]byte, 0, 64*1024*1024)
	destScanner.Buffer(destBuf, 1024*1024*1024)

	count := 0
	var srcKey string
	var destKey string

	srcHasMore := srcScanner.Scan()
	if srcHasMore {
		srcLine := srcScanner.Text()
		srcParts := strings.SplitN(srcLine, "\t", 2)
		srcKey = srcParts[0]
	}

	destHasMore := destScanner.Scan()
	if destHasMore {
		destLine := destScanner.Text()
		destParts := strings.SplitN(destLine, " ", 4)
		destKey = destParts[0]
	}

	for srcHasMore {
		for destHasMore && destKey < srcKey {
			destHasMore = destScanner.Scan()
			if destHasMore {
				destLine := destScanner.Text()
				destParts := strings.SplitN(destLine, " ", 4)
				destKey = destParts[0]
			}
		}

		if !destHasMore || destKey > srcKey {
			outFile.WriteString(srcKey + "\n")
			count++
		}

		srcHasMore = srcScanner.Scan()
		if srcHasMore {
			srcLine := srcScanner.Text()
			srcParts := strings.SplitN(srcLine, "\t", 2)
			srcKey = srcParts[0]
		}
	}

	if err := srcScanner.Err(); err != nil {
		return 0, err
	}
	if err := destScanner.Err(); err != nil {
		return 0, err
	}

	return count, nil
}

func sortSourceManifestWithCount(srcFile, sortedFile string) (int, error) {
	inFile, err := os.Open(srcFile)
	if err != nil {
		return 0, err
	}
	defer inFile.Close()

	outFile, err := os.Create(sortedFile)
	if err != nil {
		return 0, err
	}
	defer outFile.Close()

	var keys []string
	scanner := bufio.NewScanner(inFile)
	buf := make([]byte, 0, 64*1024*1024)
	scanner.Buffer(buf, 1024*1024*1024)

	isFirstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if isFirstLine && strings.HasPrefix(line, "project,bucket,name,") {
			isFirstLine = false
			continue
		}
		isFirstLine = false

		parts := strings.SplitN(line, ",", 4)
		if len(parts) >= 3 {
			keys = append(keys, parts[2])
		}
	}

	if err := scanner.Err(); err != nil {
		return 0, err
	}

	sort.Strings(keys)

	for _, key := range keys {
		outFile.WriteString(key + "\n")
	}

	return len(keys), nil
}

func sortSourceManifest(srcFile, sortedFile string) error {
	inFile, err := os.Open(srcFile)
	if err != nil {
		return err
	}
	defer inFile.Close()

	outFile, err := os.Create(sortedFile)
	if err != nil {
		return err
	}
	defer outFile.Close()

	var keys []string
	scanner := bufio.NewScanner(inFile)
	buf := make([]byte, 0, 64*1024*1024)
	scanner.Buffer(buf, 1024*1024*1024)

	isFirstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if isFirstLine && strings.HasPrefix(line, "project,bucket,name,") {
			isFirstLine = false
			continue
		}
		isFirstLine = false

		parts := strings.SplitN(line, ",", 4)
		if len(parts) >= 3 {
			keys = append(keys, parts[2])
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	sort.Strings(keys)

	for _, key := range keys {
		outFile.WriteString(key + "\n")
	}

	return nil
}

func diffSortedSource(sortedFile, destManifest, outputFile string) (int, error) {
	srcFile, err := os.Open(sortedFile)
	if err != nil {
		return 0, err
	}
	defer srcFile.Close()

	destFile, err := os.Open(destManifest)
	if err != nil {
		return 0, err
	}
	defer destFile.Close()

	outFile, err := os.Create(outputFile)
	if err != nil {
		return 0, err
	}
	defer outFile.Close()

	srcScanner := bufio.NewScanner(srcFile)
	destScanner := bufio.NewScanner(destFile)

	srcBuf := make([]byte, 0, 64*1024*1024)
	srcScanner.Buffer(srcBuf, 1024*1024*1024)
	destBuf := make([]byte, 0, 64*1024*1024)
	destScanner.Buffer(destBuf, 1024*1024*1024)

	count := 0
	var srcKey, destKey string
	srcHasMore := srcScanner.Scan()
	if srcHasMore {
		srcKey = srcScanner.Text()
	}

	destHasMore := destScanner.Scan()
	if destHasMore {
		destKey = destScanner.Text()
		destKey = strings.SplitN(destKey, " ", 2)[0]
	}

	for srcHasMore {
		for destHasMore && destKey < srcKey {
			destHasMore = destScanner.Scan()
			if destHasMore {
				destKey = destScanner.Text()
				destKey = strings.SplitN(destKey, " ", 2)[0]
			}
		}

		if !destHasMore || destKey > srcKey {
			outFile.WriteString(srcKey + "\n")
			count++
		}

		srcHasMore = srcScanner.Scan()
		if srcHasMore {
			srcKey = srcScanner.Text()
		}
	}

	if err := srcScanner.Err(); err != nil {
		return 0, err
	}
	if err := destScanner.Err(); err != nil {
		return 0, err
	}

	return count, nil
}

func processSourceManifests(srcDir, outputDir string, existsFunc func(string) bool, outputs ...io.Writer) error {
	out := offlineCLIOutput(outputs)
	fmt.Fprintln(out, cliText(out, "\n[2/3] 遍历源bucket对象清单目录..."))
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return cliErrorf("读取源目录失败: %v", err)
	}

	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileName := entry.Name()
		if !strings.HasSuffix(fileName, ".csv") && !strings.HasSuffix(fileName, ".CSV") {
			continue
		}
		files = append(files, fileName)
	}

	fmt.Fprintf(out, cliText(out, "  找到 %d 个CSV文件，开始并发处理...\n"), len(files))

	type ProcessResult struct {
		FileName string
		Count    int
		Err      error
	}

	numWorkers := 50
	if len(files) < numWorkers {
		numWorkers = len(files)
	}

	jobs := make(chan string, len(files))
	results := make(chan ProcessResult, len(files))

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fileName := range jobs {
				srcFile := srcDir + "/" + fileName
				outputFile := outputDir + "/" + fileName + ".incr"
				count, err := processSrcManifestWithCheck(srcFile, outputFile, existsFunc)
				results <- ProcessResult{
					FileName: fileName,
					Count:    count,
					Err:      err,
				}
			}
		}()
	}

	for _, fileName := range files {
		jobs <- fileName
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	totalIncrCount := 0
	fileCount := 0
	errorCount := 0

	for result := range results {
		fileCount++
		if result.Err != nil {
			errorCount++
			fmt.Fprintf(out, cliText(out, "  [错误] %s: %v\n"), result.FileName, result.Err)
		} else {
			totalIncrCount += result.Count
			if fileCount%10 == 0 {
				fmt.Fprintf(out, cliText(out, "  已处理 %d/%d 个文件，累计增量对象: %d\n"), fileCount, len(files), totalIncrCount)
			}
		}
	}

	fmt.Fprintln(out, cliText(out, "\n[3/3] 处理完成!"))
	fmt.Fprintf(out, cliText(out, "  处理文件数: %d\n"), fileCount)
	fmt.Fprintf(out, cliText(out, "  成功文件数: %d\n"), fileCount-errorCount)
	fmt.Fprintf(out, cliText(out, "  总增量对象: %d\n"), totalIncrCount)
	if errorCount > 0 {
		fmt.Fprintf(out, cliText(out, "  失败文件数: %d\n"), errorCount)
	}

	return nil
}

func processSrcManifestWithCheck(srcFile, outputFile string, existsFunc func(string) bool) (int, error) {
	inFile, err := os.Open(srcFile)
	if err != nil {
		return 0, err
	}
	defer inFile.Close()

	outFile, err := os.Create(outputFile)
	if err != nil {
		return 0, err
	}
	defer outFile.Close()

	count := 0
	scanner := bufio.NewScanner(inFile)
	buf := make([]byte, 0, 64*1024*1024)
	scanner.Buffer(buf, 1024*1024*1024)

	isFirstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if isFirstLine && strings.HasPrefix(line, "project,bucket,name,") {
			isFirstLine = false
			continue
		}
		isFirstLine = false

		parts := strings.SplitN(line, ",", 4)
		if len(parts) >= 3 {
			key := parts[2]
			if !existsFunc(key) {
				outFile.WriteString(key + "\n")
				count++
			}
		}
	}

	return count, scanner.Err()
}

func cmdCountLines(args []string, outputs ...io.Writer) error {
	out := offlineCLIOutput(outputs)
	if len(args) < 1 {
		return cliErrorf("用法: count-lines <目录>")
	}

	dir := args[0]

	dirInfo, err := os.Stat(dir)
	if err != nil {
		return cliErrorf("目录不存在: %v", err)
	}
	if !dirInfo.IsDir() {
		return cliErrorf("路径不是目录: %s", dir)
	}

	fmt.Fprintf(out, cliText(out, "统计目录: %s\n"), dir)
	fmt.Fprintln(out, cliText(out, "扫描文件..."))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return cliErrorf("读取目录失败: %v", err)
	}

	type FileResult struct {
		Lines int
		Err   error
	}

	files := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			files = append(files, dir+"/"+entry.Name())
		}
	}

	fmt.Fprintf(out, cliText(out, "找到 %d 个文件，开始并发统计...\n"), len(files))

	numWorkers := 10
	if len(files) < numWorkers {
		numWorkers = len(files)
	}

	jobs := make(chan string, len(files))
	results := make(chan FileResult, len(files))

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range jobs {
				lines, err := countFileLines(file)
				results <- FileResult{Lines: lines, Err: err}
			}
		}()
	}

	for _, file := range files {
		jobs <- file
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	totalLines := 0
	fileCount := 0
	errorCount := 0

	for result := range results {
		if result.Err != nil {
			errorCount++
		} else {
			totalLines += result.Lines
			fileCount++
			if fileCount%100 == 0 {
				fmt.Fprintf(out, cliText(out, "已处理 %d 个文件，累计 %d 行...\n"), fileCount, totalLines)
			}
		}
	}

	fmt.Fprintln(out, "================================")
	fmt.Fprintf(out, cliText(out, "文件总数: %d\n"), fileCount)
	fmt.Fprintf(out, cliText(out, "总行数: %d\n"), totalLines)
	if errorCount > 0 {
		fmt.Fprintf(out, cliText(out, "失败文件: %d\n"), errorCount)
	}

	return nil
}

func countFileLines(filename string) (int, error) {
	file, err := os.Open(filename)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	lines := 0
	for scanner.Scan() {
		lines++
	}

	if err := scanner.Err(); err != nil {
		return 0, err
	}

	return lines, nil
}
