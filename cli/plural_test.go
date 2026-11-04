package main

import (
	"testing"
)

func TestEnglishCountsUseSingularForOne(t *testing.T) {
	cases := []struct {
		key  string
		args []interface{}
		want string
	}{
		{"显示 %d 个主任务\n", []interface{}{1}, "Showing 1 parent task\n"},
		{"显示 %d 个主任务\n", []interface{}{2}, "Showing 2 parent tasks\n"},
		{"本页 %d 个子任务\n", []interface{}{1}, "1 subtask on this page\n"},
		{"共 %d 个存储连接\n", []interface{}{1}, "1 storage connection\n"},
		{"共 %d 个执行节点\n", []interface{}{0}, "0 execution nodes\n"},
	}
	for _, c := range cases {
		if got := localeEN.format(c.key, c.args...); got != c.want {
			t.Errorf("format(%q, %v) = %q, want %q", c.key, c.args, got, c.want)
		}
	}
	if got := localeZH.format("显示 %d 个主任务\n", 1); got != "显示 1 个主任务\n" {
		t.Errorf("Chinese changed: %q", got)
	}
	err := cliErrorf("需要 %d 个位置参数，实际收到 %d 个", 1, 0)
	if got := localizedCLIError(localeEN, err); got != "Expected 1 positional argument, received 0" {
		t.Errorf("error = %q", got)
	}
	if got := localizedCLIError(localeEN, cliErrorf("需要 %d 个位置参数，实际收到 %d 个", 2, 1)); got != "Expected 2 positional arguments, received 1" {
		t.Errorf("plural error = %q", got)
	}
}
