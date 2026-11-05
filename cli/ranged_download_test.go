package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

const rangedDownloadPrecheckOK = `{"success":true,"data":{"valid":true,"nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/data","valid":true}]}}`

func TestCreateDownloadRangedDownloadOption(t *testing.T) {
	for _, test := range []struct {
		name  string
		extra []string
		want  interface{}
	}{
		{"default omits the field", nil, nil},
		{"explicit auto omits the field", []string{"--ranged-download", "auto"}, nil},
		{"off is sent", []string{"--ranged-download", "off"}, "off"},
		{"value is case-insensitive", []string{"--ranged-download=OFF"}, "off"},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := func(rows []map[string]interface{}) {
				value, present := rows[0]["rangedDownload"]
				if test.want == nil && present || test.want != nil && value != test.want {
					t.Fatalf("rangedDownload %#v present=%v, want %#v", value, present, test.want)
				}
			}
			client := creationTestClient(t,
				creationExchange{method: "POST", path: "/local-transfers/validate", response: rangedDownloadPrecheckOK, inspect: check},
				creationExchange{method: "POST", path: "/local-transfers", response: `{"success":true,"data":{"acceptedCount":1,"parentTaskIds":[42]}}`, inspect: check},
			)
			args := append([]string{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data"}, test.extra...)
			if result, err := runTaskCreate(client, args); err != nil || result["submissionStatus"] != "accepted" {
				t.Fatalf("%#v %v", result, err)
			}
		})
	}
}

