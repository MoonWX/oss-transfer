package master

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func TestAllChildPriorityHTTPUsesGlobalCursorAndBoundedUpgrade(t *testing.T) {
	f := newAgentHTTPFixture(t)
	p := persistAPITask(t, f, "all-priority-child-api")
	base, err := f.master.tasks.repo.Assign(p.ID, p.Generation, 1, []transferstore.Object{{Key: "object", Size: 1}})
	if err != nil {
		t.Fatal(err)
	}
	p, err = f.master.tasks.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.master.store.Update(func(tx *state.Tx) error {
		p.ChildCount, p.DispatchedThrough = 2100, 2100
		for n := 1; n <= 2100; n++ {
			child := base
			child.Job.Number, child.Job.ChildID = n, fmt.Sprintf("%d-%d", p.ID, n)
			status := execution.Completed
			switch n {
			case 2097:
				status = execution.Running
			case 2098:
				status = execution.Failed
			case 2099:
				status = execution.PartialFailed
			case 2100:
				status = execution.Recoverable
			}
			child.Record = &execution.Record{Job: child.Job, Status: status, Attempt: 1, UpdatedAt: time.Now().UTC()}
			if err := tx.Put("master-task-children", fmt.Sprintf("%020d/%010d", p.ID, n), child); err != nil {
				return err
			}
		}
		return tx.Put("master-tasks", fmt.Sprintf("%020d", p.ID), p)
	}); err != nil {
		t.Fatal(err)
	}
	get := func(query string) map[string]any {
		t.Helper()
		code, response := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/children?view=all&order=priority&%s", f.http.URL, p.ID, query), f.admin, nil)
		if code != 200 {
			t.Fatal(code, response)
		}
		return response["data"].(map[string]any)
	}
	first := get("limit=2")
	if first["indexing"] != true || len(first["items"].([]any)) != 0 || first["hasMore"] != false || first["nextCursor"] != "" {
		t.Fatal("upgrade did not return bounded indexing state", first)
	}
	for attempt := 0; attempt < 10 && first["indexing"] == true; attempt++ {
		first = get("limit=2")
	}
	items := first["items"].([]any)
	if first["indexing"] == true || first["order"] != "priority" || len(items) != 2 || items[0].(map[string]any)["subtaskNumber"] != float64(2097) || items[1].(map[string]any)["subtaskNumber"] != float64(2098) || first["nextCursor"] != "01/0000002098" {
		t.Fatal(first)
	}
	next := get("limit=2&offset=2&cursor=" + url.QueryEscape(first["nextCursor"].(string)) + "&known=2098")
	items = next["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["subtaskNumber"] != float64(2099) || items[1].(map[string]any)["subtaskNumber"] != float64(2100) || next["nextCursor"] != "03/0000002100" || next["nextOffset"] != float64(4) {
		t.Fatal(next)
	}
	if updates := next["updates"].([]any); len(updates) != 1 || updates[0].(map[string]any)["subtaskNumber"] != float64(2098) {
		t.Fatal(updates)
	}
	last := get("limit=2&offset=4&cursor=" + url.QueryEscape(next["nextCursor"].(string)))
	items = last["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["subtaskNumber"] != float64(1) || items[1].(map[string]any)["subtaskNumber"] != float64(2) || last["nextCursor"] != "09/0000000002" {
		t.Fatal("completed children appeared before ongoing work", last)
	}
	// Direct child links remain identity lookups, independent of list order.
	code, response := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/children/1?order=priority", f.http.URL, p.ID), f.admin, nil)
	if code != 200 || response["data"].(map[string]any)["items"].([]any)[0].(map[string]any)["subtaskNumber"] != float64(1) {
		t.Fatal(code, response)
	}
	for _, bad := range []string{"cursor=0%2F0000000001", "cursor=11%2F0000000001", "known=1,1"} {
		code, _ := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/children?view=all&order=priority&%s", f.http.URL, p.ID, bad), f.admin, nil)
		if code != 400 {
			t.Fatal(bad, code)
		}
	}
}
