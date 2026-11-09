package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func taskNodesTestView(local bool) map[string]interface{} {
	direction := "OSS_TO_OSS"
	if local {
		direction = "LOCAL_TO_OSS"
	}
	return map[string]interface{}{
		"taskId": 12, "generation": 2, "nodeRevision": 3, "status": "RUNNING", "direction": direction,
		"localTransfer": local, "editable": true, "reason": "", "nowSupported": true, "nowReason": "", "pending": true,
		"bandwidthLimitMbps": 8,
		"nodes": []interface{}{
			map[string]interface{}{"id": "node-a", "address": "10.0.0.1", "displayName": "alpha", "displayAddress": "10.0.0.1", "state": "ACTIVE", "drainMode": "", "scanner": true, "online": true, "localPath": "/data/a", "pendingChildren": 4,
				"share": map[string]interface{}{"bandwidthBytesPerSecond": 500000, "listQps": 0, "headQps": 25, "putQps": 0}},
			map[string]interface{}{"id": "node-b", "address": "10.0.0.2", "displayName": "beta", "displayAddress": "10.0.0.2", "state": "DRAINING", "drainMode": "drain", "scanner": false, "online": true, "pendingChildren": 1,
				"share": map[string]interface{}{"bandwidthBytesPerSecond": 500000, "listQps": 0, "headQps": 25, "putQps": 0}},
			map[string]interface{}{"id": "node-c", "address": "10.0.0.4", "displayName": "gamma", "displayAddress": "10.0.0.4", "state": "JOINING", "scanner": false, "online": true, "share": nil},
		},
		"removedNodes": []interface{}{map[string]interface{}{"id": "node-z", "address": "10.0.0.9", "displayLabel": "zeta (10.0.0.9)", "removedAt": int64(1730000000000)}},
		"candidates": []interface{}{
			map[string]interface{}{"id": "node-d", "address": "10.0.0.3", "displayName": "delta", "displayAddress": "10.0.0.3", "eligible": true, "reason": ""},
			map[string]interface{}{"id": "node-e", "address": "10.0.0.5", "displayName": "epsilon", "displayAddress": "10.0.0.5", "eligible": false, "reason": "Agent 版本过旧"},
		},
	}
}

// taskNodesFakeMaster records every request; posts answer through respond.
type taskNodesFakeMaster struct {
	mu      sync.Mutex
	view    map[string]interface{}
	posts   []map[string]interface{}
	gets    int
	respond func(body map[string]interface{}) (int, interface{})
}

func (m *taskNodesFakeMaster) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.URL.Path != "/api/master/tasks/12/nodes" {
			t.Errorf("unexpected path %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		switch r.Method {
		case "GET":
			m.gets++
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": m.view})
		case "POST":
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			m.posts = append(m.posts, body)
			status, response := 200, interface{}(nil)
			if m.respond != nil {
				status, response = m.respond(body)
			}
			if response == nil {
				response = map[string]interface{}{"success": true, "data": map[string]interface{}{"accepted": body["dryRun"] != true, "dryRun": body["dryRun"] == true, "replayed": false, "valid": true, "view": m.view}}
			}
			w.WriteHeader(status)
			adminTestJSON(t, w, response)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}
}

func renderTaskNodesForTest(result map[string]interface{}, locale cliLocale) string {
	var buffer bytes.Buffer
	renderCLIResult(&cliLocaleWriter{Writer: &buffer, locale: locale}, result)
	return buffer.String()
}

