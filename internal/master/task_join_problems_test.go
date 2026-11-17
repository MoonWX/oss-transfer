package master

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func joiningParent(id int64, joins map[string]time.Time) task.Parent {
	return task.Parent{ID: id, Generation: 1, Status: "RUNNING", Membership: &task.MembershipState{Joining: joins}}
}

// The record keeps the latest problem of each join; each distinct problem is
// logged once per join attempt (a node removed and added again starts a new
// one), and joins that ended or tasks that left keep nothing.
func TestJoinProblemsKeepTheLatestProblemOfEachJoin(t *testing.T) {
	var joins joinProblems
	requested := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	p := joiningParent(7, map[string]time.Time{"b": requested})
	offline := newJoinProblem(joinAgentOffline)
	unverified := newJoinProblem(joinStorageUnverified, "marker missing")
	for i, step := range []struct {
		problem joinProblem
		log     joinLog
	}{
		{offline, joinLogProblem}, {offline, joinLogNothing}, {unverified, joinLogProblem},
		{unverified, joinLogNothing}, {offline, joinLogNothing},
	} {
		if got := joins.note(p, "b", step.problem); got != step.log {
			t.Fatalf("step %d: log %v, want %v", i, got, step.log)
		}
		if latest, ok := joins.latest(p, "b"); !ok || !reflect.DeepEqual(latest, step.problem) {
			t.Fatalf("step %d: latest problem %v %t", i, latest, ok)
		}
	}
	if joins.note(p, "c", offline) != joinLogNothing {
		t.Fatal("a node that does not join was recorded")
	}
	if _, ok := joins.latest(p, "c"); ok {
		t.Fatal("a node that does not join has a problem")
	}

	again := joiningParent(7, map[string]time.Time{"b": requested.Add(time.Minute)})
	if _, ok := joins.latest(again, "b"); ok {
		t.Fatal("a node added again shows the problem of its cancelled join")
	}
	if joins.note(again, "b", offline) != joinLogProblem {
		t.Fatal("the problem of a new join was not logged")
	}
	joins.retain(joiningParent(7, nil))
	if len(joins.tasks) != 0 {
		t.Fatal("a cancelled join kept its problem", joins.tasks)
	}

	for i := range maxLoggedJoinProblems + 2 {
		want := joinLogProblem
		if i == maxLoggedJoinProblems {
			want = joinLogMuted
		} else if i > maxLoggedJoinProblems {
			want = joinLogNothing
		}
		if got := joins.note(p, "b", newJoinProblem(joinStorageUnverified, fmt.Sprint("detail ", i))); got != want {
			t.Fatalf("problem %d: log %v, want %v", i, got, want)
		}
	}
	if latest, _ := joins.latest(p, "b"); latest.Args[0] != fmt.Sprint("detail ", maxLoggedJoinProblems+1) {
		t.Fatal("a problem that is not logged is not the latest", latest)
	}
	if !joins.clear(7, "b") || joins.clear(7, "b") || len(joins.tasks) != 0 {
		t.Fatal("a joined node kept its problem", joins.tasks)
	}

	other := joiningParent(8, map[string]time.Time{"x": requested})
	joins.note(p, "b", offline)
	joins.note(other, "x", offline)
	joins.prune(map[int64]bool{8: true})
	if _, ok := joins.latest(p, "b"); ok {
		t.Fatal("a task that left the active list kept its problems")
	}
	if _, ok := joins.latest(other, "x"); !ok {
		t.Fatal("an active task lost its problems")
	}
	joins.forget(8)
	if len(joins.tasks) != 0 {
		t.Fatal("a forgotten task kept its problems", joins.tasks)
	}
}

