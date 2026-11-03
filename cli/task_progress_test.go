package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestTaskProgressPrefersMasterPercentAndFallsBackToCounts(t *testing.T) {
	for _, test := range []struct {
		task map[string]interface{}
		want string
	}{
		// One large object in flight: bytes say 70%, counts would say 99%.
		{map[string]interface{}{"progressPercent": 70.4, "listObjectCount": float64(12709), "putObjectCount": float64(12708), "scanComplete": true}, "70%"},
		{map[string]interface{}{"progressPercent": 99.9, "status": "RUNNING"}, "99%"},
		{map[string]interface{}{"progressPercent": nil, "listObjectCount": float64(10), "putObjectCount": float64(5), "scanComplete": true}, "—"},
		// Older Masters omit the field.
		{map[string]interface{}{"listObjectCount": float64(4), "putObjectCount": float64(1), "skippedObjectCount": float64(1), "scanComplete": true}, "50%"},
		{map[string]interface{}{"listObjectCount": float64(4), "putObjectCount": float64(1), "scanComplete": false}, "—"},
		{map[string]interface{}{"listObjectCount": float64(4), "putObjectCount": float64(5), "scanComplete": true}, "—"},
		{map[string]interface{}{"status": "COMPLETED"}, "100%"},
	} {
		if got := cliTaskProgress(test.task); got != test.want {
			t.Errorf("%v: %s, want %s", test.task, got, test.want)
		}
	}
	var output bytes.Buffer
	renderTaskRows(&output, []map[string]interface{}{{"taskIndex": 61, "status": "RUNNING", "progressPercent": 70.4}}, false)
	if !strings.Contains(output.String(), "进度") || !strings.Contains(output.String(), "70%") {
		t.Fatalf("list progress missing: %s", output.String())
	}
}

func TestTaskShowIncludesInFlightBytesAndRequestRates(t *testing.T) {
	task := map[string]interface{}{
		"taskIndex": 61, "status": "RUNNING", "putObjectCount": float64(12708), "putObjectSize": float64(6 << 30),
		"transferredBytes": float64(7 << 30), "inFlightBytes": float64(1 << 30), "progressPercent": 70.0,
		"requestRates": map[string]interface{}{
			"observed":   map[string]interface{}{"list": 0.0, "get": 2.4, "head": 0.0, "put": 85.24},
			"configured": map[string]interface{}{"scope": "CLUSTER", "list": 100.0, "head": 0.0, "put": 0.0},
			"effective":  map[string]interface{}{"list": 100.0, "head": 0.0, "put": 1400.0},
		},
	}
	for _, test := range []struct {
		locale cliLocale
		want   []string
	}{
		{localeZH, []string{"12708 / 7 GiB (含传输中 1 GiB)", "进度", "70%", "请求速率", "LIST 0 · GET 2.4 · HEAD 0 · PUT 85.2 次/秒", "任务请求上限", "LIST / GET 100 · HEAD 不限 · PUT 不限", "生效请求上限", "PUT 1400"}},
		{localeEN, []string{"12708 / 7 GiB (includes 1 GiB in flight)", "Progress", "Request rate", "PUT 85.2 req/s", "Task request limit", "HEAD unlimited", "Effective request limit"}},
		{localeJA, []string{"(転送中 1 GiB を含む)", "進捗", "リクエストレート", "PUT 85.2 回/秒", "実効リクエスト上限", "無制限"}},
	} {
		var output bytes.Buffer
		renderTaskDetail(&cliLocaleWriter{Writer: &output, locale: test.locale}, task)
		for _, want := range test.want {
			if !strings.Contains(output.String(), want) {
				t.Errorf("%s missing %q: %s", test.locale, want, output.String())
			}
		}
	}
	// Unknown observed rates and an older Master's durable-only row.
	task["requestRates"] = map[string]interface{}{"observed": nil, "configured": nil, "effective": nil}
	delete(task, "transferredBytes")
	delete(task, "inFlightBytes")
	var output bytes.Buffer
	renderTaskDetail(&output, task)
	if !strings.Contains(output.String(), "12708 / 6 GiB") || strings.Contains(output.String(), "含传输中") || strings.Contains(output.String(), "次/秒") || strings.Contains(output.String(), "生效请求上限") {
		t.Fatalf("unknown values were invented: %s", output.String())
	}
}
