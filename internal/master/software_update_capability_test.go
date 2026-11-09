package master

import (
	"errors"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
)

// Why an online update is unavailable, and what to do instead, is shown in the
// caller's language rather than as an English capability message.
func TestSoftwareUpdateCapabilityIsLocalized(t *testing.T) {
	f := newAgentHTTPFixture(t)
	u := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "master", Version: "4.1.40", Reason: "CONTAINER", Detail: softwareupdate.ContainerUpdateMessage}, release: softwareupdate.Release{Version: "4.1.41"}}
	f.master.options.Updater = u
	agent := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.40", Reason: "UNMANAGED", Detail: "Online updates require a systemd-managed service"}}
	nodeTestAgentWithUpdater(t, f, "capability-node", agent)
	call := func(method, path string, body any) (int, map[string]any) {
		status, _, result, err := localizedRequest(f.http.Client(), method, f.http.URL+path, f.admin, "zh-CN", body)
		if err != nil {
			t.Fatal(err)
		}
		return status, result
	}
	status, body := call("GET", "/api/master/software-updates", nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	data := body["data"].(map[string]any)
	master := data["master"].(map[string]any)["info"].(map[string]any)
	if master["detail"] != "此节点运行在容器中，请拉取新镜像并重建容器来更新（例如先运行 docker compose pull，再运行 docker compose up -d）" || master["reason"] != "CONTAINER" {
		t.Fatal("container detail was not localized", master)
	}
	nodes := data["agents"].([]any)
	if detail := nodes[0].(map[string]any)["info"].(map[string]any)["detail"]; detail != "在线更新需要由 systemd 管理的服务；请使用安装脚本升级（install.sh --upgrade）" {
		t.Fatal("Agent detail was not localized", detail)
	}
	if status, _ := call("POST", "/api/master/software-updates/check", map[string]any{}); status != 200 {
		t.Fatal(status)
	}
	u.mu.Lock()
	u.err = errors.New(softwareupdate.ContainerUpdateMessage)
	u.mu.Unlock()
	status, body = call("POST", "/api/master/software-updates/master", map[string]string{"version": "4.1.41"})
	if status != 409 || body["message"] != "此节点运行在容器中，请拉取新镜像并重建容器来更新（例如先运行 docker compose pull，再运行 docker compose up -d）" {
		t.Fatal("update refusal was not localized", status, body)
	}
}

// A v4.1 container Agent reports its own English sentence for the CONTAINER
// reason. The console shows the current localized one instead.
func TestOlderContainerAgentDetailIsLocalized(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := &fakeSoftwareUpdater{info: softwareupdate.Info{Role: "agent", Version: "4.1.41", Reason: "CONTAINER", Detail: "Container installations must be updated with Docker Compose"}}
	nodeTestAgentWithUpdater(t, f, "container-node", agent)
	for locale, want := range map[string]string{"zh-CN": "此节点运行在容器中，请拉取新镜像并重建容器来更新（例如先运行 docker compose pull，再运行 docker compose up -d）", "en-US": softwareupdate.ContainerUpdateMessage} {
		status, _, body, err := localizedRequest(f.http.Client(), "GET", f.http.URL+"/api/master/software-updates", f.admin, locale, nil)
		if err != nil || status != 200 {
			t.Fatal(status, body, err)
		}
		nodes := body["data"].(map[string]any)["agents"].([]any)
		if detail := nodes[0].(map[string]any)["info"].(map[string]any)["detail"]; detail != want {
			t.Fatalf("%s: container detail %q", locale, detail)
		}
	}
}