// Every reason has a source text and its translations, with the same verbs;
// the first names the joining node.
func TestJoinProblemMessagesAreTranslated(t *testing.T) {
	verbs := regexp.MustCompile(`%[a-z]`)
	for code, source := range joinProblemMessages {
		entry, ok := apiMessages[source]
		if !ok {
			t.Fatalf("%s: %q has no translations", code, source)
		}
		want := verbs.FindAllString(source, -1)
		if len(want) == 0 || want[0] != "%s" || entry.en != source {
			t.Fatalf("%s: %q", code, source)
		}
		for _, text := range []string{entry.zh, entry.ja} {
			if text == "" || !reflect.DeepEqual(verbs.FindAllString(text, -1), want) {
				t.Errorf("%s: %q does not translate %q", code, text, source)
			}
		}
		if strings.ContainsAny(entry.zh, "—\"") {
			t.Errorf("%s: %q uses an em dash or straight quotes", code, entry.zh)
		}
	}
	zh := context.WithValue(context.Background(), responseLocaleKey{}, "zh-CN")
	if got := newJoinProblem(joinStorageUnverified, "marker missing").message(zh, "B"); got != "执行节点 B 未通过共享目录校验：marker missing" {
		t.Fatal(got)
	}
	if got := newJoinProblem(joinTaskNotRunning).message(context.Background(), "B"); got != "The task is not running; Agent B joins after the task runs again" {
		t.Fatal(got)
	}
}

