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
		words []string
		wants []string
	}{
		{[]string{"oss-transfer-cli", "config", ""}, []string{"get", "set"}},
		{[]string{"oss-transfer-cli", "config", "set", ""}, []string{"language"}},
		{[]string{"oss-transfer-cli", "config", "set", "language", ""}, []string{"auto", "zh", "en", "ja"}},
		{[]string{"oss-transfer-cli", "--lang", ""}, []string{"auto", "zh", "en", "ja"}},
		{[]string{"oss-transfer-cli", "import-inventory", "source/bucket", "dest/bucket", "./lists", "--"}, []string{"--node", "--request-id", "--overwrite"}},
		{[]string{"oss-transfer-cli", "ag"}, []string{"agent"}},
		{[]string{"oss-transfer-cli", "agent", ""}, []string{"invite", "list", "show", "update", "remove"}},
		{[]string{"oss-transfer-cli", "setup", ""}, []string{"status", "complete"}},
		{[]string{"oss-transfer-cli", "setup", "complete", "--"}, []string{"--setup-token-env", "--password-stdin", "--public-url"}},
		{[]string{"oss-transfer-cli", "agent", "update", "node-one", "--"}, []string{"--local-root", "--clear-local-roots"}},
		{[]string{"oss-transfer-cli", "agent", "invite", "--"}, []string{"--label", "--address"}},
		{[]string{"oss-transfer-cli", "node", ""}, []string{"invite"}},
		{[]string{"oss-transfer-cli", "task", ""}, []string{"restore", "purge"}},
		{[]string{"oss-transfer-cli", "task", "children", "224", "--"}, []string{"--offset", "--limit"}},
		{[]string{"oss-transfer-cli", "task", "show", "224", "--"}, []string{"--child", "--node", "--offset", "--limit"}},
		{[]string{"oss-transfer-cli", "task", "trash", "--"}, []string{"--task-id", "--worker", "--direction", "--search"}},
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
		if len(test.words) == 3 && test.words[1] == "task" && strings.Contains("\n"+string(output), "\ncreation\n") {
			t.Fatal("completion still offers unsupported task creation")
		}
	}
}
