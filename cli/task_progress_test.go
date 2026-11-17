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
		{localeZH, []string{"12708 / 7 GiB", "进度", "70%", "请求速率", "LIST 0 · GET 2.4 · HEAD 0 · PUT 85.2 次/秒", "任务请求上限", "LIST / GET 100 · HEAD 不限 · PUT 不限", "生效请求上限", "PUT 1400（LIST 还受扫描节点的 LIST 上限约束，未计入）"}},
		{localeEN, []string{"12708 / 7 GiB", "Progress", "Request rate", "PUT 85.2 req/s", "Task request limit", "HEAD unlimited", "Effective request limit", "PUT 1400 (LIST is also bounded by the LIST limit of the node that scans, not included)"}},
		{localeJA, []string{"12708 / 7 GiB", "進捗", "リクエストレート", "PUT 85.2 回/秒", "実効リクエスト上限", "無制限", "スキャンするノードの LIST 上限"}},
	} {
		var output bytes.Buffer
		renderTaskDetail(&cliLocaleWriter{Writer: &output, locale: test.locale}, task)
		for _, want := range test.want {
			if !strings.Contains(output.String(), want) {
				t.Errorf("%s missing %q: %s", test.locale, want, output.String())
			}
		}
		for _, note := range []string{"传输中", "in flight", "転送中"} {
			if strings.Contains(output.String(), note) {
				t.Errorf("%s shows an in-flight note %q: %s", test.locale, note, output.String())
			}
		}
	}
	// Unknown observed rates and an older Master's durable-only row.
	task["requestRates"] = map[string]interface{}{"observed": nil, "configured": nil, "effective": nil}
	delete(task, "transferredBytes")
	delete(task, "inFlightBytes")
	var output bytes.Buffer
	renderTaskDetail(&output, task)
	if !strings.Contains(output.String(), "12708 / 6 GiB") || strings.Contains(output.String(), "次/秒") || strings.Contains(output.String(), "生效请求上限") {
		t.Fatalf("unknown values were invented: %s", output.String())
	}
}

// File progress floors its percentage, never shows 100% before the
// destination confirmed every byte, names each stage and wait in the chosen
// language, says what "transferred" counts, and marks older Agents' rows
// when the Master reports each file's basis.
func TestTaskProgressFloorsAndLocalizesStages(t *testing.T) {
	file := func(stage, basis, phase string, transferred float64) map[string]interface{} {
		row := map[string]interface{}{"childTaskId": "7-1", "executionNodeLabel": "worker", "sourceKey": "big.bin", "size": float64(10000), "transferredBytes": transferred, "stage": stage}
		if basis != "" {
			row["basis"] = basis
		}
		if phase != "" {
			row["phase"] = phase
		}
		return row
	}
	result := func(rows ...map[string]interface{}) map[string]interface{} {
		items := []interface{}{}
		for _, row := range rows {
			items = append(items, row)
		}
		return map[string]interface{}{"data": map[string]interface{}{"parentTaskId": 7, "observedAt": "now", "items": items, "basisReported": true}}
	}
	for _, test := range []struct {
		locale cliLocale
		want   []string
	}{
		{localeZH, []string{"99.9%", "传输中", "传输中（等待重试）", "传输中（等待写入请求限速）", "传输中（等待上传缓冲）", "100.0%", "确认中", "目标端已确认的字节"}},
		{localeEN, []string{"99.9%", "Transferring", "Transferring (Waiting to retry)", "Transferring (Waiting for the request limit)", "Transferring (Waiting for the upload buffer)", "Finalizing", "bytes the destination has confirmed"}},
		{localeJA, []string{"99.9%", "転送中", "転送中（再試行待ち）", "転送中（リクエスト制限の待機中）", "転送中（アップロードバッファーの待機中）", "確定中", "転送先が確認したバイト数"}},
	} {
		var output bytes.Buffer
		out := &cliLocaleWriter{Writer: &output, locale: test.locale}
		renderTaskRuntimeResult(out, result(
			file("TRANSFERRING", "TARGET", "", 9999),
			file("TRANSFERRING", "TARGET", "RETRY_WAIT", 1000),
			file("TRANSFERRING", "TARGET", "WAITING_REQUEST_LIMIT", 1000),
			file("TRANSFERRING", "TARGET", "WAITING_UPLOAD_BUFFER", 1000),
			file("FINALIZING", "TARGET", "", 10000),
		))
		for _, want := range test.want {
			if !strings.Contains(output.String(), want) {
				t.Errorf("%s missing %q: %s", test.locale, want, output.String())
			}
		}
		// Only the confirmed file reaches 100%.
		if strings.Count(output.String(), "100.0%") != 1 {
			t.Errorf("%s: %s", test.locale, output.String())
		}
		for _, wrong := range []string{"RETRY_WAIT", "older", "旧版", "旧バージョン"} {
			if strings.Contains(output.String(), wrong) {
				t.Errorf("%s shows %q: %s", test.locale, wrong, output.String())
			}
		}
		output.Reset()
		renderTaskRuntimeResult(out, result(file("FINALIZING", "", "", 10000)))
		for _, want := range map[cliLocale][]string{
			localeZH: {"源端已读完（旧版节点）", "旧版节点的进度按读取源端计算", "写入目标端可能仍在进行"},
			localeEN: {"Source read (older Agent)", "Older Agents count progress from source reads", "writing to the destination may still be in progress"},
			localeJA: {"転送元の読み取り完了（旧バージョン）", "転送元の読み取り量で計算されます", "転送先への書き込みが継続中"},
		}[test.locale] {
			if !strings.Contains(output.String(), want) {
				t.Errorf("%s older Agent missing %q: %s", test.locale, want, output.String())
			}
		}
		if strings.Contains(output.String(), "确认中") || strings.Contains(output.String(), "Finalizing") || strings.Contains(output.String(), "確定中") {
			t.Errorf("%s: an older Agent's read source shows as finalizing: %s", test.locale, output.String())
		}
	}
}