func TestCreateDownloadValidateOnlyCarriesRangedDownload(t *testing.T) {
	client := creationTestClient(t, creationExchange{
		method: "POST", path: "/local-transfers/validate", response: rangedDownloadPrecheckOK,
		inspect: func(rows []map[string]interface{}) {
			if rows[0]["rangedDownload"] != "off" {
				t.Fatal(rows)
			}
		},
	})
	result, err := runTaskCreate(client, []string{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--ranged-download", "off", "--validate-only"})
	if err != nil || result["submissionStatus"] != "validated" {
		t.Fatalf("%#v %v", result, err)
	}
}

func TestCreateRangedDownloadUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--ranged-download", "on"},
		{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--ranged-download", ""},
		{"local-to-oss", "target/dst", "--binding", "10.0.0.1=/mnt/data", "--ranged-download", "off"},
		{"local-to-oss", "target/dst", "--binding", "10.0.0.1=/mnt/data", "--ranged-download", "auto"},
		{"oss-to-oss", "source/src", "target/dst", "--ranged-download", "off"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := runTaskCreate(creationTestClient(t), args)
			if err == nil || !isCLIUsageError(err) || !strings.Contains(err.Error(), "--ranged-download") {
				t.Fatalf("want usage error, got %v", err)
			}
		})
	}
	if _, err := runTaskCreate(creationTestClient(t), []string{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--ranged-download", "off", "--ranged-download", "auto"}); err == nil {
		t.Fatal("repeated --ranged-download accepted")
	}
	for _, args := range [][]string{
		{"task", "create", "oss-to-oss", "source/src", "target/dst", "--ranged-download", "off"},
		{"task", "create", "oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--ranged-download", "sometimes"},
	} {
		code, _, stderr := runMockCLI(t, args, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected network") })
		if code != 2 || !strings.Contains(stderr, "--ranged-download") {
			t.Fatalf("%v: %d %s", args, code, stderr)
		}
	}
}

func TestImportLocalRangedDownload(t *testing.T) {
	file := creationImportFile(t, "local.json", `[{"direction":"OSS_TO_LOCAL","sourceEndpoint":{"type":"OSS","s3Alias":"source","bucket":"src"},"destinationEndpoint":{"type":"LOCAL","nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/data"}]},"rangedDownload":"off"}]`)
	check := func(rows []map[string]interface{}) {
		if rows[0]["rangedDownload"] != "off" {
			t.Fatal(rows)
		}
	}
	client := creationTestClient(t,
		creationExchange{method: "POST", path: "/local-transfers/validate", response: rangedDownloadPrecheckOK, inspect: check},
		creationExchange{method: "POST", path: "/local-transfers", response: `{"success":true,"data":{"acceptedCount":1,"parentTaskIds":[83]}}`, inspect: check},
	)
	if _, err := runTaskImport(client, []string{"local", file}); err != nil {
		t.Fatal(err)
	}
}

func TestImportRejectsInvalidRangedDownload(t *testing.T) {
	download := `"direction":"OSS_TO_LOCAL","sourceEndpoint":{"type":"OSS","s3Alias":"source","bucket":"src"},"destinationEndpoint":{"type":"LOCAL","nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/data"}]}`
	upload := `"direction":"LOCAL_TO_OSS","sourceEndpoint":{"type":"LOCAL","nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/data"}]},"destinationEndpoint":{"type":"OSS","s3Alias":"target","bucket":"dst"}`
	for _, test := range []struct{ kind, data string }{
		{"local", `[{` + download + `,"rangedDownload":"OFF"}]`},
		{"local", `[{` + download + `,"rangedDownload":false}]`},
		{"local", `[{` + download + `,"rangedDownload":"never"}]`},
		{"local", `[{` + upload + `,"rangedDownload":"off"}]`},
		{"buckets", `[{"sourceBucket":"a","destBucket":"b","rangedDownload":"off"}]`},
		{"tasks", `[{"sourceBucket":"a","destBucket":"b","rangedDownload":"off"}]`},
	} {
		t.Run(test.kind+test.data, func(t *testing.T) {
			file := creationImportFile(t, "input.json", test.data)
			_, err := runTaskImport(creationTestClient(t), []string{test.kind, file})
			if err == nil || !isCLIUsageError(err) || !strings.Contains(err.Error(), "rangedDownload") {
				t.Fatalf("want usage error, got %v", err)
			}
		})
	}
	// auto is the default for every direction and passes through unchanged.
	file := creationImportFile(t, "input.json", `[{"sourceBucket":"a","destBucket":"b","rangedDownload":"auto"}]`)
	client := creationTestClient(t, creationExchange{
		method: "POST", path: "/add-buckets", response: `{"success":true,"addedCount":1,"creationIds":["c1"]}`,
		inspect: func(rows []map[string]interface{}) {
			if rows[0]["rangedDownload"] != "auto" {
				t.Fatal(rows)
			}
		},
	})
	if _, err := runTaskImport(client, []string{"buckets", file}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskDetailShowsRangedDownloadForLocalDestinations(t *testing.T) {
	render := func(locale cliLocale, task map[string]interface{}) string {
		var buffer bytes.Buffer
		renderTaskDetail(&cliLocaleWriter{Writer: &buffer, locale: locale}, task)
		return buffer.String()
	}
	download := func(extra map[string]interface{}) map[string]interface{} {
		task := map[string]interface{}{"taskIndex": 7, "direction": "OSS_TO_LOCAL", "status": "RUNNING"}
		for key, value := range extra {
			task[key] = value
		}
		return task
	}
	for _, test := range []struct {
		locale cliLocale
		task   map[string]interface{}
		want   string
	}{
		{localeZH, download(map[string]interface{}{"rangedDownload": "off"}), "分段下载"},
		{localeEN, download(map[string]interface{}{"rangedDownload": "off"}), "Ranged download"},
		{localeJA, download(map[string]interface{}{"rangedDownload": "off"}), "範囲分割ダウンロード"},
	} {
		text := render(test.locale, test.task)
		line := ""
		for _, candidate := range strings.Split(text, "\n") {
			if strings.HasPrefix(candidate, test.want) {
				line = candidate
			}
		}
		if !strings.HasSuffix(strings.TrimSpace(line), "off") {
			t.Fatalf("%s: %q in %s", test.locale, line, text)
		}
	}
	// Tasks created before the setting existed report auto.
	if text := render(localeEN, download(nil)); !strings.Contains(text, "Ranged download") || !strings.Contains(text, "auto") {
		t.Fatal(text)
	}
	// A child row without the field inherits the parent's value; do not guess.
	if text := render(localeEN, download(map[string]interface{}{"childTaskId": "7-1"})); strings.Contains(text, "Ranged download") {
		t.Fatal(text)
	}
	for _, direction := range []string{"OSS_TO_OSS", "LOCAL_TO_OSS"} {
		if text := render(localeEN, map[string]interface{}{"taskIndex": 8, "direction": direction, "rangedDownload": "auto"}); strings.Contains(text, "Ranged download") {
			t.Fatalf("%s: %s", direction, text)
		}
	}
}

func TestTaskShowRendersRangedDownloadFromMaster(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/children"):
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"items": []interface{}{}}})
		default:
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"taskIndex": 7, "direction": "OSS_TO_LOCAL", "status": "RUNNING", "rangedDownload": "off"}})
		}
	}
	code, out, stderr := runMockCLI(t, []string{"--lang", "en", "task", "show", "7"}, handler)
	if code != 0 || !strings.Contains(out, "Ranged download") || !strings.Contains(out, "off") {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	code, out, stderr = runMockCLI(t, []string{"task", "show", "7", "--json"}, handler)
	if code != 0 || !strings.Contains(out, `"rangedDownload": "off"`) {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
}
