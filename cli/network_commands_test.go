package main

import (
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyNetworkPatchPreservesOmittedFields(t *testing.T) {
	for _, tc := range []struct {
		args []string
		body map[string]any
	}{
		{nil, nil},
		{[]string{"--public-url", "https://transfer.example/"}, map[string]any{"publicUrl": "https://transfer.example"}},
		{[]string{"--agent-url", "http://10.0.0.1:1685/"}, map[string]any{"agentUrl": "http://10.0.0.1:1685"}},
		{[]string{"--agent-url", ""}, map[string]any{"agentUrl": ""}},
	} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/master/network" {
				t.Fatal(r.URL)
			}
			if tc.body == nil {
				if r.Method != "GET" {
					t.Fatal(r.Method)
				}
			} else if r.Method != "PUT" || !reflect.DeepEqual(adminTestBody(t, r), tc.body) {
				t.Fatal("network update changed omitted fields")
			}
			adminTestJSON(t, w, map[string]any{"success": true, "data": map[string]any{"publicUrl": "https://transfer.example", "agentUrl": "http://10.0.0.1:1685", "effectiveAgentUrl": "http://10.0.0.1:1685"}})
		})
		if _, err := runAdminCommand(client, "node", append([]string{"network"}, tc.args...)); err != nil {
			t.Fatal(err)
		}
		if watchable(append([]string{"node", "network"}, tc.args...)) != (tc.body == nil) {
			t.Fatal("network mutation must not repeat with --watch")
		}
	}
}
func TestNetworkAddressesShowInheritanceWithoutChangingJSON(t *testing.T) {
	for _, locale := range []cliLocale{localeZH, localeEN, localeJA} {
		for _, inherited := range []bool{true, false} {
			address := "http://10.0.0.1:1685"
			rawAddress := address
			if inherited {
				address, rawAddress = "https://transfer.example", ""
			}
			data := map[string]any{"publicUrl": "https://transfer.example", "agentUrl": rawAddress, "effectiveAgentUrl": address}
			result := map[string]any{"success": true, "data": data}
			var out bytes.Buffer
			renderCLIResult(&cliLocaleWriter{Writer: &out, locale: locale}, result)
			for _, want := range []string{locale.text("旧版默认地址"), locale.text("Agent 连接地址"), address} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q: %s", want, out.String())
				}
			}
			if strings.Contains(out.String(), locale.text("沿用旧版默认地址")) != inherited {
				t.Fatalf("wrong address inheritance: %s", out.String())
			}
			if data["agentUrl"] != rawAddress || data["effectiveAgentUrl"] != address {
				t.Fatal("rendering changed the structured API response")
			}
		}
	}
}

func TestMasterAddressValidationRejectsBadOriginsBeforeHTTP(t *testing.T) {
	for _, flag := range []string{"--public-url", "--agent-url"} {
		for _, value := range []string{"ftp://host", "http://host/path", "http://user:secret@host", "http://host?query", "http://host#fragment", "http://host:0", "http://host:65536", "http://host:abc", "http://host/\n"} {
			client := adminTestClient(t, func(http.ResponseWriter, *http.Request) {
				t.Fatal("invalid URL made an HTTP request")
			})
			if _, err := nodeNetwork(client, []string{flag, value}); err == nil {
				t.Fatalf("accepted %s %q", flag, value)
			}
		}
	}
}

func TestNodeDetailsAliasesPreserveIncompleteResultsAndTranslateLabels(t *testing.T) {
	for _, group := range []string{"node", "agent"} {
		for _, complete := range []bool{true, false} {
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/master/nodes/storage-node/details" {
					t.Fatal(r.Method, r.URL)
				}
				adminTestJSON(t, w, map[string]any{"success": true, "data": map[string]any{
					"complete": complete, "hostname": "storage-node", "localIps": []string{"10.0.0.2"}, "connectionAddress": "10.0.0.2",
					"mounts": []map[string]any{{"path": "/mnt/data", "source": "nfs.example:/share", "fileSystem": "nfs4", "readOnly": false}},
				}})
			})
			result, err := runAdminCommand(client, group, []string{"details", "storage-node"})
			if err != nil || incompleteResult(result) != !complete {
				t.Fatalf("%s details: %v %v", group, result, err)
			}
			if !watchable([]string{group, "details", "storage-node"}) {
				t.Fatal("details should allow --watch")
			}
			for _, tc := range []struct {
				locale cliLocale
				label  string
			}{{localeZH, "挂载目录"}, {localeEN, "Mounted directories"}, {localeJA, "マウントディレクトリ"}} {
				var out bytes.Buffer
				renderCLIResult(&cliLocaleWriter{Writer: &out, locale: tc.locale}, result)
				for _, text := range []string{tc.label, "/mnt/data", "10.0.0.2", "false", "nfs4"} {
					if !strings.Contains(out.String(), text) {
						t.Fatalf("missing %q: %s", text, out.String())
					}
				}
			}
		}
	}
}