func TestTaskNodesShowsMembershipTableAndJSON(t *testing.T) {
	master := &taskNodesFakeMaster{view: taskNodesTestView(false)}
	client := adminTestClient(t, master.handler(t))
	result, err := runTaskCommand(client, []string{"nodes", "12"})
	if err != nil || len(master.posts) != 0 || master.gets != 1 {
		t.Fatal("membership query must be one GET", err, master.posts, master.gets)
	}
	text := renderTaskNodesForTest(result, localeZH)
	for _, want := range []string{"任务 #12 执行节点（成员版本 3，RUNNING）", "alpha (10.0.0.1)", "执行中", "移出中（等待当前工作完成）", "加入中", "4 Mbps", "不限 / 25 / 不限", "有节点正在加入或移出", "已移出的节点", "zeta (10.0.0.9)", "可加入的 Agent", "delta (10.0.0.3)", "Agent 版本过旧"} {
		if !strings.Contains(text, want) {
			t.Errorf("membership output missing %q:\n%s", want, text)
		}
	}
	english := renderTaskNodesForTest(result, localeEN)
	for _, want := range []string{"Task #12 execution nodes (membership revision 3, RUNNING)", "Draining", "Joining", "Agents available to add", "Agent 版本过旧"} {
		if !strings.Contains(english, want) {
			t.Errorf("English membership output missing %q:\n%s", want, english)
		}
	}

	code, out, stderr := runMockCLI(t, []string{"--json", "task", "nodes", "12"}, master.handler(t))
	var decoded map[string]interface{}
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &decoded) != nil {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	data, _ := decoded["data"].(map[string]interface{})
	if data["nodeRevision"] != float64(3) || len(objectList(data["candidates"])) != 2 || len(objectList(data["nodes"])) != 3 {
		t.Fatalf("JSON must pass the membership view through: %s", out)
	}
	if !watchable([]string{"task", "nodes", "12"}) || watchable([]string{"task", "nodes", "12", "--add", "10.0.0.3"}) {
		t.Fatal("only the read-only membership query may be watched")
	}
}

func TestTaskNodesSubmitsChangesWithRevisionModeAndRequestID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		add    []interface{}
		remove []interface{}
		mode   string
	}{
		{"add", []string{"--add", "10.0.0.3,10.0.0.5", "--add", "10.0.0.3"}, []interface{}{"10.0.0.3", "10.0.0.5"}, []interface{}{}, "drain"},
		{"remove drain", []string{"--remove", "10.0.0.2"}, []interface{}{}, []interface{}{"10.0.0.2"}, "drain"},
		{"remove now", []string{"--remove", "node-b", "--now"}, []interface{}{}, []interface{}{"node-b"}, "now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master := &taskNodesFakeMaster{view: taskNodesTestView(false)}
			client := adminTestClient(t, master.handler(t))
			result, err := runTaskCommand(client, append(append([]string{"nodes", "12"}, tc.args...), "--yes"))
			if err != nil || master.gets != 1 || len(master.posts) != 1 {
				t.Fatal(err, master.gets, master.posts)
			}
			body := master.posts[0]
			requestID, _ := body["requestId"].(string)
			if validateCreationRequestID(requestID) != nil || !strings.HasPrefix(requestID, "cli-") {
				t.Fatalf("missing generated request ID: %v", body)
			}
			want := map[string]interface{}{"requestId": requestID, "add": tc.add, "remove": tc.remove, "mode": tc.mode, "expectedNodeRevision": float64(3)}
			if !reflect.DeepEqual(body, want) {
				t.Fatalf("body = %#v\nwant %#v", body, want)
			}
			if result["requestId"] != requestID {
				t.Fatal("result must report the request ID")
			}
			text := renderTaskNodesForTest(result, localeZH)
			if !strings.Contains(text, "已受理执行节点变更（请求标识 "+requestID+"）") || !strings.Contains(text, "任务 #12 执行节点") {
				t.Fatal(text)
			}
		})
	}
}

func TestTaskNodesRetryReusesRequestIdentityAndRevision(t *testing.T) {
	master := &taskNodesFakeMaster{view: taskNodesTestView(false)}
	master.view["nodeRevision"] = 4
	client := adminTestClient(t, master.handler(t))
	if _, err := runTaskCommand(client, []string{"nodes", "12", "--add", "10.0.0.3", "--request-id", "cli-retry-1", "--expected-revision", "3", "--yes"}); err != nil {
		t.Fatal(err)
	}
	body := master.posts[0]
	if body["requestId"] != "cli-retry-1" || body["expectedNodeRevision"] != float64(3) {
		t.Fatalf("retry must resend the original identity and revision: %v", body)
	}
}

