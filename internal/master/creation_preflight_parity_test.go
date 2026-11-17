package master

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// overrideAgent is a current, writable test Agent (see readOnlyAgent) that
// may answer some commands itself.
type overrideAgent struct {
	readOnlyAgent
	answer func(agenttransport.Command) (agenttransport.Response, bool)
}

func (a overrideAgent) Execute(ctx context.Context, command agenttransport.Command) (agenttransport.Response, error) {
	if response, ok := a.answer(command); ok {
		return response, nil
	}
	return a.readOnlyAgent.Execute(ctx, command)
}

func overrideTestAgent(t *testing.T, f *agentHTTPFixture, number int, answer func(agenttransport.Command) (agenttransport.Response, bool)) task.Node {
	t.Helper()
	var calls atomic.Int64
	return connectTestAgent(t, f, number, func(service *agentservice.Service) agenttransport.Executor {
		return overrideAgent{readOnlyAgent{Service: service, write: "writable", fingerprints: &calls}, answer}
	})
}

func agentReply(status int, value any) (agenttransport.Response, bool) {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return agenttransport.Response{Status: status, Payload: raw}, true
}

func labelAgent(t *testing.T, f *agentHTTPFixture, node task.Node, label string) {
	t.Helper()
	if _, err := f.master.agents.registry.UpdateLabel(node.ID, label); err != nil {
		t.Fatal(err)
	}
}

// removeOnCapabilities removes the Agent at target through the API when the
// Agent it answers for is next asked for its capabilities, and sends the
// removal's HTTP status (0 when the request failed).
func removeOnCapabilities(f *agentHTTPFixture, target *atomic.Pointer[string], removed chan<- int) func(agenttransport.Command) (agenttransport.Response, bool) {
	return func(command agenttransport.Command) (agenttransport.Response, bool) {
		if command.Operation != "capabilities" {
			return agenttransport.Response{}, false
		}
		if address := target.Swap(nil); address != nil {
			status, _, _, err := localizedRequest(f.http.Client(), "DELETE", f.http.URL+"/api/master/nodes/"+*address, f.admin, "", nil)
			if err != nil {
				status = 0
			}
			removed <- status
		}
		return agenttransport.Response{}, false
	}
}

// The creation preflight runs creation's checks that write nothing, so that a
// passed preflight is not refused by creating the task.
func TestCreationPreflightRunsTheChecksOfCreation(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := sharedTestAgents(t, f, &calls, "writable", "writable")
	shared := t.TempDir()
	row := uploadRow(taskBinding{agents[0].node.Address, shared}, taskBinding{agents[1].node.Address, shared})
	refusal := func(path, language string, row taskCreateRequest) string {
		t.Helper()
		status, _, response, err := localizedRequest(f.http.Client(), "POST", f.http.URL+path, f.admin, language, []taskCreateRequest{row})
		data, _ := response["data"].(map[string]any)
		problems, _ := data["validationErrors"].([]any)
		if err != nil || status != 200 || len(problems) != 1 || path == "/api/master/local-transfers/validate" && data["valid"] != false || path == "/api/master/local-transfers" && data["acceptedCount"] != float64(0) {
			t.Fatal("the row was not refused", path, status, response, err)
		}
		// The preflight numbers a refused row as creation does.
		problem, numbered := strings.CutPrefix(fmt.Sprint(problems[0]), "1: ")
		if !numbered {
			t.Fatal("the refusal does not name its row", path, problems)
		}
		return problem
	}
	narrow := row
	narrow.RequestLimits = &execution.RequestLimits{Scope: "CLUSTER", BandwidthBitsPerSecond: 8}
	invalid := row
	invalid.DestinationEndpoint = &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "two words"}
	for _, path := range []string{"/api/master/local-transfers/validate", "/api/master/local-transfers"} {
		for _, test := range []struct {
			row      taskCreateRequest
			language string
			want     string
		}{
			{row, "en-US", "storage connection qa does not exist"},
			{row, "zh-CN", "存储连接 qa 不存在"},
		} {
			if got := refusal(path, test.language, test.row); got != test.want {
				t.Fatal(path, got)
			}
		}
	}
	createComparisonStorage(t, f)
	for _, path := range []string{"/api/master/local-transfers/validate", "/api/master/local-transfers"} {
		for _, test := range []struct {
			row      taskCreateRequest
			language string
			want     string
		}{
			{invalid, "zh-CN", "存储桶名称无效"},
			{narrow, "", "bandwidth limit must provide at least one byte per second per selected Agent"},
			{narrow, "zh-CN", "带宽上限过低，无法为每个所选 Agent 分配至少每秒 1 字节"},
		} {
			if got := refusal(path, test.language, test.row); got != test.want {
				t.Fatal(path, got)
			}
		}
	}
	if data := validateRows(t, f, row); data["valid"] != true {
		t.Fatal("a valid row failed the preflight", data)
	}
	if parents, _, err := f.master.allTaskParents(); err != nil || len(parents) != 0 {
		t.Fatal("a refused row created a task", parents, err)
	}
}

// Creating a task re-checks its Agents the same way; the refusal is in the
// request's language.
func TestCreationRecheckRefusesARemovedAgentInTheRequestLanguage(t *testing.T) {
	f := newAgentHTTPFixture(t)
	createComparisonStorage(t, f)
	var calls atomic.Int64
	first := sharedTestAgents(t, f, &calls, "writable")[0].node
	labelAgent(t, f, first, "First worker")
	var target atomic.Pointer[string]
	removed := make(chan int, 1)
	// With OVERWRITE_CHANGED the preflight ends with each Agent's
	// capabilities, this one's last.
	second := overrideTestAgent(t, f, 2, removeOnCapabilities(f, &target, removed))
	shared := t.TempDir()
	row := taskCreateRequest{Direction: "OSS_TO_LOCAL", ConflictPolicy: execution.ConflictOverwriteChanged,
		SourceEndpoint:      &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "source"},
		DestinationEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{first.Address, shared}, {second.Address, shared}}}}
	target.Store(&first.Address)
	status, _, response, err := localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers", f.admin, "zh-CN", []taskCreateRequest{row})
	select {
	case code := <-removed:
		if code != http.StatusOK {
			t.Fatal("the Agent could not be removed during the preflight", code)
		}
	default:
		t.Fatal("the Agent was not removed during the preflight")
	}
	data, _ := response["data"].(map[string]any)
	if err != nil || status != 200 || data["acceptedCount"] != float64(0) || fmt.Sprint(data["validationErrors"]) != "[1: 执行节点 First worker (127.0.0.1) 未在线]" {
		t.Fatal("a task was created on a removed Agent, or the refusal is not translated", status, response, err)
	}
}