// joinProblemView returns the joinProblem of a node in the task's node view.
func joinProblemView(t *testing.T, f *agentHTTPFixture, id int64, language string, node task.Node) map[string]any {
	t.Helper()
	status, _, body, err := localizedRequest(f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/nodes", f.http.URL, id), f.admin, language, nil)
	if err != nil || status != 200 {
		t.Fatal(status, body, err)
	}
	for _, raw := range body["data"].(map[string]any)["nodes"].([]any) {
		if row := raw.(map[string]any); row["id"] == node.ID {
			problem, _ := row["joinProblem"].(map[string]any)
			return problem
		}
	}
	t.Fatal("the node is not in the view", node.ID, body)
	return nil
}

func expectJoinProblem(t *testing.T, f *agentHTTPFixture, id int64, language string, node task.Node, code, message string) {
	t.Helper()
	problem := joinProblemView(t, f, id, language, node)
	if problem["code"] != code || problem["message"] != message {
		t.Fatalf("%s join problem of %s: %v, want %s %q", language, node.Address, problem, code, message)
	}
}

// joinLogLines counts the log lines that report one problem of a task.
func joinLogLines(logs *capturedLog, p task.Parent, message string) int {
	return len(logs.matching(fmt.Sprintf("task %d generation %d: a joining node has not joined yet: %s", p.ID, p.Generation, message)))
}

// A join that keeps failing says why in the node view, in the request's
// language, through the steps that hold it up: takeover not set up, the
// shared directory check, the lease handshake, a problem of the task. Each
// reason is logged once per join, and nothing is left once the node joins
// or its join is cancelled.
func TestJoinProblemsExplainAJoinThatKeepsFailing(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	current := sharedTestAgents(t, f, &calls, "writable")[0].node
	var mode atomic.Value
	mode.Store("")
	joiner := overrideTestAgent(t, f, 2, func(command agenttransport.Command) (agenttransport.Response, bool) {
		switch {
		case mode.Load() == "unverified" && command.Operation == "shared_probe_verify":
			return agentReply(500, map[string]string{"code": "operation_failed", "message": "shared probe marker is missing"})
		case mode.Load() == "lease" && command.Operation == "lease_rejoin":
			return agentReply(500, map[string]string{"code": "operation_failed", "message": "lease store is unavailable"})
		}
		return agenttransport.Response{}, false
	})
	labelAgent(t, f, current, "Member A")
	labelAgent(t, f, joiner, "Joiner B")
	shared := t.TempDir()
	p := localTask(t, f, "join-problems", "LOCAL_TO_OSS", shared, false, current)
	runtime := f.master.tasks
	runtime.leases.ttl = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logs := captureLog(t)
	label := f.master.nodeLabel
	if status, body := nodeChange(t, f, p.ID, "", joinBody(joiner, shared, map[string]any{"requestId": "join-b"})); status != 200 {
		t.Fatal(status, body)
	}
	p, err := runtime.repo.Get(p.ID)
	if err != nil || !p.NodeJoining(joiner.ID) {
		t.Fatal("the join was not recorded", err)
	}
	if problem := joinProblemView(t, f, p.ID, "zh-CN", joiner); problem != nil {
		t.Fatal("a join that no step has tried has a problem", problem)
	}
	step := func(available ...task.Node) error {
		t.Helper()
		ready := map[string]bool{}
		for _, node := range available {
			ready[node.ID] = true
		}
		next, err := runtime.reconcileFailover(ctx, p, ready)
		if current, readErr := runtime.repo.Get(p.ID); readErr == nil {
			p = current
		} else {
			p = next
		}
		return err
	}

	// Takeover is set up only with every current node available.
	if err := step(); err != nil {
		t.Fatal(err)
	}
	expectJoinProblem(t, f, p.ID, "zh-CN", joiner, joinTakeoverWaiting, fmt.Sprintf("执行节点 %s 需等此任务启用接管后才能加入。启用接管需等待以下现有节点可用：%s", label(joiner), label(current)))

	// The added node does not see the task's test file: the same reason on
	// every step, logged once.
	mode.Store("unverified")
	for range 3 {
		if err := step(current); err != nil {
			t.Fatal(err)
		}
	}
	if !p.Failover.Enabled || !p.NodeJoining(joiner.ID) {
		t.Fatal("takeover was not set up, or the node joined", p.Failover, p.Membership)
	}
	detail := fmt.Sprintf("Agent %s cannot verify the original shared directory: shared probe marker is missing", label(joiner))
	unverified := fmt.Sprintf("Agent %s has not passed the shared directory check: %s", label(joiner), detail)
	expectJoinProblem(t, f, p.ID, "", joiner, joinStorageUnverified, unverified)
	expectJoinProblem(t, f, p.ID, "en-US", joiner, joinStorageUnverified, unverified)
	expectJoinProblem(t, f, p.ID, "zh-CN", joiner, joinStorageUnverified, fmt.Sprintf("执行节点 %s 未通过共享目录校验：%s", label(joiner), detail))
	if lines := joinLogLines(logs, p, unverified); lines != 1 {
		t.Fatal("the problem was not logged once", lines, logs.matching("has not joined yet"))
	}

	// The lease handshake fails; then the check fails again, already logged.
	mode.Store("lease")
	for range 2 {
		if err := step(current); err != nil {
			t.Fatal(err)
		}
	}
	leaseFailed := fmt.Sprintf("The execution lease of Agent %s for this task could not be set up: lease store is unavailable", label(joiner))
	expectJoinProblem(t, f, p.ID, "en-US", joiner, joinLeaseFailed, leaseFailed)
	expectJoinProblem(t, f, p.ID, "zh-CN", joiner, joinLeaseFailed, fmt.Sprintf("无法为执行节点 %s 建立此任务的执行租约：lease store is unavailable", label(joiner)))
	mode.Store("unverified")
	if err := step(current); err != nil {
		t.Fatal(err)
	}
	expectJoinProblem(t, f, p.ID, "en-US", joiner, joinStorageUnverified, unverified)
	if joinLogLines(logs, p, unverified) != 1 || joinLogLines(logs, p, leaseFailed) != 1 {
		t.Fatal("a reason was logged more than once in one join", logs.matching("has not joined yet"))
	}

	// A problem of the task ends the step before any node can join.
	mode.Store("")
	proof := storedProof(t, f, p.ID)
	if err := f.db.Update(func(tx *state.Tx) error { return tx.Delete("master-task-storage-proofs", fmt.Sprint(p.ID)) }); err != nil {
		t.Fatal(err)
	}
	if err := step(current); err == nil {
		t.Fatal("a missing takeover proof did not stop the step")
	}
	expectJoinProblem(t, f, p.ID, "zh-CN", joiner, joinTaskProblem, fmt.Sprintf("执行节点 %s 需等此任务的当前问题解决后才能加入：the original shared directory has not been verified", label(joiner)))
	if err := f.db.Update(func(tx *state.Tx) error { return tx.Put("master-task-storage-proofs", fmt.Sprint(p.ID), proof) }); err != nil {
		t.Fatal(err)
	}

	// The node joins: its problem goes, and the log says it joined.
	if err := step(current); err != nil {
		t.Fatal(err)
	}
	if p.NodeJoining(joiner.ID) || p.NodeRetired(joiner.ID) {
		t.Fatal("the node did not join", p.Membership, p.Failover)
	}
	if problem := joinProblemView(t, f, p.ID, "zh-CN", joiner); problem != nil || runtime.joins.tasks[p.ID] != nil {
		t.Fatal("a joined node kept its problem", problem, runtime.joins.tasks)
	}
	if lines := logs.matching(fmt.Sprintf("task %d generation %d: node %s has joined", p.ID, p.Generation, label(joiner))); len(lines) != 1 {
		t.Fatal("the join was not logged once", lines)
	}

	// Cancelling a join that keeps failing clears its problem at once; added
	// again, the node starts a new join, whose reasons are logged again.
	second := overrideTestAgent(t, f, 3, func(command agenttransport.Command) (agenttransport.Response, bool) {
		if command.Operation == "shared_probe_verify" {
			return agentReply(500, map[string]string{"code": "operation_failed", "message": "shared probe marker is missing"})
		}
		return agenttransport.Response{}, false
	})
	labelAgent(t, f, second, "Joiner C")
	secondUnverified := fmt.Sprintf("Agent %s has not passed the shared directory check: Agent %s cannot verify the original shared directory: shared probe marker is missing", label(second), label(second))
	for attempt, request := range []string{"join-c", "join-c-again"} {
		if status, body := nodeChange(t, f, p.ID, "", joinBody(second, shared, map[string]any{"requestId": request})); status != 200 {
			t.Fatal(status, body)
		}
		if problem := joinProblemView(t, f, p.ID, "en-US", second); problem != nil {
			t.Fatal("a new join shows a problem before any step tried it", attempt, problem)
		}
		if err := step(current, joiner); err != nil {
			t.Fatal(err)
		}
		expectJoinProblem(t, f, p.ID, "en-US", second, joinStorageUnverified, secondUnverified)
		if lines := joinLogLines(logs, p, secondUnverified); lines != attempt+1 {
			t.Fatal("each join did not log its problem once", attempt, lines, logs.matching("has not joined yet"))
		}
		if attempt == 0 {
			if status, body := nodeChange(t, f, p.ID, "", map[string]any{"remove": []string{second.Address}, "requestId": "cancel-c"}); status != 200 {
				t.Fatal(status, body)
			}
			if runtime.joins.tasks[p.ID][second.ID] != nil {
				t.Fatal("a cancelled join kept its problem")
			}
		}
	}
}

// ossJoinTask creates an OSS-to-OSS task on current and adds joining nodes
// to it directly, without the node change's checks.
func ossJoinTask(t *testing.T, f *agentHTTPFixture, key string, current task.Node, joining ...task.Node) task.Parent {
	t.Helper()
	repo := f.master.tasks.repo
	p, _, err := repo.Create(task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: []task.Node{current}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED"}, key)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err = repo.ChangeNodes(p.ID, task.NodeChange{Add: joining, Mode: task.DrainModeDrain, RequestKey: key, Fingerprint: key})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The waits that skip a node, and the refusals that no retry fixes, each name
// their reason: an offline Agent, one enrolled again, an upgrade window, an
// Agent too old to join, one that does not report its version, one that
// cannot record a skipped shared check, a node change still moving work, a
// scan moving or held by the joining node, takeover that cannot be set up,
// and revision numbers used up. A task that is not running says so, and
// forgets what its steps recorded.
func TestJoinProblemsNameWaitsAndRefusals(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	sharedAgents := sharedTestAgents(t, f, &calls, "writable", "legacy")
	silent := overrideTestAgent(t, f, 3, func(command agenttransport.Command) (agenttransport.Response, bool) {
		if command.Operation == "capabilities" {
			return agentReply(500, map[string]string{"code": "operation_failed", "message": "capabilities are unavailable"})
		}
		return agenttransport.Response{}, false
	})
	a, stale, paused, ready := takeoverTestAgent(t, f, "waits-a"), takeoverTestAgent(t, f, "waits-stale"), takeoverTestAgent(t, f, "waits-paused"), takeoverTestAgent(t, f, "waits-ready")
	old := oldTestAgent(t, f, "waits-old")
	waitOnline(t, f, a.node, stale.node, paused.node, ready.node, old)
	for node, name := range map[task.Node]string{sharedAgents[0].node: "Writable", sharedAgents[1].node: "Legacy", silent: "Silent", a.node: "Member A", stale.node: "Enrolled again", paused.node: "Upgrading", ready.node: "Ready", old: "Old"} {
		labelAgent(t, f, node, name)
	}
	ghost := task.Node{ID: "ghost-node", EnrollmentID: "ghost-enrollment", Address: "ghost.example"}
	moved := stale.node
	moved.EnrollmentID = "earlier-enrollment"
	runtime := f.master.tasks
	runtime.leases.ttl = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logs := captureLog(t)
	label := f.master.nodeLabel
	if err := f.db.Update(func(tx *state.Tx) error {
		return tx.Put("agent-maintenance", paused.node.ID, agentMaintenance{ID: "upgrade", EnrollmentID: paused.node.EnrollmentID, ExpiresAt: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)})
	}); err != nil {
		t.Fatal(err)
	}

	p := ossJoinTask(t, f, "waits", a.node, ghost, moved, paused.node, old, silent)
	p, err := runtime.repo.EnableFailover(p.ID, p.Generation, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]task.Node{
		joinAgentOffline:       ghost,
		joinEnrollmentChanged:  moved,
		joinMaintenance:        paused.node,
		joinAgentTooOld:        old,
		joinVersionUnavailable: silent,
	}
	for range 3 {
		if p, err = runtime.rejoinAvailableNodes(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	for code, node := range want {
		problem := joinProblemView(t, f, p.ID, "en-US", node)
		if problem["code"] != code {
			t.Fatalf("%s: %v", node.Address, problem)
		}
		if lines := logs.matching(fmt.Sprintf("task %d generation %d: a joining node has not joined yet: ", p.ID, p.Generation), problem["message"].(string)); len(lines) != 1 {
			t.Fatal("a reason was not logged once", code, lines)
		}
	}
	expectJoinProblem(t, f, p.ID, "zh-CN", ghost, joinAgentOffline, fmt.Sprintf("执行节点 %s 离线，恢复连接后继续加入", label(ghost)))
	expectJoinProblem(t, f, p.ID, "zh-CN", moved, joinEnrollmentChanged, fmt.Sprintf("执行节点 %s 添加到此任务后重新注册过，无法以原来的注册身份加入。请取消此次加入，再重新添加该节点", label(moved)))
	expectJoinProblem(t, f, p.ID, "zh-CN", paused.node, joinMaintenance, fmt.Sprintf("执行节点 %s 正处于升级维护期，维护结束后继续加入", label(paused.node)))
	expectJoinProblem(t, f, p.ID, "zh-CN", old, joinAgentTooOld, fmt.Sprintf("执行节点 %s 版本过旧，无法加入运行中的任务，请先升级", label(old)))
	expectJoinProblem(t, f, p.ID, "en-US", silent, joinVersionUnavailable, fmt.Sprintf("Cannot read the version information of Agent %s: capabilities are unavailable", label(silent)))

	// Waits of the whole task, and a joining node that holds the scan.
	with := func(change func(*task.FailoverState)) task.Parent {
		q := p
		failover := *p.Failover
		change(&failover)
		q.Failover = &failover
		return q
	}
	if _, err := runtime.rejoinAvailableNodes(ctx, with(func(failover *task.FailoverState) {
		failover.Migrations = map[string]task.FailoverMigration{"lost": {}}
	})); err != nil {
		t.Fatal(err)
	}
	expectJoinProblem(t, f, p.ID, "zh-CN", ghost, joinTakeoverInProgress, fmt.Sprintf("执行节点 %s 将在失联或移出节点的工作转移到其他节点后加入", label(ghost)))
	expectJoinProblem(t, f, p.ID, "en-US", old, joinTakeoverInProgress, fmt.Sprintf("Agent %s joins after the work of a lost or removed node has moved to other nodes", label(old)))
	if _, err := runtime.rejoinAvailableNodes(ctx, with(func(failover *task.FailoverState) { failover.ScannerSeeding = true })); err != nil {
		t.Fatal(err)
	}
	expectJoinProblem(t, f, p.ID, "zh-CN", paused.node, joinScanHandoff, fmt.Sprintf("执行节点 %s 将在扫描移交到其他节点后加入", label(paused.node)))
	if _, err := runtime.rejoinAvailableNodes(ctx, with(func(failover *task.FailoverState) { failover.ScannerNodeID = paused.node.ID })); err != nil {
		t.Fatal(err)
	}
	expectJoinProblem(t, f, p.ID, "zh-CN", paused.node, joinScanUnfinished, fmt.Sprintf("执行节点 %s 原为此任务的扫描节点，需等其他节点接管扫描后才能加入", label(paused.node)))
	expectJoinProblem(t, f, p.ID, "en-US", ghost, joinAgentOffline, fmt.Sprintf("Agent %s is offline; it joins after it reconnects", label(ghost)))

	// An Agent that cannot record a skipped shared check never joins a task
	// whose check was skipped.
	writable, legacy := sharedAgents[0].node, sharedAgents[1].node
	dir := t.TempDir()
	legacy.LocalRoot = dir
	skipped := localTask(t, f, "skipped", "LOCAL_TO_OSS", dir, true, writable)
	if skipped, _, err = runtime.repo.ChangeNodes(skipped.ID, task.NodeChange{Add: []task.Node{legacy}, Mode: task.DrainModeDrain, RequestKey: "join-legacy", Fingerprint: "join-legacy"}); err != nil {
		t.Fatal(err)
	}
	if skipped, err = runtime.reconcileFailover(ctx, skipped, map[string]bool{writable.ID: true}); err != nil {
		t.Fatal(err)
	}
	expectJoinProblem(t, f, skipped.ID, "zh-CN", legacy, joinUpgradeRequired, fmt.Sprintf("请先升级执行节点 %s，才能运行已跳过共享目录校验的任务", label(legacy)))

	// Takeover cannot be set up while a current node does not report that
	// it supports it.
	unready := ossJoinTask(t, f, "unready", silent, ready.node)
	if _, err := runtime.reconcileFailover(ctx, unready, map[string]bool{silent.ID: true}); err != nil {
		t.Fatal(err)
	}
	expectJoinProblem(t, f, unready.ID, "en-US", ready.node, joinTakeoverNotReady, fmt.Sprintf("Agent %s joins after takeover is set up for this task. Setting it up needs every current node to report that it supports takeover", label(ready.node)))

	// The revision numbers a join needs are used up.
	exhausted := ossJoinTask(t, f, "exhausted", a.node, ready.node)
	if exhausted, err = runtime.repo.EnableFailover(exhausted.ID, exhausted.Generation, false); err != nil {
		t.Fatal(err)
	}
	exhausted.BudgetRevision, exhausted.Shares = math.MaxUint64-1, nil
	if err := f.db.Update(func(tx *state.Tx) error { return tx.Put("master-tasks", fmt.Sprintf("%020d", exhausted.ID), exhausted) }); err != nil {
		t.Fatal(err)
	}
	if exhausted, err = runtime.rejoinAvailableNodes(ctx, exhausted); err != nil {
		t.Fatal(err)
	}
	if !exhausted.NodeJoining(ready.node.ID) {
		t.Fatal("the node joined without a revision number")
	}
	expectJoinProblem(t, f, exhausted.ID, "zh-CN", ready.node, joinExhausted, fmt.Sprintf("执行节点 %s 无法加入：此任务加入节点所需的修订号已用尽。请取消此次加入", label(ready.node)))

	// A task that is not running says so instead of its last reason, and
	// forgets its reasons once it has stopped.
	if p, err = runtime.repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	expectJoinProblem(t, f, p.ID, "zh-CN", ghost, joinTaskNotRunning, fmt.Sprintf("任务当前未运行，执行节点 %s 将在任务重新运行后加入", label(ghost)))
	if err := runtime.repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	if err := runtime.recordStep(ctx, p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	if runtime.joins.tasks[p.ID] != nil {
		t.Fatal("a stopped task kept its join problems")
	}
	expectJoinProblem(t, f, p.ID, "en-US", old, joinTaskNotRunning, fmt.Sprintf("The task is not running; Agent %s joins after the task runs again", label(old)))
	runtime.joins.prune(map[int64]bool{})
	if len(runtime.joins.tasks) != 0 {
		t.Fatal("tasks that left the active list kept their join problems", runtime.joins.tasks)
	}
}