func TestTaskNodesUncertainSubmissionPrintsRetryIdentity(t *testing.T) {
	view := taskNodesTestView(false)
	client := &ApiClient{baseUrl: "http://admin-cli-test.invalid/api/master", client: &http.Client{Transport: adminTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method == "POST" {
			return nil, io.ErrUnexpectedEOF
		}
		body, _ := json.Marshal(map[string]interface{}{"success": true, "data": view})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}}
	result, err := runTaskCommand(client, []string{"nodes", "12", "--remove", "10.0.0.2", "--yes"})
	requestID, _ := result["requestId"].(string)
	if err == nil || requestID == "" || !strings.Contains(err.Error(), "--request-id "+requestID+" --expected-revision 3") {
		t.Fatal(result, err)
	}
}

func TestTaskNodesRequiresConfirmationWithoutSubmitting(t *testing.T) {
	master := &taskNodesFakeMaster{view: taskNodesTestView(false)}
	code, out, stderr := runMockCLI(t, []string{"task", "nodes", "12", "--remove", "10.0.0.2", "--now"}, master.handler(t))
	if code != 1 || len(master.posts) != 0 || !strings.Contains(stderr, "--yes") {
		t.Fatalf("unconfirmed change must not be submitted: %d %v %s", code, master.posts, stderr)
	}
	if !strings.Contains(out, "立即移出（撤回运行中的工作）") || !strings.Contains(out, "10.0.0.2") || !strings.Contains(out, "基于成员版本 3") {
		t.Fatalf("confirmation summary missing: %s", out)
	}
	code, out, _ = runMockCLI(t, []string{"--json", "task", "nodes", "12", "--add", "10.0.0.3"}, master.handler(t))
	var decoded map[string]interface{}
	if code != 1 || len(master.posts) != 0 || json.Unmarshal([]byte(out), &decoded) != nil || decoded["success"] != false {
		t.Fatalf("%d %s", code, out)
	}
	plan, _ := decoded["taskNodesPlan"].(map[string]interface{})
	if plan["mode"] != "drain" || !reflect.DeepEqual(plan["add"], []interface{}{"10.0.0.3"}) {
		t.Fatalf("JSON summary = %s", out)
	}
}

func TestTaskNodesLocalBindingPreflight(t *testing.T) {
	binding := []interface{}{map[string]interface{}{"executionNodeIp": "10.0.0.3", "localPath": "/mnt/share,1"}}
	check := func(valid bool) map[string]interface{} {
		return map[string]interface{}{"executionNodeIp": "10.0.0.3", "localPath": "/mnt/share,1", "displayName": "delta", "displayAddress": "10.0.0.3", "valid": valid, "pathType": map[bool]string{true: "DIRECTORY", false: "MISSING"}[valid], "message": map[bool]string{true: "", false: "目录不存在"}[valid]}
	}
	t.Run("pass", func(t *testing.T) {
		master := &taskNodesFakeMaster{view: taskNodesTestView(true)}
		master.respond = func(body map[string]interface{}) (int, interface{}) {
			return 200, map[string]interface{}{"success": true, "data": map[string]interface{}{"accepted": body["dryRun"] != true, "dryRun": body["dryRun"] == true, "valid": true, "nodeBindings": []interface{}{check(true)}, "view": master.view}}
		}
		client := adminTestClient(t, master.handler(t))
		result, err := runTaskCommand(client, []string{"nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.3=/mnt/share,1", "--yes"})
		if err != nil || len(master.posts) != 2 {
			t.Fatal(err, master.posts)
		}
		dry, real := master.posts[0], master.posts[1]
		if dry["dryRun"] != true || dry["requestId"] != nil || !reflect.DeepEqual(dry["bindings"], binding) {
			t.Fatalf("dry run body = %v", dry)
		}
		if real["dryRun"] != nil || real["requestId"] == nil || !reflect.DeepEqual(real["bindings"], binding) || real["expectedNodeRevision"] != float64(3) {
			t.Fatalf("submission body = %v", real)
		}
		if text := renderTaskNodesForTest(result, localeZH); !strings.Contains(text, "通过") || !strings.Contains(text, "DIRECTORY") || !strings.Contains(text, "本地路径") {
			t.Fatal(text)
		}
		// Without --yes the preflight still runs and its results are shown.
		master.posts = nil
		code, out, _ := runMockCLI(t, []string{"task", "nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.3=/mnt/share,1"}, master.handler(t))
		if code != 1 || len(master.posts) != 1 || master.posts[0]["dryRun"] != true || !strings.Contains(out, "节点共享目录预检") || !strings.Contains(out, "目录绑定") {
			t.Fatalf("%d %v %s", code, master.posts, out)
		}
	})
	t.Run("fail", func(t *testing.T) {
		master := &taskNodesFakeMaster{view: taskNodesTestView(true)}
		master.respond = func(body map[string]interface{}) (int, interface{}) {
			return 200, map[string]interface{}{"success": true, "data": map[string]interface{}{"accepted": false, "dryRun": true, "valid": false, "nodeBindings": []interface{}{check(false)}, "validationErrors": []interface{}{"共享目录不一致"}, "view": master.view}}
		}
		code, out, stderr := runMockCLI(t, []string{"task", "nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.3=/mnt/share,1", "--yes"}, master.handler(t))
		if code != 1 || len(master.posts) != 1 || master.posts[0]["dryRun"] != true {
			t.Fatalf("failed preflight must stop before submission: %d %v", code, master.posts)
		}
		for _, want := range []string{"未通过", "MISSING", "目录不存在", "共享目录不一致"} {
			if !strings.Contains(out, want) {
				t.Errorf("preflight output missing %q: %s", want, out)
			}
		}
		if !strings.Contains(stderr, "预检未通过") {
			t.Fatal(stderr)
		}
	})
}

