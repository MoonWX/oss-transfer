package master

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAPILanguageNegotiation(t *testing.T) {
	for _, test := range []struct{ header, want string }{
		{"en-US", "en-US"}, {"zh-TW", "zh-CN"}, {"JA-jp", "ja-JP"},
		{"en;q=0.2, ja-JP;q=0.9, zh;q=0.5", "ja-JP"},
		{"fr-FR, zh-CN;q=0.8, en;q=0.2", "zh-CN"},
		{"ja;q=0, en;q=0.5", "en-US"}, {"ja;q=invalid, zh", "zh-CN"},
		{"ja;q=NaN, zh;q=2, en", "en-US"}, {"de-DE", "en-US"},
		{"en;q=0, *;q=0.5", "zh-CN"}, {"ja;q=0.8, zh;q=0.8", "ja-JP"},
	} {
		t.Run(test.header, func(t *testing.T) {
			if got := negotiateLocale(test.header); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func localizedRequest(client *http.Client, method, url, token, language string, body any) (int, http.Header, map[string]any, error) {
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		input = bytes.NewReader(encoded)
	}
	r, err := http.NewRequest(method, url, input)
	if err != nil {
		return 0, nil, nil, err
	}
	if language != "" {
		r.Header.Set("Accept-Language", language)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(r)
	if err != nil {
		return 0, nil, nil, err
	}
	defer response.Body.Close()
	var result map[string]any
	err = json.NewDecoder(response.Body).Decode(&result)
	return response.StatusCode, response.Header, result, err
}

// Requests use real HTTP handlers and the persisted auth store. A global locale
// would let concurrent users change each other's messages; this test rejects it.
func TestAPIAuthLanguagesAreRequestScoped(t *testing.T) {
	f := newAgentHTTPFixture(t)
	tests := []struct{ header, locale, want string }{
		{"zh-CN", "zh-CN", "请先登录"},
		{"en-US", "en-US", "Sign in required"},
		{"ja-JP", "ja-JP", "ログインしてください"},
		{"fr-FR", "en-US", "Sign in required"},
		{"", "", "Sign in required"},
	}
	var group sync.WaitGroup
	for round := 0; round < 8; round++ {
		for _, test := range tests {
			group.Add(1)
			go func() {
				defer group.Done()
				code, headers, result, err := localizedRequest(f.http.Client(), "GET", f.http.URL+"/api/auth/current", "", test.header, nil)
				if err != nil {
					t.Error(err)
					return
				}
				if code != http.StatusUnauthorized || result["success"] != false || result["message"] != test.want {
					t.Errorf("language %q: code=%d response=%v", test.header, code, result)
				}
				if headers.Get("Content-Language") != test.locale {
					t.Errorf("language %q: headers=%v", test.header, headers)
				}
				if test.header != "" && !strings.Contains(headers.Get("Vary"), "Accept-Language") {
					t.Errorf("missing Vary: %v", headers)
				}
			}()
		}
	}
	group.Wait()
	for _, test := range []struct{ header, want string }{
		{"en", "Setup is already complete"}, {"zh", "初始化已完成"}, {"ja", "初期設定は完了しています"},
	} {
		code, _, result, err := localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/setup/complete", "", test.header, map[string]any{})
		if err != nil || code != http.StatusConflict || result["message"] != test.want {
			t.Fatalf("setup %q: %d %v %v", test.header, code, result, err)
		}
	}
}

// The preflight goes through an enrolled Agent and probes a real local path.
// Only its application message changes; paths, addresses and API fields do not.
func TestAPILocalizedPreflightPreservesPaths(t *testing.T) {
	f := newAgentHTTPFixture(t)
	_, node := taskAPIAgent(t, f)
	missing := filepath.Join(t.TempDir(), "源路径不存在 {key} spaced name")
	upload := taskCreateRequest{
		Direction:           "LOCAL_TO_OSS",
		SourceEndpoint:      &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{node.Address, missing}}},
		DestinationEndpoint: &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "destination", ObjectKey: "源路径不存在 / target"},
	}
	for _, test := range []struct{ language, want string }{
		{"zh-CN", "源路径不存在"}, {"en-US", "Source path does not exist"}, {"ja-JP", "転送元のパスが存在しません"}, {"", "源路径不存在"},
	} {
		code, _, result, err := localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, test.language, []taskCreateRequest{upload})
		if err != nil || code != http.StatusOK {
			t.Fatalf("preflight: %d %v %v", code, result, err)
		}
		data := result["data"].(map[string]any)
		checks := data["nodeBindings"].([]any)
		if data["valid"] != false || len(checks) != 1 {
			t.Fatal(data)
		}
		check := checks[0].(map[string]any)
		if check["message"] != test.want || check["localPath"] != missing || check["executionNodeIp"] != node.Address || check["pathType"] != "MISSING" {
			t.Fatalf("localized preflight changed data: %v", check)
		}
		if data["validationErrors"].([]any)[0] != node.Address+": "+test.want {
			t.Fatal(data)
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("preflight changed the filesystem: %v", err)
	}
}

func TestAPILocalizationKeepsUnknownErrorsAndUserValues(t *testing.T) {
	for _, language := range []string{"zh-CN", "en-US", "ja-JP", ""} {
		r := httptest.NewRequest("GET", "/api/example", nil)
		if language != "" {
			r.Header.Set("Accept-Language", language)
		}
		recorder := httptest.NewRecorder()
		w, _ := localizedAPI(recorder, r)
		raw := `S3 SlowDown: key="源路径不存在 / Task not found {name}" request-id=AbC123`
		fail(w, http.StatusBadGateway, raw)
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["message"] != raw || recorder.Code != http.StatusBadGateway {
			t.Fatal(body)
		}

		recorder = httptest.NewRecorder()
		w, _ = localizedAPI(recorder, r)
		writeJSON(w, 200, map[string]any{"status": "SKIP", "key": "执行节点未连接", "name": "Task not found", "path": raw})
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["status"] != "SKIP" || body["key"] != "执行节点未连接" || body["name"] != "Task not found" || body["path"] != raw {
			t.Fatalf("data translated: %v", body)
		}
	}
	// Empty and absent headers keep the old contract, including Chinese text.
	for _, header := range []string{"", "  "} {
		r := httptest.NewRequest("GET", "/api/example", nil)
		r.Header.Set("Accept-Language", header)
		w, _ := localizedAPI(httptest.NewRecorder(), r)
		if got := localize(w, "存储连接已保存"); got != "存储连接已保存" {
			t.Fatal(fmt.Sprintf("legacy response changed: %q", got))
		}
	}
}
