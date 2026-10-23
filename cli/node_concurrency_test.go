package main

import (
	"net/http"
	"testing"
)

func TestNodeAndAgentConcurrencyCommandsUseSameAPI(t *testing.T) {
	for _, run := range []func(*ApiClient, string, []string) (map[string]interface{}, error){runNodeAdmin, runAgentAdmin} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "PUT" || r.URL.Path != "/api/master/nodes/storage-node" {
				t.Fatal(r.URL)
			}
			body := adminTestBody(t, r)
			if len(body) != 2 || body["maxConcurrentChildren"] != float64(12) || body["objectsPerChild"] != float64(6) {
				t.Fatal(body)
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true})
		})
		if _, err := run(client, "update", []string{"storage-node", "--child-concurrency", "12", "--object-concurrency", "6"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrencyCLIRejectsInvalidBoundsBeforeRequest(t *testing.T) {
	client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid bounds sent to server") })
	for _, args := range [][]string{
		{"--child-concurrency", "0"}, {"--object-concurrency", "33"}, {"--child-concurrency", "2.5"}, {"--child-concurrency", "64", "--object-concurrency", "32"}, {"--object-concurrency", "1", "--object-concurrency", "2"},
	} {
		if _, err := agentUpdate(client, append([]string{"storage-node"}, args...)); err == nil {
			t.Fatal("invalid concurrency accepted", args)
		}
	}
}