func TestTaskNodesUsageErrorsExitTwoBeforeAnyRequest(t *testing.T) {
	refuse := func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("usage error made a request: %s %s", r.Method, r.URL.Path)
	}
	for _, args := range [][]string{
		{"task", "nodes"},
		{"task", "nodes", "12", "13"},
		{"task", "nodes", "abc"},
		{"task", "nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.3:/mnt"},
		{"task", "nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.3=relative"},
		{"task", "nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.4=/mnt"},
		{"task", "nodes", "12", "--binding", "10.0.0.4=/mnt"},
		{"task", "nodes", "12", "--add", "10.0.0.3", "--remove", "10.0.0.1,10.0.0.3"},
		{"task", "nodes", "12", "--add", "10.0.0.3", "--unknown"},
		{"task", "nodes", "12", "--add", "10.0.0.3", "--now"},
		{"task", "nodes", "12", "--now"},
		{"task", "nodes", "12", "--yes"},
		{"task", "nodes", "12", "--add", ","},
		{"task", "nodes", "12", "--add"},
		{"task", "nodes", "12", "--remove", "10.0.0.2", "--expected-revision", "-1"},
	} {
		code, _, stderr := runMockCLI(t, args, refuse)
		if code != 2 || stderr == "" {
			t.Errorf("%v exit %d, want 2: %s", args, code, stderr)
		}
	}
}

func TestTaskNodesLocalBindingRulesUseMembershipView(t *testing.T) {
	for _, tc := range []struct {
		local bool
		args  []string
	}{
		{true, []string{"task", "nodes", "12", "--add", "10.0.0.3,10.0.0.5", "--binding", "10.0.0.3=/mnt"}},
		{false, []string{"task", "nodes", "12", "--add", "10.0.0.3", "--binding", "10.0.0.3=/mnt", "--yes"}},
	} {
		master := &taskNodesFakeMaster{view: taskNodesTestView(tc.local)}
		code, _, stderr := runMockCLI(t, tc.args, master.handler(t))
		if code != 2 || len(master.posts) != 0 || master.gets != 1 {
			t.Errorf("%v: %d %v %s", tc.args, code, master.posts, stderr)
		}
	}
}

func TestTaskNodesRejectedChangePropagatesServerMessage(t *testing.T) {
	master := &taskNodesFakeMaster{view: taskNodesTestView(false)}
	master.respond = func(map[string]interface{}) (int, interface{}) {
		return 409, map[string]interface{}{"success": false, "code": "CHANGE_PENDING", "message": "另一项节点变更仍在进行"}
	}
	code, _, stderr := runMockCLI(t, []string{"task", "nodes", "12", "--add", "10.0.0.3", "--yes"}, master.handler(t))
	if code != 1 || !strings.Contains(stderr, "另一项节点变更仍在进行") || len(master.posts) != 1 {
		t.Fatalf("%d %s", code, stderr)
	}
	code, out, _ := runMockCLI(t, []string{"--json", "task", "nodes", "12", "--remove", "10.0.0.2", "--yes"}, master.handler(t))
	var decoded map[string]interface{}
	if code != 1 || json.Unmarshal([]byte(out), &decoded) != nil || decoded["code"] != "CHANGE_PENDING" || !strings.Contains(getString(decoded, "error"), "另一项节点变更仍在进行") || getString(decoded, "requestId") == "" {
		t.Fatalf("%d %s", code, out)
	}
}