// An older Master drops each file's basis, whichever Agent reported it, so a
// file without one is not marked as an older Agent's: a finalizing file reads
// as read from the source, which is true for every Agent, and no note claims
// what its bytes count.
func TestTaskProgressFromAnOlderMasterClaimsNoBasis(t *testing.T) {
	for _, test := range []struct {
		locale cliLocale
		want   string
		wrong  []string
	}{
		{localeZH, "源端已读完", []string{"旧版", "确认中", "目标端已确认"}},
		{localeEN, "Source read", []string{"older", "Finalizing", "destination has confirmed"}},
		{localeJA, "転送元の読み取り完了", []string{"旧バージョン", "確定中", "転送先が確認"}},
	} {
		var output bytes.Buffer
		out := &cliLocaleWriter{Writer: &output, locale: test.locale}
		row := map[string]interface{}{"childTaskId": "7-1", "executionNodeLabel": "worker", "sourceKey": "big.bin", "size": float64(10000), "transferredBytes": float64(10000), "stage": "FINALIZING"}
		renderTaskRuntimeResult(out, map[string]interface{}{"data": map[string]interface{}{"parentTaskId": 7, "observedAt": "now", "items": []interface{}{row}}})
		if !strings.Contains(output.String(), test.want) {
			t.Errorf("%s missing %q: %s", test.locale, test.want, output.String())
		}
		for _, wrong := range test.wrong {
			if strings.Contains(output.String(), wrong) {
				t.Errorf("%s shows %q: %s", test.locale, wrong, output.String())
			}
		}
	}
}

// The current speed says what it counts once the Master reports it.
func TestTaskShowSpeedSaysWhatItCounts(t *testing.T) {
	task := map[string]interface{}{"taskIndex": 61, "status": "RUNNING", "currentSpeedBytesPerSecond": float64(2 << 20)}
	for basis, want := range map[string]string{
		"TARGET": "2 MiB/s（发往目标端，含失败后重新发送的字节）",
		"SOURCE": "2 MiB/s（执行节点版本较旧，按读取源端计算）",
		"MIXED":  "2 MiB/s（部分执行节点版本较旧，这些节点按读取源端计算）",
		"":       "2 MiB/s",
	} {
		task["transferBasis"] = basis
		var output bytes.Buffer
		renderTaskDetail(&output, task)
		if !strings.Contains(output.String(), want) || basis == "" && strings.Contains(output.String(), "MiB/s（") {
			t.Errorf("%q: %s", basis, output.String())
		}
	}
	task["transferBasis"] = "MIXED"
	var output bytes.Buffer
	renderTaskDetail(&cliLocaleWriter{Writer: &output, locale: localeEN}, task)
	if !strings.Contains(output.String(), "2 MiB/s (some Agents are older and count source reads)") {
		t.Errorf("English speed basis: %s", output.String())
	}
	output.Reset()
	renderTaskDetail(&cliLocaleWriter{Writer: &output, locale: localeJA}, task)
	if !strings.Contains(output.String(), "それらのノードは転送元の読み取り量で計算") {
		t.Errorf("Japanese speed basis: %s", output.String())
	}
}
