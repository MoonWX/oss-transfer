package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCLICompletionSupportsAgentsAndPagedTaskQueries(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash completion requires Bash")
	}
	script := `source "$1"
shift
COMP_WORDS=("$@")
COMP_CWORD=$((${#COMP_WORDS[@]} - 1))
_init_completion() {
  words=("${COMP_WORDS[@]}")
  cword=$COMP_CWORD
  cur=${words[cword]}
  prev=${words[cword-1]}
}
_oss_transfer_cli_completion
printf '%s\n' "${COMPREPLY[@]}"
`
	for _, test := range []struct {
		words   []string
		wants   []string
		rejects []string
	}{
		{[]string{"oss-transfer-cli", "config", ""}, []string{"get", "set"}, nil},
		{[]string{"oss-transfer-cli", "config", "set", ""}, []string{"language"}, nil},
		{[]string{"oss-transfer-cli", "config", "set", "language", ""}, []string{"auto", "zh", "en", "ja"}, nil},
		{[]string{"oss-transfer-cli", "--lang", ""}, []string{"auto", "zh", "en", "ja"}, nil},
		{[]string{"oss-transfer-cli", "import-inventory", "source/bucket", "dest/bucket", "./lists", "--"}, []string{"--node", "--request-id", "--overwrite"}, nil},
		{[]string{"oss-transfer-cli", "ag"}, []string{"agent"}, nil},
		{[]string{"oss-transfer-cli", "agent", ""}, []string{"invite", "list", "show", "update", "remove"}, nil},
		{[]string{"oss-transfer-cli", "agent", "update", "node-one", "--"}, []string{"--local-root", "--clear-local-roots"}, nil},
		{[]string{"oss-transfer-cli", "agent", "invite", "--"}, []string{"--label", "--address", "--master-url"}, nil},
		{[]string{"oss-transfer-cli", "node", ""}, []string{"invite", "details", "network"}, nil},
		{[]string{"oss-transfer-cli", "agent", ""}, []string{"details"}, nil},
		{[]string{"oss-transfer-cli", "node", "network", "--"}, []string{"--public-url", "--agent-url"}, nil},
		{[]string{"oss-transfer-cli", "task", ""}, []string{"restore", "purge"}, nil},
		{[]string{"oss-transfer-cli", "task", "limits", "224", "--"}, []string{"--list-qps", "--head-qps", "--put-qps", "--bandwidth-mbps"}, nil},
		{[]string{"oss-transfer-cli", "task", "create", "oss-to-oss", "source/src", "target/dst", "--"}, []string{"--list-qps", "--head-qps", "--put-qps"}, nil},
		{[]string{"oss-transfer-cli", "task", ""}, []string{"nodes"}, nil},
		{[]string{"oss-transfer-cli", "task", "nodes", "224", "--"}, []string{"--add", "--remove", "--binding", "--now", "--yes"}, nil},
		{[]string{"oss-transfer-cli", "task", "nodes", "224", "--add", "10.0.0.3", "--"}, []string{"--binding", "--yes"}, nil},
		{[]string{"oss-transfer-cli", "task", "children", "224", "--"}, []string{"--offset", "--limit"}, nil},
		{[]string{"oss-transfer-cli", "task", "show", "224", "--"}, []string{"--child", "--node", "--offset", "--limit"}, nil},
		{[]string{"oss-transfer-cli", "task", "trash", "--"}, []string{"--task-id", "--worker", "--direction", "--search"}, nil},
		{[]string{"oss-transfer-cli", "object", ""}, []string{"compare", "list", "status", "export", "resume", "cancel", "delete"}, nil},
		{[]string{"oss-transfer-cli", "object", "status", "job-1", "--"}, []string{"--after", "--limit", "--status"}, nil},
		{[]string{"oss-transfer-cli", "object", "export", "job-1", "--"}, []string{"--output", "--format", "--status"}, nil},
		{[]string{"oss-transfer-cli", "object", "export", "job-1", "--format", ""}, []string{"zip", "jsonl"}, []string{"json", "csv"}},
		{[]string{"oss-transfer-cli", "object", "status", "job-1", "--status", ""}, []string{"DIFFERENT", "ETAG_MISMATCH", "MISSING_TARGET", "EXTRA_TARGET", "CHANGED_DURING_COMPARE", "ERROR", "BOTH_MISSING"}, []string{"RUNNING"}},
		{[]string{"oss-transfer-cli", "object", "compare", "--source-prefix", "a/", "--"}, []string{"--dest-prefix", "--scope", "--mode"}, nil},
		{[]string{"oss-transfer-cli", "task", "import", "tasks", "--format", ""}, []string{"json", "csv"}, []string{"zip"}},
		{[]string{"oss-transfer-cli", "task", "list", "--status", ""}, []string{"RUNNING", "COMPLETED"}, []string{"DIFFERENT"}},
	} {
		args := append([]string{"-c", script, "completion-test", "../deploy/oss-transfer-cli.completion"}, test.words...)
		output, err := exec.Command(bash, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("completion %v failed: %v %s", test.words, err, output)
		}
		for _, want := range test.wants {
			if !strings.Contains("\n"+string(output), "\n"+want+"\n") {
				t.Errorf("completion %v missing %s: %s", test.words, want, output)
			}
		}
		for _, reject := range test.rejects {
			if strings.Contains("\n"+string(output), "\n"+reject+"\n") {
				t.Errorf("completion %v offers %s: %s", test.words, reject, output)
			}
		}
		if len(test.words) == 3 && test.words[1] == "task" && strings.Contains("\n"+string(output), "\ncreation\n") {
			t.Fatal("completion still offers unsupported task creation")
		}
	}
}