func TestTaskNodesRefusesUneditableTaskAndUnsupportedNow(t *testing.T) {
	for _, tc := range []struct {
		edit func(map[string]interface{})
		args []string
		want string
	}{
		{func(v map[string]interface{}) { v["editable"], v["reason"] = false, "任务已完成" }, []string{"--add", "10.0.0.3", "--yes"}, "任务已完成"},
		{func(v map[string]interface{}) { v["nowSupported"], v["nowReason"] = false, "旧版 Agent 不支持" }, []string{"--remove", "10.0.0.2", "--now", "--yes"}, "旧版 Agent 不支持"},
	} {
		master := &taskNodesFakeMaster{view: taskNodesTestView(false)}
		tc.edit(master.view)
		code, _, stderr := runMockCLI(t, append([]string{"task", "nodes", "12"}, tc.args...), master.handler(t))
		if code != 1 || len(master.posts) != 0 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v: %d %v %s", tc.args, code, master.posts, stderr)
		}
	}
}

func TestTaskShowRendersExecutionMembershipWhenPresent(t *testing.T) {
	base := map[string]interface{}{"taskIndex": 12, "direction": "OSS_TO_OSS", "status": "RUNNING", "sourceBucket": "src", "destBucket": "dst", "eligibleWorkerNodes": []interface{}{"10.0.0.1", "10.0.0.2"}}
	var legacy bytes.Buffer
	renderTaskDetail(&cliLocaleWriter{Writer: &legacy, locale: localeZH}, base)
	if strings.Contains(legacy.String(), "执行节点成员") {
		t.Fatal("older Master output changed")
	}
	task := map[string]interface{}{}
	for key, value := range base {
		task[key] = value
	}
	view := taskNodesTestView(false)
	task["executionMembership"] = map[string]interface{}{"nodeRevision": 3, "pending": true, "nodes": view["nodes"], "joining": []interface{}{"10.0.0.4"}, "draining": []interface{}{"10.0.0.2"}}
	var buffer bytes.Buffer
	renderTaskDetail(&cliLocaleWriter{Writer: &buffer, locale: localeZH}, task)
	text := buffer.String()
	if !strings.Contains(text, "平均速度") || strings.Index(text, "执行节点成员") < strings.Index(text, "平均速度") {
		t.Fatalf("membership must follow the existing details:\n%s", text)
	}
	for _, want := range []string{"执行节点成员（成员版本 3）", "beta (10.0.0.2)", "移出中（等待当前工作完成）", "加入中", "4 Mbps", "有节点正在加入或移出"} {
		if !strings.Contains(text, want) {
			t.Errorf("task show missing %q:\n%s", want, text)
		}
	}

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/master/tasks/12":
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": task})
		case "/api/master/tasks/12/children":
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"items": []interface{}{}}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}
	code, out, stderr := runMockCLI(t, []string{"--json", "task", "show", "12"}, handler)
	var decoded map[string]interface{}
	if code != 0 || json.Unmarshal([]byte(out), &decoded) != nil {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	row, _ := decoded["task"].(map[string]interface{})
	membership, _ := row["executionMembership"].(map[string]interface{})
	if membership["nodeRevision"] != float64(3) || !reflect.DeepEqual(membership["draining"], []interface{}{"10.0.0.2"}) || len(objectList(membership["nodes"])) != 3 {
		t.Fatalf("task show JSON must keep executionMembership: %s", out)
	}
}

