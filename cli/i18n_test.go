package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Existing tests assert Chinese text. Do not let a runner's system locale change
// those fixtures; locale tests below explicitly choose each environment.
func TestMain(m *testing.M) { os.Setenv("OSS_TRANSFER_LANG", "zh"); os.Exit(m.Run()) }

func clearLocaleEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("OSS_TRANSFER_CLI_CONFIG_DIR", t.TempDir())
	for _, name := range []string{"OSS_TRANSFER_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		t.Setenv(name, "")
	}
}
func TestCLILocaleSelection(t *testing.T) {
	clearLocaleEnvironment(t)
	wantFallback := localeEN
	if language, ok := normalizeCLILocale(platformLocaleName()); ok {
		wantFallback = language
	}
	if got, _ := resolveCLILocale(nil); got != wantFallback {
		t.Fatal(got)
	}
	for _, test := range []struct {
		value string
		want  cliLocale
	}{{"ja_JP.UTF-8", localeJA}, {"zh-Hant-TW", localeZH}, {"en_US.UTF-8", localeEN}, {"C.UTF-8", localeEN}, {"POSIX", localeEN}, {"fr_FR", localeEN}} {
		t.Setenv("LANG", test.value)
		got, err := resolveCLILocale(nil)
		if err != nil || got != test.want {
			t.Fatalf("%s: %s %v", test.value, got, err)
		}
	}
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_MESSAGES", "ja_JP.UTF-8")
	if got, _ := resolveCLILocale(nil); got != localeJA {
		t.Fatal(got)
	}
	t.Setenv("LC_ALL", "zh_CN.UTF-8")
	if got, _ := resolveCLILocale(nil); got != localeZH {
		t.Fatal(got)
	}
	t.Setenv("OSS_TRANSFER_LANG", "ja")
	if got, _ := resolveCLILocale(nil); got != localeJA {
		t.Fatal(got)
	}
	if got, err := resolveCLILocale([]string{"--lang=en-US"}); got != localeEN || err != nil {
		t.Fatal(got, err)
	}
	if got, _ := resolveCLILocale([]string{"task", "--object-key=--lang", "--", "--lang", "en"}); got != localeJA {
		t.Fatal(got)
	}
	for _, value := range []string{"fr", "C", "C.UTF-8", "POSIX", "", "enough"} {
		_, err := resolveCLILocale([]string{"--lang", value})
		if err == nil {
			t.Fatalf("accepted unsupported --lang %q", value)
		}
	}
}
func TestCLIHelpAllLanguagesAndAliases(t *testing.T) {
	groups := [][]string{{}, {"config"}, {"task"}, {"task", "create"}, {"task", "import"}, {"node"}, {"agent"}, {"setup"}, {"storage"}, {"limit"}, {"exceptions"}, {"object"}, {"update"}, {"backup"}, {"login"}, {"tools"}}
	for _, group := range groups {
		source := helpFor(group)
		for _, locale := range []cliLocale{localeZH, localeEN, localeJA} {
			help := helpForLocale(group, locale)
			if help == "" || locale != localeZH && help == source {
				t.Fatalf("missing %s help %v", locale, group)
			}
			// All declared flags remain available in the translated reference.
			flags := regexp.MustCompile(`--[a-z][a-z-]+`).FindAllString(source, -1)
			for _, flag := range flags {
				if !strings.Contains(help, flag) {
					t.Errorf("%s %v help missing %s", locale, group, flag)
				}
			}
		}
	}
	for _, locale := range []cliLocale{localeEN, localeJA} {
		if helpForLocale([]string{"add-task"}, locale) != helpForLocale([]string{"task", "create"}, locale) {
			t.Fatal("alias help drift")
		}
	}
}
func TestCLILocalizedErrorsPreserveUserTextAndUnwrap(t *testing.T) {
	raw := errors.New("来源/正常/配置目标")
	err := cliErrorf("第 %d 项: %w", 7, cliErrorf("请求失败: %w", raw))
	if !errors.Is(err, raw) {
		t.Fatal("error chain was lost")
	}
	for _, test := range []struct {
		locale cliLocale
		want   string
	}{{localeZH, "第 7 项"}, {localeEN, "Item 7"}, {localeJA, "7 件目"}} {
		got := localizedCLIError(test.locale, err)
		if !strings.Contains(got, test.want) || !strings.Contains(got, raw.Error()) || strings.Contains(got, "%!") {
			t.Fatal(got)
		}
	}
}
func TestCLILanguageChangesLabelsNotDataOrJSON(t *testing.T) {
	for _, test := range []struct {
		locale  string
		heading string
	}{{"zh", "连接名称"}, {"en", "Connection"}, {"ja", "接続名"}} {
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept-Language") != test.locale {
				t.Fatalf("language header: %q", r.Header.Get("Accept-Language"))
			}
			io.WriteString(w, `{"success":true,"configs":[{"alias":"配置目标/正常/来源","endpoint":"https://example.invalid/来源","accessKey":"secret","secretKey":"secret"}],"message":"配置目标"}`)
		}
		code, out, stderr := runMockCLI(t, []string{"--lang", test.locale, "storage", "list"}, handler)
		if code != 0 || stderr != "" || !strings.Contains(out, test.heading) || !strings.Contains(out, "配置目标/正常/来源") || !strings.Contains(out, "https://example.invalid/来源") || !strings.Contains(out, "\n配置目标\n") {
			t.Fatalf("%s %d %s %s", test.locale, code, out, stderr)
		}
		code, out, stderr = runMockCLI(t, []string{"--lang", test.locale, "storage", "list", "--json"}, handler)
		var result map[string]interface{}
		if code != 0 || json.Unmarshal([]byte(out), &result) != nil || result["message"] != "配置目标" || result["success"] != true {
			t.Fatalf("%s %d %s %s", test.locale, code, out, stderr)
		}
		configs := result["configs"].([]interface{})
		alias := configs[0].(map[string]interface{})["alias"]
		if alias != "配置目标/正常/来源" {
			t.Fatal(alias)
		}
	}
}
func TestCLIConcurrentInvocationsDoNotShareLanguage(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			language, want := "en", "--watch must"
			if i%2 == 1 {
				language, want = "ja", "--watch は"
			}
			var out, stderr bytes.Buffer
			code := runCLI([]string{"--watch", "0", "--lang", language}, strings.NewReader(""), &out, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), want) {
				t.Errorf("%s %d %s", language, code, stderr.String())
			}
		}(i)
	}
	wg.Wait()
}
func TestCLIOfflineToolsUseSelectedOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "状态.txt"), []byte("first\nsecond\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ locale, want string }{{"en", "Total lines: 2"}, {"ja", "合計行数: 2"}, {"zh", "总行数: 2"}} {
		var out, stderr bytes.Buffer
		code := runCLI([]string{"--lang", test.locale, "count-lines", dir}, strings.NewReader(""), &out, &stderr)
		if code != 0 || stderr.Len() != 0 || !strings.Contains(out.String(), test.want) {
			t.Fatalf("%s %d %s %s", test.locale, code, out.String(), stderr.String())
		}
	}
	for _, cmd := range []string{"count-lines", "diff-objects"} {
		var out, stderr bytes.Buffer
		code := runCLI([]string{"--lang", "en", cmd}, nil, &out, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "Usage:") {
			t.Fatal(code, stderr.String())
		}
	}
}
func TestCLICatalogCoversControlledMessagesAndFormats(t *testing.T) {
	// Missing translations are real visible regressions. Check source-key lookup
	// sites and compare formatting verbs so a translation cannot drop an argument.
	verbs := regexp.MustCompile(`%(?:\[[0-9]+\])?[-+#0 ]*(?:[0-9]+|\*)?(?:\.[0-9]+)?[a-zA-Z%]`)
	for key, values := range cliMessages {
		source := strings.Join(verbs.FindAllString(key, -1), "|")
		for _, value := range values {
			if value == "" || strings.Join(verbs.FindAllString(value, -1), "|") != source {
				t.Errorf("format drift %q => %q", key, value)
			}
		}
	}
	paths, _ := filepath.Glob("*.go")
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "i18n_catalog") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(node ast.Node) bool {
			c, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			idx := -1
			if id, ok := c.Fun.(*ast.Ident); ok {
				if id.Name == "cliText" {
					idx = 1
				}
				if id.Name == "cliErrorf" {
					idx = 0
				}
			}
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "text" {
				idx = 0
			}
			if idx < 0 || idx >= len(c.Args) {
				return true
			}
			literal, ok := c.Args[idx].(*ast.BasicLit)
			if !ok {
				return true
			}
			key, _ := strconv.Unquote(literal.Value)
			if regexp.MustCompile(`[\p{Han}]`).MatchString(key) {
				if _, exists := cliMessages[key]; !exists {
					t.Errorf("missing catalog in %s: %q", path, key)
				}
			}
			return true
		})
	}
}

