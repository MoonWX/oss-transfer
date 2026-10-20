package master

import (
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestNetworkSettingsPersistAndRouteNewEnrollment(t *testing.T) {
	f := newAgentHTTPFixture(t)
	call := func(method, path string, value any) (int, map[string]any) {
		return request(t, f.http.Client(), method, f.http.URL+path, f.admin, value)
	}
	status, body := call("GET", "/api/master/network", nil)
	data := body["data"].(map[string]any)
	if status != 200 || data["agentUrl"] != "" || data["effectiveAgentUrl"] != data["publicUrl"] {
		t.Fatal("older configuration must fall back to public URL", status, body)
	}
	status, _ = request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/network", "", map[string]string{"agentUrl": "http://10.0.0.2:1685"})
	if status != 401 {
		t.Fatal("network configuration must require admin authentication", status)
	}
	for _, bad := range []string{"file:///tmp/master", "https://user:secret@master.test", "https://master.test/route", "https://master.test:0", "https://master.test:99999"} {
		status, _ = call("PUT", "/api/master/network", map[string]string{"agentUrl": bad})
		if status != 400 {
			t.Fatal("invalid Agent origin accepted", bad, status)
		}
	}
	status, body = call("PUT", "/api/master/network", map[string]string{"agentUrl": "http://10.0.0.43:1685/"})
	if status != 200 || body["data"].(map[string]any)["publicUrl"] != "https://master.example:1685" {
		t.Fatal("Agent URL must not replace browser URL", status, body)
	}
	f.close()
	f.open(t)
	f.master.options.AgentArtifactBase = officialAgentArtifactBase
	f.master.options.AgentArtifactVersion = "v4.1.10"
	f.master.options.AgentDockerImage = "moonwx/osstransfer:4.1.10"
	status, body = call("POST", "/api/master/agents/invitations", map[string]string{"label": "worker-a"})
	if status != 200 {
		t.Fatal(status, body)
	}
	data = body["data"].(map[string]any)
	for _, key := range []string{"joinUrl", "traditionalCommand", "windowsCommand", "dockerCommand", "nativeCommand"} {
		if !strings.Contains(data[key].(string), "http://10.0.0.43:1685") || strings.Contains(data[key].(string), "master.example") {
			t.Fatal("new enrollment command used browser origin", key, data[key])
		}
	}
	response, err := http.Get(f.http.URL + "/api/agents/bootstrap/" + data["inviteToken"].(string))
	if err != nil {
		t.Fatal(err)
	}
	script, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(script), "--master-url 'http://10.0.0.43:1685'") {
		t.Fatal("bootstrap embedded the wrong Master address", response.StatusCode)
	}
	status, body = call("PUT", "/api/master/network", map[string]string{"agentUrl": "", "publicUrl": "https://new-master.example"})
	if status != 200 || body["data"].(map[string]any)["agentUrl"] != "" || body["data"].(map[string]any)["effectiveAgentUrl"] != "https://new-master.example" {
		t.Fatal("cleared Agent URL must follow browser URL", status, body)
	}
}

func TestInheritedAgentURLRemainsInheritedAcrossEditsAndRestart(t *testing.T) {
	f := newAgentHTTPFixture(t)
	assertAddresses := func(public, rawAgent, effectiveAgent string) {
		t.Helper()
		status, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/network", f.admin, nil)
		if status != 200 {
			t.Fatal(status, body)
		}
		data := body["data"].(map[string]any)
		if data["publicUrl"] != public || data["agentUrl"] != rawAgent || data["effectiveAgentUrl"] != effectiveAgent {
			t.Fatal("address inheritance was not preserved", data)
		}
		status, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/agents/invitations", f.admin, map[string]string{"label": "inheritance-check"})
		if status != 200 {
			t.Fatal(status, body)
		}
		invitation := body["data"].(map[string]any)
		if invitation["joinUrl"] != effectiveAgent+"/api/agents/join" || !strings.Contains(invitation["nativeCommand"].(string), "--master-url '"+effectiveAgent+"'") {
			t.Fatal("new invitation used the wrong effective Agent origin", invitation["joinUrl"])
		}
	}
	change := func(value map[string]string) {
		t.Helper()
		status, body := request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/network", f.admin, value)
		if status != 200 {
			t.Fatal(status, body)
		}
	}

	assertAddresses("https://master.example:1685", "", "https://master.example:1685")
	change(map[string]string{"publicUrl": "https://second.example"})
	assertAddresses("https://second.example", "", "https://second.example")
	change(map[string]string{"agentUrl": "http://10.0.0.43:1685"})
	change(map[string]string{"publicUrl": "https://third.example"})
	assertAddresses("https://third.example", "http://10.0.0.43:1685", "http://10.0.0.43:1685")
	change(map[string]string{"agentUrl": ""})
	assertAddresses("https://third.example", "", "https://third.example")
	change(map[string]string{"publicUrl": "https://fourth.example"})
	f.close()
	f.open(t)
	assertAddresses("https://fourth.example", "", "https://fourth.example")
}

