package main

import (
	"bytes"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

const scanNodesReply = `{"success":true,"data":{"nodes":[{"address":"10.0.0.2","label":"edge","agent":true,"online":true}]}}`

// node update and agent update send --scan-concurrency as maxConcurrentScans
// and confirm it in the summary, in every language.
func TestNodeAndAgentUpdateSetScanConcurrency(t *testing.T) {
	for _, command := range []string{"node", "agent"} {
		for _, test := range []struct{ lang, want string }{
			{"zh", "节点 10.0.0.2 已更新：扫描并发：4"},
			{"en", "Scan concurrency: 4"},
			{"ja", "スキャン並列数：4"},
		} {
			var sent map[string]interface{}
			code, out, stderr := runMockCLI(t, []string{"--lang", test.lang, command, "update", "10.0.0.2", "--scan-concurrency", "4"}, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					if r.URL.Path != "/api/master/nodes/10.0.0.2" {
						t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
					}
					sent = adminTestBody(t, r)
				}
				io.WriteString(w, scanNodesReply)
			})
			if code != 0 || !strings.Contains(out, test.want) {
				t.Fatalf("%s update in %s: exit %d: %s %s", command, test.lang, code, out, stderr)
			}
			if len(sent) != 1 || sent["maxConcurrentScans"] != float64(4) {
				t.Fatalf("%s update sent %v", command, sent)
			}
		}
	}
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body := adminTestBody(t, r)
		if len(body) != 3 || body["maxConcurrentScans"] != float64(16) || body["maxConcurrentChildren"] != float64(8) || body["label"] != "edge" {
			t.Fatal(body)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true})
	})
	if _, err := runNodeAdmin(client, "update", []string{"storage-node", "--label", "edge", "--child-concurrency=8", "--scan-concurrency=16"}); err != nil {
		t.Fatal(err)
	}
}

func TestScanConcurrencyCLIRejectsInvalidValuesBeforeRequest(t *testing.T) {
	client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("an invalid value was sent to the server") })
	for _, args := range [][]string{
		{"--scan-concurrency", "0"}, {"--scan-concurrency", "17"}, {"--scan-concurrency", "2.5"}, {"--scan-concurrency", "-1"},
		{"--scan-concurrency", "2", "--scan-concurrency", "3"},
	} {
		_, err := agentUpdate(client, append([]string{"storage-node"}, args...))
		if err == nil || !strings.Contains(err.Error(), "--scan-concurrency 必须是 1–16 的整数") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

// A Master released before scan concurrency rejects the unknown field with
// its malformed-request answer; the CLI says that the Master needs an update.
// Any other refusal is shown as the Master gave it.
func TestScanConcurrencyOnAnOlderMaster(t *testing.T) {
	older := func(message string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			adminTestJSON(t, w, map[string]interface{}{"success": false, "message": message})
		}
	}
	for _, test := range []struct{ lang, message, want string }{
		{"zh", "JSON 请求格式无效", "Master 版本较旧，不支持设置扫描并发；请先升级 Master。"},
		{"en", "Invalid JSON request", "The Master runs an older version that cannot set scan concurrency; update the Master first."},
		{"ja", "JSON 要求の形式が無効です", "Master のバージョンが古いため、スキャン並列数を設定できません。先に Master を更新してください。"},
	} {
		code, out, stderr := runMockCLI(t, []string{"--lang", test.lang, "node", "update", "10.0.0.2", "--scan-concurrency", "4"}, older(test.message))
		if code == 0 || !strings.Contains(stderr+out, test.want) {
			t.Fatalf("%s: exit %d: %s %s", test.lang, code, out, stderr)
		}
	}
	// Without the field, the same answer is not about scan concurrency.
	code, out, stderr := runMockCLI(t, []string{"node", "update", "10.0.0.2", "--child-concurrency", "8"}, older("JSON 请求格式无效"))
	if code == 0 || strings.Contains(stderr+out, "扫描并发") || !strings.Contains(stderr+out, "JSON 请求格式无效") {
		t.Fatalf("exit %d: %s %s", code, out, stderr)
	}
	// A current Master's own refusals stay as they are.
	for _, message := range []string{"节点设置无效，请检查并发范围和总数限制", "此节点的 Agent 版本较旧，不支持设置扫描并发，请先升级"} {
		code, out, stderr = runMockCLI(t, []string{"node", "update", "10.0.0.2", "--scan-concurrency", "4"}, older(message))
		if code == 0 || strings.Contains(stderr+out, "Master 版本较旧") || !strings.Contains(stderr+out, message) {
			t.Fatalf("exit %d: %s %s", code, out, stderr)
		}
	}
}

