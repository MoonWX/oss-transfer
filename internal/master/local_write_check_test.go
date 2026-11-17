package master

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
)

// noWriteProbeAgent is an Agent from before the write probe.
type noWriteProbeAgent struct{ *agentservice.Service }

func (a noWriteProbeAgent) Execute(ctx context.Context, command agenttransport.Command) (agenttransport.Response, error) {
	if command.Operation == "local_write_probe" {
		return agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}, nil
	}
	return a.Service.Execute(ctx, command)
}

func TestDownloadPrecheckProvesTheAgentCanWrite(t *testing.T) {
	f := newAgentHTTPFixture(t)
	current := connectTestAgent(t, f, 1, func(s *agentservice.Service) agenttransport.Executor { return s })
	older := connectTestAgent(t, f, 2, func(s *agentservice.Service) agenttransport.Executor { return noWriteProbeAgent{s} })
	validate := func(node, path string) map[string]any {
		t.Helper()
		row := taskCreateRequest{Direction: "OSS_TO_LOCAL",
			SourceEndpoint:      &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "source"},
			DestinationEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{node, path}}}}
		code, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []taskCreateRequest{row})
		if code != 200 {
			t.Fatal(code, response)
		}
		bindings := response["data"].(map[string]any)["nodeBindings"].([]any)
		return bindings[0].(map[string]any)
	}
	root := t.TempDir()
	for _, path := range []string{root, filepath.Join(root, "new", "download")} {
		check := validate(current.Address, path)
		if check["valid"] != true || check["writeProbeDirectory"] != root || check["freeBytes"] == nil || !strings.Contains(check["message"].(string), "可用空间") {
			t.Fatalf("writable destination %s: %+v", path, check)
		}
		if runtime.GOOS != "windows" && check["effectiveUid"] != float64(os.Geteuid()) {
			t.Fatalf("effective UID missing: %+v", check)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("precheck left files or created directories: %v %v", entries, err)
	}
	// An Agent without the probe still passes, but the result says that
	// nothing proved the directory writable, instead of a plain pass.
	for path, prefix := range map[string]string{root: "", filepath.Join(root, "new"): "目标目录将在任务开始时创建；"} {
		check := validate(older.Address, path)
		if check["valid"] != true || check["freeBytes"] != nil || check["code"] != "LOCAL_PATH_WRITE_NOT_CHECKED" || check["message"] != prefix+"执行节点版本较旧，未检查目录是否可写；升级该节点后重新预检即可检查" {
			t.Fatalf("an Agent without the probe must pass without claiming the directory is writable: %+v", check)
		}
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	for _, path := range []string{locked, filepath.Join(locked, "download")} {
		check := validate(current.Address, path)
		message, _ := check["message"].(string)
		if check["valid"] != false || check["code"] != "LOCAL_PATH_NOT_WRITABLE" || !strings.Contains(message, "UID "+strconv.Itoa(os.Geteuid())) || !strings.Contains(message, locked) {
			t.Fatalf("unwritable destination %s: %+v", path, check)
		}
	}
}

// A root Agent refused by the file system is not told to chown the directory
// to root, which never helps: file permissions do not refuse root, and under
// NFS root_squash the client cannot even run chown. Other users get a command
// they can paste, path included.
func TestWriteProbeMessageGivesAdviceThatCanWork(t *testing.T) {
	denied := func(uid *int, directory string) agentservice.WriteProbe {
		return agentservice.WriteProbe{Path: directory, ProbeDirectory: directory, ErrorCode: agentservice.WriteProbePermissionDenied, EffectiveUID: uid}
	}
	root, user := 0, 1001
	for _, locale := range []string{"", "zh-CN", "en-US", "ja-JP"} {
		ctx := context.WithValue(context.Background(), responseLocaleKey{}, locale)
		message := writeProbeMessage(ctx, denied(&root, "/mnt/nas/dst"))
		if strings.Contains(message, "chown") || !strings.Contains(message, "/mnt/nas/dst") || !strings.Contains(message, "root_squash") || !strings.Contains(message, "allow_other") || !strings.Contains(message, "65534") {
			t.Errorf("%q: root Agent: %s", locale, message)
		}
		message = writeProbeMessage(ctx, denied(&user, "/mnt/nas/dst"))
		if !strings.Contains(message, "chown 1001 /mnt/nas/dst") || strings.Contains(message, "root_squash") {
			t.Errorf("%q: UID 1001: %s", locale, message)
		}
		message = writeProbeMessage(ctx, denied(&user, "/mnt/nas/it's mine"))
		if !strings.Contains(message, `chown 1001 '/mnt/nas/it'"'"'s mine'`) {
			t.Errorf("%q: path needing quotes: %s", locale, message)
		}
		message = writeProbeMessage(ctx, denied(nil, `D:\downloads`))
		if strings.Contains(message, "chown") || strings.Contains(message, "root_squash") || !strings.Contains(message, `D:\downloads`) {
			t.Errorf("%q: Windows Agent: %s", locale, message)
		}
	}
	want := map[string]string{
		"zh-CN": "节点无权写入 /mnt/nas/dst：Agent 以 root 运行仍被拒绝",
		"en-US": "The node cannot write to /mnt/nas/dst: the Agent runs as root and was still refused",
		"ja-JP": "ノードは /mnt/nas/dst に書き込めません。Agent は root で動作していますが拒否されました",
	}
	for locale, prefix := range want {
		if message := writeProbeMessage(context.WithValue(context.Background(), responseLocaleKey{}, locale), denied(&root, "/mnt/nas/dst")); !strings.HasPrefix(message, prefix) {
			t.Errorf("%s: %s", locale, message)
		}
	}
}