func TestNodeDetailsUseAuthenticatedReverseChannel(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := nodeTestAgent(t, f, "details-a")
	status, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/nodes/"+agent.address+"/details", f.admin, nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	data := body["data"].(map[string]any)
	if data["complete"] != true || data["hostname"] == "" || data["os"] == "" || data["arch"] == "" || data["connectionAddress"] != "127.0.0.1" {
		t.Fatal("node details were not obtained from the connected Agent", data)
	}
	if _, ok := data["mounts"].([]any); !ok {
		t.Fatal("mounts must be a list", data)
	}
	if _, ok := data["localIps"].([]any); !ok {
		t.Fatal("localIps must be a list", data)
	}
	status, _ = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/nodes/"+agent.address+"/details", "", nil)
	if status != 401 {
		t.Fatal("node metadata exposed without authentication", status)
	}
}

func TestDisplayNamesRemainAccessibleThroughPublicNodeKeys(t *testing.T) {
	f := newAgentHTTPFixture(t)
	for index, name := range []string{"北京 1", "北京/备份", "北京,备份", "北京备份", "1"} {
		t.Run(name, func(t *testing.T) {
			invite, err := f.master.agents.registry.Invite("", name)
			if err != nil {
				t.Fatal(err)
			}
			credential := agentCredential(t)
			identity := "named-node-" + strconv.Itoa(index)
			status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/agents/join", "", map[string]string{
				"token": invite.Token, "instanceId": identity, "credential": credential, "label": "host-a",
			})
			if status != 200 {
				t.Fatal(status, body)
			}
			member := body["data"].(map[string]any)
			address := member["address"].(string)
			if member["label"] != name || address == "" || strings.ContainsAny(address, " /,") || address == "1" {
				t.Fatal("display name or command-safe key is incorrect", member)
			}
			node := &nodeFixture{master: f, path: filepath.Join(t.TempDir(), "agent.db"), address: address, credential: credential}
			node.open(t)
			defer node.close(t)
			status, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/nodes/"+url.PathEscape(address)+"/details", f.admin, nil)
			if status != 200 {
				t.Fatal(status, body)
			}
			details := body["data"].(map[string]any)
			if details["complete"] != true || details["id"] != identity || details["label"] != name || details["hostname"] == "" {
				t.Fatal("the named Agent could not be queried by its public key", details)
			}
		})
	}
}

func TestInvitationsPinDifferentEndpointsAcrossRestart(t *testing.T) {
	f := newAgentHTTPFixture(t)
	f.master.options.AgentArtifactBase = officialAgentArtifactBase
	f.master.options.AgentArtifactVersion = "v4.1.11"
	endpoints := []string{"https://transfer.example", "http://10.0.0.43:1685", "https://[2001:db8::1]:8443"}
	tokens := make([]string, len(endpoints))
	for i, endpoint := range endpoints {
		status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/agents/invitations", f.admin, map[string]string{"label": "endpoint-test", "masterUrl": endpoint + "/"})
		if status != 200 {
			t.Fatal(status, body)
		}
		data := body["data"].(map[string]any)
		tokens[i] = data["inviteToken"].(string)
		if data["joinUrl"] != endpoint+"/api/agents/bootstrap/"+tokens[i] {
			t.Fatal("invitation ignored its endpoint", data)
		}
	}
	status, body := request(t, f.http.Client(), "PUT", f.http.URL+"/api/master/network", f.admin, map[string]string{"agentUrl": "https://changed.example"})
	if status != 200 {
		t.Fatal(status, body)
	}
	f.close()
	f.open(t)
	f.master.options.AgentArtifactBase = officialAgentArtifactBase
	f.master.options.AgentArtifactVersion = "v4.1.11"
	for i, endpoint := range endpoints {
		for _, suffix := range []string{"", "?platform=windows"} {
			response, err := f.http.Client().Get(f.http.URL + "/api/agents/bootstrap/" + tokens[i] + suffix)
			if err != nil {
				t.Fatal(err)
			}
			script, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 || !strings.Contains(string(script), "'"+endpoint+"'") || strings.Contains(string(script), "changed.example") {
				t.Fatal("saved invitation endpoint drifted", response.StatusCode, string(script))
			}
		}
	}
}

func TestInvitationEndpointValidationAndLegacyFallback(t *testing.T) {
	f := newAgentHTTPFixture(t)
	for _, endpoint := range []string{"", "ftp://host", "https://host/path", "https://user:password@host", "https://host?x=1", "https://host#fragment", "https://host:0", "https://host:65536", "https://host\n"} {
		status, _ := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/agents/invitations", f.admin, map[string]string{"masterUrl": endpoint})
		if status != 400 {
			t.Fatal("invalid invitation endpoint accepted", endpoint, status)
		}
	}
	if err := f.db.Update(func(tx *state.Tx) error { return tx.Put("system", "configuration", Configuration{Configured: true}) }); err != nil {
		t.Fatal(err)
	}
	status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/agents/invitations", f.admin, map[string]string{"label": "legacy-client"})
	if status != 200 || body["data"].(map[string]any)["joinUrl"] != f.http.URL+"/api/agents/join" {
		t.Fatal("legacy client did not use request origin", status, body)
	}
	req, _ := http.NewRequest("POST", f.http.URL+"/api/master/agents/invitations", strings.NewReader(`{"label":"proxy-legacy"}`))
	req.Header.Set("Authorization", "Bearer "+f.admin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Host", "untrusted.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	response, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	bytes, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || strings.Contains(string(bytes), "untrusted.example") || !strings.Contains(string(bytes), f.http.URL) {
		t.Fatal("forwarded headers changed enrollment endpoint", response.StatusCode, string(bytes))
	}
}