// node list shows each node's running scans over its scan concurrency.
func TestNodeListShowsScans(t *testing.T) {
	result := map[string]interface{}{
		"data": map[string]interface{}{"nodes": []interface{}{
			map[string]interface{}{"address": "10.0.0.2", "agent": true, "online": true},
			map[string]interface{}{"address": "10.0.0.3", "agent": true, "online": true},
			map[string]interface{}{"address": "10.0.0.4", "agent": true, "online": false},
		}},
		"workers": map[string]interface{}{
			"10.0.0.2": map[string]interface{}{"success": true, "executionStatus": map[string]interface{}{"activeScans": float64(3), "maxConcurrentScans": float64(1)}},
			"10.0.0.3": map[string]interface{}{"success": true, "executionStatus": map[string]interface{}{"activeScans": float64(0), "maxConcurrentScans": float64(8)}},
			"10.0.0.4": map[string]interface{}{"success": false, "message": "Agent is offline"},
		},
	}
	for _, test := range []struct {
		lang  string
		wants []string
	}{
		{"zh", []string{"节点 节点标识 角色 连接状态 扫描", "10.0.0.2 10.0.0.2 Agent 在线 3/1", "10.0.0.3 10.0.0.3 Agent 在线 0/8", "10.0.0.4 10.0.0.4 Agent 离线 —"}},
		{"en", []string{"Node Node ID Role Connection Scans", "10.0.0.2 10.0.0.2 Agent Online 3/1"}},
		{"ja", []string{"ノード ノード ID 役割 接続状態 スキャン", "10.0.0.3 10.0.0.3 Agent オンライン 0/8"}},
	} {
		var output bytes.Buffer
		renderCLIResult(&cliLocaleWriter{Writer: &output, locale: cliLocale(test.lang)}, result)
		normalized := strings.Join(strings.Fields(output.String()), " ")
		for _, want := range test.wants {
			if !strings.Contains(normalized, want) {
				t.Errorf("%s: missing %q in %s", test.lang, want, output.String())
			}
		}
	}
}

func TestCLICompletionOffersScanConcurrency(t *testing.T) {
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
	for _, words := range [][]string{
		{"oss-transfer-cli", "node", "update", "10.0.0.2", "--"},
		{"oss-transfer-cli", "agent", "update", "10.0.0.2", "--"},
		{"oss-transfer-cli", "node", "update", "10.0.0.2", "--scan-concurrency", "4", "--"},
	} {
		args := append([]string{"-c", script, "completion-test", "../deploy/oss-transfer-cli.completion"}, words...)
		output, err := exec.Command(bash, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("completion %v failed: %v %s", words, err, output)
		}
		if !strings.Contains("\n"+string(output), "\n--scan-concurrency\n") {
			t.Errorf("completion %v lacks --scan-concurrency: %s", words, output)
		}
	}
	// The value of --scan-concurrency is not completed as a command.
	args := append([]string{"-c", script, "completion-test", "../deploy/oss-transfer-cli.completion"}, "oss-transfer-cli", "node", "update", "10.0.0.2", "--scan-concurrency", "")
	output, err := exec.Command(bash, args...).CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "" {
		t.Fatalf("completion offered a value for --scan-concurrency: %v %q", err, output)
	}
}
