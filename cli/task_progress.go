package main

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

func cliFinite(value interface{}) (float64, bool) {
	number, ok := cliNumeric(value)
	return number, ok && !math.IsNaN(number) && !math.IsInf(number, 0) && number >= 0
}

// cliTaskProgress prefers the Master's percent so the CLI and console agree.
// Older Masters omit it; their rows keep the console's former count formula.
func cliTaskProgress(task map[string]interface{}) string {
	percent := func(value float64) string { return strconv.Itoa(int(math.Floor(min(value, 100)))) + "%" }
	if value, exists := task["progressPercent"]; exists {
		if number, ok := cliFinite(value); ok {
			return percent(number)
		}
		return "—"
	}
	if getString(task, "status") == "COMPLETED" {
		return "100%"
	}
	total, ok := cliFinite(task["listObjectCount"])
	put, _ := cliFinite(task["putObjectCount"])
	skipped, _ := cliFinite(task["skippedObjectCount"])
	if complete, _ := task["scanComplete"].(bool); !complete || !ok || total == 0 || put+skipped > total {
		return "—"
	}
	return percent((put + skipped) / total * 100)
}

// The object count stays completed objects; bytes include fresh in-flight
// bytes when the Master reports them.
func cliTaskTransferred(out io.Writer, task map[string]interface{}) string {
	key := "transferredBytes"
	if _, ok := task[key]; !ok {
		key = "putObjectSize"
	}
	text := cliTaskCount(task, "putObjectCount") + " / " + cliTaskSize(task, key)
	if inFlight, ok := cliFinite(task["inFlightBytes"]); ok && inFlight > 0 && inFlight < math.MaxInt64 {
		text += " " + fmt.Sprintf(cliText(out, "(含传输中 %s)"), formatSize(int64(inFlight)))
	}
	return text
}

func cliRate(value interface{}) string {
	number, ok := cliFinite(value)
	if !ok {
		return "—"
	}
	return strconv.FormatFloat(math.Round(number*10)/10, 'f', -1, 64)
}

// Limits use the task-limit convention that zero means unlimited.
func cliRequestLimits(out io.Writer, limits map[string]interface{}) string {
	parts := []string{}
	for _, kind := range []struct{ field, label string }{{"list", "LIST / GET"}, {"head", "HEAD"}, {"put", "PUT"}} {
		value := cliText(out, "不限")
		if number, ok := cliFinite(limits[kind.field]); ok && number > 0 {
			value = cliRate(number)
		}
		parts = append(parts, kind.label+" "+value)
	}
	return strings.Join(parts, " · ")
}

// Observed rates are admitted HTTP attempts including retries. Unknown (an
// unreachable or older Agent) prints "—", never a partial sum.
func cliTaskRequestRates(out io.Writer, task map[string]interface{}) [][2]string {
	rates, ok := task["requestRates"].(map[string]interface{})
	if !ok {
		return nil
	}
	observed := "—"
	if values, ok := rates["observed"].(map[string]interface{}); ok {
		parts := []string{}
		for _, kind := range []struct{ field, label string }{{"list", "LIST"}, {"get", "GET"}, {"head", "HEAD"}, {"put", "PUT"}} {
			parts = append(parts, kind.label+" "+cliRate(values[kind.field]))
		}
		observed = strings.Join(parts, " · ") + cliText(out, " 次/秒")
	}
	fields := [][2]string{{cliText(out, "请求速率"), observed}}
	if limits, ok := rates["configured"].(map[string]interface{}); ok {
		fields = append(fields, [2]string{cliText(out, "任务请求上限"), cliRequestLimits(out, limits)})
	}
	if limits, ok := rates["effective"].(map[string]interface{}); ok {
		fields = append(fields, [2]string{cliText(out, "生效请求上限"), cliRequestLimits(out, limits)})
	}
	return fields
}
