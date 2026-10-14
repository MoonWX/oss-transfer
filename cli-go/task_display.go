package main

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
)

func renderTaskHistoryPage(out io.Writer, data map[string]interface{}, action string) {
	if before := getInt(data, "nextBeforeId"); before > 0 {
		fmt.Fprintf(out, "更早记录：task %s --before-id %d（可沿用原有筛选参数）\n", action, before)
	}
}

func renderSubtaskPage(out io.Writer, result map[string]interface{}) {
	page, ok := result["subtaskPage"].(map[string]interface{})
	if !ok {
		return
	}
	total, offset, limit := getInt(page, "totalCount"), getInt(page, "offset"), getInt(page, "limit")
	if total > 0 && limit > 0 {
		end := offset + limit
		if end > total {
			end = total
		}
		if len(objectList(result["children"])) == 0 || offset >= total {
			fmt.Fprintf(out, "此编号范围无记录，最大编号 %d\n", total)
		} else {
			fmt.Fprintf(out, "子任务编号 %d–%d / %d\n", offset+1, end, total)
		}
	}
	if page["hasMore"] == true && limit > 0 {
		parent := getInt(page, "parentTaskId")
		fmt.Fprintf(out, "下一页：task children %d --offset %d --limit %d\n", parent, offset+limit, limit)
	}
}

func cliTaskCount(task map[string]interface{}, key string) string {
	number, ok := cliNumeric(task[key])
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || math.Trunc(number) != number {
		return "—"
	}
	return strconv.FormatFloat(number, 'f', 0, 64)
}

func cliTaskSize(task map[string]interface{}, key string) string {
	number, ok := cliNumeric(task[key])
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number >= math.MaxInt64 {
		return "—"
	}
	return formatSize(int64(number))
}

func cliTaskMetric(task map[string]interface{}, prefix string) string {
	return cliTaskCount(task, prefix+"Count") + " / " + cliTaskSize(task, prefix+"Size")
}

func cliTaskSpeed(task map[string]interface{}, key string) string {
	size := cliTaskSize(task, key)
	if size == "—" {
		return size
	}
	return size + "/s"
}

func cliTaskTimestamp(value interface{}) string {
	if text, ok := value.(string); ok {
		return text
	}
	if number, ok := cliNumeric(value); ok && !math.IsNaN(number) && !math.IsInf(number, 0) && number > 0 && number < math.MaxInt64 {
		return time.UnixMilli(int64(number)).Format("2006-01-02 15:04:05")
	}
	return ""
}

func cliTaskScopeFields(task map[string]interface{}) [][2]string {
	fields := [][2]string{}
	endpoint, _ := task["sourceEndpoint"].(map[string]interface{})
	for _, field := range [][2]string{{"objectKey", "源对象 Key"}, {"prefix", "源前缀"}} {
		value := getString(task, field[0])
		if value == "" {
			value = getString(endpoint, field[0])
		}
		if value != "" {
			fields = append(fields, [2]string{field[1], value})
		}
	}
	if mapping, ok := task["keyMapping"].(map[string]interface{}); ok {
		for _, field := range [][2]string{{"mode", "目标命名"}, {"sourcePrefix", "替换源前缀"}, {"targetPrefix", "目标前缀"}, {"targetKey", "目标对象 Key"}} {
			if value, exists := mapping[field[0]].(string); exists {
				if value == "" {
					value = "\"\""
				}
				fields = append(fields, [2]string{field[1], value})
			}
		}
	}
	for _, field := range [][2]string{{"targetRelativePath", "目标相对路径"}, {"dateAfter", "修改时间晚于"}, {"dateBefore", "修改时间早于"}} {
		if value := getString(task, field[0]); value != "" {
			fields = append(fields, [2]string{field[1], value})
		}
	}
	return fields
}