// The node table shows each node's demand: busy nodes share the task limits,
// idle ones keep a small floor, and a node still confirming its reduction is
// adjusting. An older Master reports no demand.
func TestTaskNodesShowsDemandColumn(t *testing.T) {
	view := taskNodesTestView(false)
	nodes := view["nodes"].([]interface{})
	busy, idle := nodes[0].(map[string]interface{}), nodes[1].(map[string]interface{})
	busy["demand"], busy["adjusting"] = "busy", false
	idle["demand"], idle["adjusting"] = "idle", true
	master := &taskNodesFakeMaster{view: view}
	client := adminTestClient(t, master.handler(t))
	result, err := runTaskCommand(client, []string{"nodes", "12"})
	if err != nil {
		t.Fatal(err)
	}
	for locale, wants := range map[cliLocale][]string{
		localeZH: {"需求", "按需（忙）", "空闲（已释放），调整中：等待节点确认降额"},
		localeEN: {"Demand", "On demand (busy)", "Idle (released), adjusting: waiting for the node to confirm its reduction"},
		localeJA: {"需要", "需要に応じて（ビジー）", "アイドル（解放済み）"},
	} {
		text := renderTaskNodesForTest(result, locale)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%v output missing %q:\n%s", locale, want, text)
			}
		}
	}
	delete(busy, "demand")
	if got := taskNodeDemand(io.Discard, busy); got != "—" {
		t.Fatal("an older Master's equal split has no demand", got)
	}
}

// A stopped or finished task's nodes no longer execute or share its limits, so
// the table names that state and shows no demand or share.
func TestTaskNodesOfInactiveTaskShowNoRuntimeShare(t *testing.T) {
	for status, wants := range map[string]map[cliLocale]string{
		"STOPPED":        {localeZH: "已停止", localeEN: "Stopped", localeJA: "停止済み"},
		"FAILED":         {localeZH: "已结束", localeEN: "Finished", localeJA: "終了"},
		"COMPLETED":      {localeZH: "已结束", localeEN: "Finished", localeJA: "終了"},
		"PARTIAL_FAILED": {localeZH: "已结束", localeEN: "Finished", localeJA: "終了"},
	} {
		view := taskNodesTestView(false)
		view["status"], view["pending"] = status, false
		for _, raw := range view["nodes"].([]interface{}) {
			raw.(map[string]interface{})["demand"] = "busy"
		}
		master := &taskNodesFakeMaster{view: view}
		result, err := runTaskCommand(adminTestClient(t, master.handler(t)), []string{"nodes", "12"})
		if err != nil {
			t.Fatal(err)
		}
		for locale, want := range wants {
			text := renderTaskNodesForTest(result, locale)
			if strings.Contains(text, "0.5 Mbps") || strings.Contains(text, "4 Mbps") {
				t.Errorf("%s %v output still shows a bandwidth share:\n%s", status, locale, text)
			}
			for _, stale := range []string{"执行中", "按需（忙）", "Active", "On demand (busy)", "移出中", "Draining"} {
				if strings.Contains(text, stale) {
					t.Errorf("%s %v output still shows runtime state %q:\n%s", status, locale, stale, text)
				}
			}
			if strings.Count(text, want) < 3 {
				t.Errorf("%s %v output must mark each node %q:\n%s", status, locale, want, text)
			}
		}
	}
	// A running task keeps its live state and shares.
	view := taskNodesTestView(false)
	master := &taskNodesFakeMaster{view: view}
	result, err := runTaskCommand(adminTestClient(t, master.handler(t)), []string{"nodes", "12"})
	if err != nil {
		t.Fatal(err)
	}
	if text := renderTaskNodesForTest(result, localeZH); !strings.Contains(text, "执行中") || !strings.Contains(text, "Mbps") {
		t.Fatalf("running task lost its live node state:\n%s", text)
	}
}

func TestTaskShowMembershipOfStoppedTaskShowsNoRuntimeShare(t *testing.T) {
	var buffer bytes.Buffer
	view := taskNodesTestView(false)
	renderTaskMembership(&cliLocaleWriter{Writer: &buffer, locale: localeZH}, map[string]interface{}{
		"status": "STOPPED", "executionMembership": map[string]interface{}{"nodeRevision": 3, "nodes": view["nodes"]},
	})
	if text := buffer.String(); strings.Contains(text, "Mbps") || strings.Contains(text, "执行中") || !strings.Contains(text, "已停止") {
		t.Fatalf("task show membership of a stopped task:\n%s", text)
	}
}
