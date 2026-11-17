package main

import (
	"net/http"
	"strings"
	"testing"
)

// The help says what a stop keeps (completed objects) and what it does not
// (objects in transfer start over), and what an incomplete exception list
// means, in every language.
func TestHelpStatesWhatStopKeepsAndWhatPartialMeans(t *testing.T) {
	for _, test := range []struct {
		locale       string
		args         []string
		wants, avoid []string
	}{
		{"zh", []string{"help", "task"}, []string{"已完成的对象会保留", "正在传输的对象在任务继续后从头传输"}, []string{"停止传输并保留进度"}},
		{"en", []string{"help", "task"}, []string{"completed objects are kept", "objects in transfer start over when the task continues"}, []string{"Stop and retain progress"}},
		{"ja", []string{"help", "task"}, []string{"完了したオブジェクトは保持し", "転送中のオブジェクトは再開後に最初から転送し直します"}, []string{"進捗を保持して停止"}},
		{"zh", []string{"help", "exceptions"}, []string{"其未完成子任务的失败记录可能尚未上报"}, []string{"表示节点返回是否完整"}},
		{"en", []string{"help", "exceptions"}, []string{"an offline Agent has unfinished subtasks whose failures may not be"}, []string{"describe node coverage"}},
		{"ja", []string{"help", "exceptions"}, []string{"未完了のサブタスクを持つノードがオフラインで"}, []string{"応答の完全性"}},
	} {
		code, out, _ := runMockCLI(t, append([]string{"--lang", test.locale}, test.args...), func(http.ResponseWriter, *http.Request) { t.Fatal("network during help") })
		if code != 0 {
			t.Fatalf("%s %v: exit %d", test.locale, test.args, code)
		}
		for _, want := range test.wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s %v lacks %q:\n%s", test.locale, test.args, want, out)
			}
		}
		for _, avoid := range test.avoid {
			if strings.Contains(out, avoid) {
				t.Errorf("%s %v still says %q", test.locale, test.args, avoid)
			}
		}
	}
}
