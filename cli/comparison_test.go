package main

import (
	"net/http"
	"testing"
)

func TestRangeComparisonPreservesPrefixesAndSupportsStatusCancel(t *testing.T) {
	calls := 0
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			body := adminTestBody(t, r)
			if r.Method != "POST" || r.URL.Path != "/api/master/comparisons" || body["sourcePrefix"] != " dir/+中文 " || body["destPrefix"] != "backup/" || body["mode"] != "content" || body["scope"] != "prefix" {
				t.Fatalf("bad range request: %s %+v", r.URL, body)
			}
		case 2:
			if r.URL.Query().Get("after") != "100" || r.URL.Query().Get("limit") != "25" {
				t.Fatalf("bad pagination: %s", r.URL)
			}
		case 3:
			if r.Method != "POST" || r.URL.Path != "/api/master/comparisons/job-1/cancel" {
				t.Fatalf("bad cancel: %s %s", r.Method, r.URL)
			}
		}
		adminTestJSON(t, w, map[string]interface{}{"id": "job-1", "status": "RUNNING"})
	})
	for _, args := range [][]string{{"compare", "--source-bucket", "source/a", "--dest-bucket", "dest/b", "--source-prefix", " dir/+中文 ", "--dest-prefix", "backup/", "--mode", "content"}, {"status", "job-1", "--after", "100", "--limit", "25"}, {"cancel", "job-1"}} {
		if _, err := runAdminCommand(client, "object", args); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatal(calls)
	}
}

func TestRangeComparisonRejectsMixedKeysAndInvalidModes(t *testing.T) {
	client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid comparison reached server") })
	for _, args := range [][]string{{"--scope", "prefix", "--source-key", "a"}, {"--scope", "object", "--source-prefix", "a"}, {"--mode", "md5"}, {"--scope", "object", "--mode", "content"}} {
		if _, err := runAdminCommand(client, "object", append([]string{"compare", "--source-bucket", "source/a", "--dest-bucket", "dest/b"}, args...)); err == nil {
			t.Fatalf("invalid options accepted: %v", args)
		}
	}
}
