package master

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestPriorityChildPagesAndVisibleIdentityUpdates(t *testing.T) {
	f := newAgentHTTPFixture(t)
	p := persistAPITask(t, f, "priority-child-api")
	var rows []task.Child
	var observations []task.Observation
	for number := 1; number <= 70; number++ {
		child, err := f.master.tasks.repo.Assign(p.ID, p.Generation, number, []transferstore.Object{{Key: fmt.Sprint(number), Size: 1}})
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, child)
		status := execution.Recoverable
		if number > 65 {
			status = execution.Running
		}
		observations = append(observations, task.Observation{Number: number, Record: execution.Record{Job: child.Job, Status: status, Attempt: 1, UpdatedAt: time.Now().UTC()}})
	}
	if err := f.master.tasks.repo.ObserveBatch(p.ID, p.Generation, observations); err != nil {
		t.Fatal(err)
	}
	get := func(query string) map[string]any {
		t.Helper()
		code, response := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/children?view=active&order=priority&%s", f.http.URL, p.ID, query), f.admin, nil)
		if code != 200 {
			t.Fatal(code, response)
		}
		return response["data"].(map[string]any)
	}
	first := get("offset=0&limit=3")
	items := first["items"].([]any)
	if first["nextOffset"] != float64(3) || first["nextCursor"] != "0/0000000068" || len(items) != 3 || items[0].(map[string]any)["subtaskNumber"] != float64(66) {
		t.Fatal(first)
	}
	next := get("offset=3&limit=3&cursor=" + url.QueryEscape(first["nextCursor"].(string)))
	items = next["items"].([]any)
	if next["nextCursor"] != "3/0000000001" || items[0].(map[string]any)["subtaskNumber"] != float64(69) || items[2].(map[string]any)["subtaskNumber"] != float64(1) {
		t.Fatal(next)
	}
	if err := f.master.tasks.repo.Observe(p.ID, p.Generation, 60, execution.Record{Job: rows[59].Job, Status: execution.Running, Attempt: 2, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.repo.Observe(p.ID, p.Generation, 61, execution.Record{Job: rows[60].Job, Status: execution.Completed, Attempt: 2, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	page := get("offset=25&limit=3&cursor=3%2F0000000050&known=60,61")
	updates := page["updates"].([]any)
	if len(updates) != 2 || updates[0].(map[string]any)["status"] != "RUNNING" || updates[1].(map[string]any)["status"] != "COMPLETED" {
		t.Fatal(updates)
	}
	for _, query := range []string{"cursor=../1", "known=-1", "known=1,1", "known=not-a-number"} {
		code, _ := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/children?view=active&order=priority&%s", f.http.URL, p.ID, query), f.admin, nil)
		if code != 400 {
			t.Fatal(query, code)
		}
	}
}