func TestCLITaskDetailsTranslateLabelsAndPreserveObjectKeys(t *testing.T) {
	task := map[string]interface{}{
		"taskIndex": 224, "direction": "OSS_TO_OSS", "status": "COMPLETED",
		"sourceBucket": "来源", "destBucket": "目标", "objectKey": "正常/源对象 Key.txt",
		"keyMapping":   map[string]interface{}{"mode": "EXACT_KEY", "targetKey": "配置目标/目标对象 Key.txt"},
		"errorMessage": "失败原因/正常", "conflictPolicy": "OVERWRITE",
	}
	for _, test := range []struct {
		locale cliLocale
		want   []string
	}{{localeEN, []string{"Conflict policy", "Source object key", "Target object key", "Failure reason", "Node limits only"}}, {localeJA, []string{"競合ポリシー", "転送元オブジェクト Key", "転送先オブジェクト Key", "失敗の理由", "ノードの上限に従う"}}} {
		var buffer bytes.Buffer
		renderTaskDetail(&cliLocaleWriter{Writer: &buffer, locale: test.locale}, task)
		text := buffer.String()
		for _, want := range append(test.want, "正常/源对象 Key.txt", "配置目标/目标对象 Key.txt", "失败原因/正常", "COMPLETED", "OVERWRITE", "EXACT_KEY") {
			if !strings.Contains(text, want) {
				t.Fatalf("%s missing %q: %s", test.locale, want, text)
			}
		}
	}
}
