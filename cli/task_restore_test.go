package main

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestTaskRestoreUsesSelectedTrashIDsWithoutActiveTaskLookup(t *testing.T) {
	requests := 0
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "POST" || r.URL.Path != "/api/master/restore" {
			t.Fatalf("restore must not query active tasks or another mutation: %s %s", r.Method, r.URL)
		}
		body := adminTestBody(t, r)
		if len(body) != 1 || !reflect.DeepEqual(body["taskIds"], []interface{}{float64(224), float64(225), float64(226), float64(227)}) {
			t.Fatalf("restore changed selected IDs: %#v", body)
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "accepted": 4, "failed": 0, "partial": false})
	})
	if _, err := runTaskCommand(client, []string{"restore", "#224,225", "225-227"}); err != nil || requests != 1 {
		t.Fatalf("restore: requests=%d err=%v", requests, err)
	}
}

func TestTaskRestoreRejectsInvalidScopeBeforeAnyRequest(t *testing.T) {
	client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid restore reached server") })
	for _, args := range [][]string{nil, {"0"}, {"-1"}, {"224,"}, {"224-223"}, {"1-10001"}, {"99999999999999999999"}, {"224", "--yes"}, {"--all"}, {"224", "--force"}} {
		if _, err := runTaskCommand(client, append([]string{"restore"}, args...)); err == nil {
			t.Errorf("invalid restore accepted: %v", args)
		}
	}
	if watchable([]string{"task", "restore", "224"}) {
		t.Fatal("restore must not repeat through --watch")
	}
}

func TestTaskRestoreExitStatusAndLocalizedHelp(t *testing.T) {
	for _, language := range []string{"zh", "en", "ja"} {
		t.Run(language, func(t *testing.T) {
			for _, test := range []struct {
				name, body   string
				status, exit int
			}{
				{"success", `{"success":true,"accepted":1,"failed":0,"partial":false}`, 200, 0},
				{"partial", `{"success":true,"accepted":0,"failed":1,"partial":true}`, 200, 3},
				{"unauthorized", `{"success":false,"message":"unauthorized"}`, 401, 1},
				{"server-error", `{"success":false,"message":"database unavailable"}`, 500, 1},
				{"business-error", `{"success":false,"message":"restore rejected"}`, 200, 1},
			} {
				t.Run(test.name, func(t *testing.T) {
					code, output, stderr := runMockCLI(t, []string{"--lang", language, "--json", "task", "restore", "224"}, func(w http.ResponseWriter, r *http.Request) {
						if r.Method != "POST" || r.URL.Path != "/api/master/restore" {
							t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
						}
						w.WriteHeader(test.status)
						_, _ = w.Write([]byte(test.body))
					})
					if code != test.exit || (test.exit != 0 && stderr == "") {
						t.Fatalf("exit=%d, want=%d: %s %s", code, test.exit, output, stderr)
					}
				})
			}
			for _, command := range [][]string{{"--help"}, {"task", "--help"}} {
				code, output, stderr := runMockCLI(t, append([]string{"--lang", language}, command...), func(http.ResponseWriter, *http.Request) { t.Fatal("help reached server") })
				if code != 0 || !strings.Contains(output, "restore <ID...>") || strings.Contains(output, "creation <") {
					t.Fatalf("stale or missing help: %d %s %s", code, output, stderr)
				}
			}
			code, _, stderr := runMockCLI(t, []string{"--lang", language, "task", "creation", "obsolete-id"}, func(http.ResponseWriter, *http.Request) { t.Fatal("removed command reached server") })
			if code != 1 || !strings.Contains(stderr, "creation") {
				t.Fatalf("removed command not rejected: %d %s", code, stderr)
			}
		})
	}
}
