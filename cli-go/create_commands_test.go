package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type creationTransport func(*http.Request) (*http.Response, error)

func (fn creationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type creationExchange struct {
	method   string
	path     string
	response string
	inspect  func([]map[string]interface{})
	status   int
}

func creationTestClient(t *testing.T, exchanges ...creationExchange) *ApiClient {
	t.Helper()
	index := 0
	t.Cleanup(func() {
		if index != len(exchanges) {
			t.Errorf("received %d requests, want %d", index, len(exchanges))
		}
	})
	return &ApiClient{
		baseUrl: "http://unit.invalid/api/master",
		client: &http.Client{Transport: creationTransport(func(request *http.Request) (*http.Response, error) {
			if index >= len(exchanges) {
				t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
			}
			exchange := exchanges[index]
			index++
			if request.Method != exchange.method || request.URL.Path != "/api/master"+exchange.path {
				t.Fatalf("request %s %s, want %s /api/master%s", request.Method, request.URL.Path, exchange.method, exchange.path)
			}
			if exchange.inspect != nil {
				var rows []map[string]interface{}
				if err := json.NewDecoder(request.Body).Decode(&rows); err != nil {
					t.Fatal(err)
				}
				exchange.inspect(rows)
			}
			status := exchange.status
			if status == 0 {
				status = http.StatusOK
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(exchange.response)), Request: request}, nil
		})},
	}
}

func creationNodesExchange() creationExchange {
	return creationExchange{method: "GET", path: "/local-nodes", response: `{"success":true,"data":{"nodes":[{"executionNodeIp":"10.0.0.1","reachable":true},{"executionNodeIp":"10.0.0.2","reachable":true},{"executionNodeIp":"10.0.0.3","reachable":false}]}}`}
}

func TestCreateOssMatchesWebPayload(t *testing.T) {
	client := creationTestClient(t, creationNodesExchange(), creationExchange{
		method: "POST", path: "/add-buckets", response: `{"success":true,"addedCount":1,"creationIds":["creation-42"]}`,
		inspect: func(rows []map[string]interface{}) {
			want := map[string]interface{}{
				"sourceS3Alias": "source", "sourceBucket": "src", "destS3Alias": "target", "destBucket": "dst",
				"prefix": " old/ ", "keyMapping": map[string]interface{}{"mode": "REPLACE_PREFIX", "sourcePrefix": " old/ ", "targetPrefix": " new/ "},
				"overwrite": true, "distributionMode": "balanced", "eligibleWorkerNodes": []interface{}{"10.0.0.2"},
				"dateAfter": "2026-09-01T12:30:00", "dateBefore": "2026-09-02T12:30:00", "taskBandwidthLimitMbps": 25.5,
			}
			if len(rows) != 1 || !reflect.DeepEqual(rows[0], want) {
				t.Fatalf("payload %#v, want %#v", rows, want)
			}
		},
	})
	result, err := runTaskCreate(client, []string{"oss-to-oss", "source/src", "target/dst", "--prefix", " old/ ", "--mapping", "replace-prefix", "--target-prefix", " new/ ", "--node", "10.0.0.2", "--overwrite", "--balanced", "--date-after", "2026-09-01T12:30", "--date-before", "2026-09-02 12:30:00", "--bandwidth-mbps", "25.5"})
	if err != nil || result["submissionStatus"] != "accepted" || result["creationIds"].([]interface{})[0] != "creation-42" {
		t.Fatalf("result %#v, error %v", result, err)
	}
}

func TestCreateOssDefaultNodesAndExactKey(t *testing.T) {
	client := creationTestClient(t, creationNodesExchange(), creationExchange{
		method: "POST", path: "/add-buckets", response: `{"success":true,"addedCount":1,"creationIds":["c1"]}`,
		inspect: func(rows []map[string]interface{}) {
			if !reflect.DeepEqual(rows[0]["eligibleWorkerNodes"], []interface{}{"10.0.0.1", "10.0.0.2"}) {
				t.Fatal(rows)
			}
			if rows[0]["objectKey"] != " leading/key " || !reflect.DeepEqual(rows[0]["keyMapping"], map[string]interface{}{"mode": "EXACT_KEY", "targetKey": " renamed/key "}) {
				t.Fatal(rows)
			}
		},
	})
	_, err := runTaskCreate(client, []string{"oss-to-oss", "source/src", "target/dst", "--object-key", " leading/key ", "--mapping", "exact-key", "--target-key", " renamed/key "})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateOssUnavailableNodeNeverSubmits(t *testing.T) {
	client := creationTestClient(t, creationNodesExchange())
	_, err := runTaskCreate(client, []string{"oss-to-oss", "source/src", "target/dst", "--node", "10.0.0.3"})
	if err == nil || !strings.Contains(err.Error(), "不可用") {
		t.Fatal(err)
	}
}

func TestCreateOssValidationBeforeNetwork(t *testing.T) {
	cases := [][]string{
		{"oss-to-oss", "source/src", "source/src"},
		{"oss-to-oss", "source/src", "target/dst", "--prefix", "x", "--object-key", "y"},
		{"oss-to-oss", "source/src", "target/dst", "--mapping", "exact-key", "--target-key", "renamed"},
		{"oss-to-oss", "source/src", "target/dst", "--mapping", "add-prefix"},
		{"oss-to-oss", "source/src", "target/dst", "--target-prefix", "archive/"},
		{"oss-to-oss", "source/src", "target/dst", "--date-after", "2026-02-30T12:00:00"},
		{"oss-to-oss", "source/src", "target/dst", "--date-after", "2026-09-01T12:00", "--date-before", "2026-09-01T12:00"},
		{"oss-to-oss", "source/src", "target/dst", "--bandwidth-mbps", "NaN"},
		{"oss-to-oss", "source/src", "target/dst", "--bandwidth-mbps", "0.01"},
		{"oss-to-oss", "source/src", "target/dst", "--bandwidth-mbps", "1000001"},
		{"oss-to-oss", "source/src", "target/dst", "--node"},
		{"oss-to-oss", "source/src", "target/dst", "--unknown"},
		{"oss-to-oss", "source/src", "target/dst", "--conflict", "skip", "--overwrite"},
		{"oss-to-oss", "source/src", "target/dst", "--distribution", "affinity", "--balanced"},
		{"oss-to-oss", "source/src", "target/dst", "--validate-only"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := runTaskCreate(creationTestClient(t), args)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestCreateDownloadPrechecksAndUsesNodeBindings(t *testing.T) {
	var checked []map[string]interface{}
	client := creationTestClient(t, creationExchange{
		method: "POST", path: "/local-transfers/validate", response: `{"success":true,"data":{"valid":true,"nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/data","valid":true}]}}`,
		inspect: func(rows []map[string]interface{}) { checked = rows },
	}, creationExchange{
		method: "POST", path: "/local-transfers", response: `{"success":true,"data":{"acceptedCount":1,"parentTaskIds":[42]}}`,
		inspect: func(rows []map[string]interface{}) {
			if !reflect.DeepEqual(rows, checked) {
				t.Fatal("precheck and submission differ")
			}
			row := rows[0]
			if row["direction"] != "OSS_TO_LOCAL" || row["conflictPolicy"] != "OVERWRITE" || row["targetRelativePath"] != "renamed.txt" {
				t.Fatal(row)
			}
			want := map[string]interface{}{"type": "LOCAL", "nodeBindings": []interface{}{map[string]interface{}{"executionNodeIp": "10.0.0.1", "localPath": "/mnt/data"}}}
			if !reflect.DeepEqual(row["destinationEndpoint"], want) {
				t.Fatal(row)
			}
		},
	})
	result, err := runTaskCreate(client, []string{"oss-to-local", "source/src", "--object-key", "report.txt", "--binding", "10.0.0.1=/mnt/data", "--target-relative-path", "renamed.txt", "--conflict", "overwrite"})
	if err != nil || result["submissionStatus"] != "accepted" {
		t.Fatalf("%#v %v", result, err)
	}
}

func TestCreateUploadDerivesDistributionAndValidatesOnly(t *testing.T) {
	client := creationTestClient(t, creationExchange{
		method: "POST", path: "/local-transfers/validate", response: `{"success":true,"data":{"valid":true,"nodeBindings":[{"valid":true,"pathType":"DIRECTORY"},{"valid":true,"pathType":"DIRECTORY"}]}}`,
		inspect: func(rows []map[string]interface{}) {
			if rows[0]["distributionMode"] != "balanced" || rows[0]["direction"] != "LOCAL_TO_OSS" {
				t.Fatal(rows)
			}
			if rows[0]["destinationEndpoint"].(map[string]interface{})["prefix"] != " upload/ " {
				t.Fatal(rows)
			}
		},
	})
	result, err := runTaskCreate(client, []string{"local-to-oss", "target/dst", "--binding", "10.0.0.1=/mnt/data", "--binding", "10.0.0.2=/mnt/data", "--target-prefix", " upload/ ", "--validate-only"})
	if err != nil || result["submissionStatus"] != "validated" {
		t.Fatalf("%#v %v", result, err)
	}
}

func TestCreateUploadPrecheckBlocksIncompatibleFileShapes(t *testing.T) {
	for _, test := range []struct {
		name     string
		args     []string
		response string
	}{
		{"directory cannot use exact key", []string{"--binding", "10.0.0.1=/mnt/data", "--target-key", "file"}, `{"success":true,"data":{"valid":true,"nodeBindings":[{"valid":true,"pathType":"DIRECTORY"}]}}`},
		{"single file cannot use two nodes", []string{"--binding", "10.0.0.1=/mnt/file", "--binding", "10.0.0.2=/mnt/file"}, `{"success":true,"data":{"valid":true,"nodeBindings":[{"valid":true,"pathType":"FILE"},{"valid":true,"pathType":"FILE"}]}}`},
		{"failed precheck", []string{"--binding", "10.0.0.1=/mnt/file"}, `{"success":true,"data":{"valid":false,"nodeBindings":[{"valid":false,"message":"source missing"}]}}`},
		{"incomplete precheck", []string{"--binding", "10.0.0.1=/mnt/file"}, `{"success":true,"data":{"valid":true,"nodeBindings":[]}}`},
		{"malformed precheck binding", []string{"--binding", "10.0.0.1=/mnt/file", "--target-key", "file"}, `{"success":true,"data":{"valid":true,"nodeBindings":[null]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := creationTestClient(t, creationExchange{method: "POST", path: "/local-transfers/validate", response: test.response})
			result, err := runTaskCreate(client, append([]string{"local-to-oss", "target/dst"}, test.args...))
			if err == nil || result["submissionStatus"] != "validation_failed" {
				t.Fatalf("%#v %v", result, err)
			}
		})
	}
}

func TestCreateLocalValidationBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"local-to-oss", "target/dst"},
		{"local-to-oss", "target/dst", "--binding", "10.0.0.1=relative"},
		{"local-to-oss", "target/dst", "--binding", "10.0.0.1=/mnt/data", "--binding", "10.0.0.1=/mnt/other"},
		{"local-to-oss", "target/dst", "--binding", "10.0.0.1=/mnt/data", "--target-prefix", "x", "--target-key", "x"},
		{"local-to-oss", "target/dst", "--binding", "10.0.0.1=/mnt/data", "--prefix", "x"},
		{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--target-relative-path", "file"},
		{"oss-to-local", "source/src", "--binding", "10.0.0.1=/mnt/data", "--object-key", "x", "--target-relative-path", "../file"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := runTaskCreate(creationTestClient(t), args); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func creationImportFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportBucketsCSVMatchesWebColumns(t *testing.T) {
	file := creationImportFile(t, "buckets.csv", "source/src,target/dst,2026-09-01T12:00,true,balanced\n")
	client := creationTestClient(t, creationExchange{
		method: "POST", path: "/add-buckets", response: `{"success":true,"addedCount":1,"creationIds":["c1"]}`,
		inspect: func(rows []map[string]interface{}) {
			if rows[0]["overwrite"] != true || rows[0]["distributionMode"] != "balanced" || rows[0]["dateAfter"] != "2026-09-01T12:00:00" {
				t.Fatal(rows)
			}
		},
	})
	if _, err := runTaskImport(client, []string{"buckets", file}); err != nil {
		t.Fatal(err)
	}
}

func TestImportSubtasksPreservesBoundsAndMapping(t *testing.T) {
	file := creationImportFile(t, "tasks.json", `[{"sourceBucket":"src","destBucket":"dst","sourceS3Alias":"s","destS3Alias":"d","sourceEndpoint":{"prefix":" x/ ","prefixFilterEnabled":true},"startKey":" x/ a","endKey":" x/ z","workerNode":"10.0.0.1","keyMapping":{"mode":"REPLACE_PREFIX","sourcePrefix":" x/ ","targetPrefix":""}}]`)
	client := creationTestClient(t, creationExchange{
		method: "POST", path: "/import", response: `{"success":true,"importedCount":1,"taskIds":[72]}`,
		inspect: func(rows []map[string]interface{}) {
			if rows[0]["startKey"] != " x/ a" || rows[0]["workerNode"] != "10.0.0.1" || rows[0]["keyMapping"].(map[string]interface{})["targetPrefix"] != "" {
				t.Fatal(rows)
			}
		},
	})
	if _, err := runTaskImport(client, []string{"tasks", "--file", file}); err != nil {
		t.Fatal(err)
	}
}

func TestImportLocalUsesSamePrecheckAndCreation(t *testing.T) {
	file := creationImportFile(t, "local.json", `[{"direction":"LOCAL_TO_OSS","sourceEndpoint":{"type":"LOCAL","nodeBindings":[{"executionNodeIp":"10.0.0.1","localPath":"/mnt/file"}]},"destinationEndpoint":{"type":"OSS","s3Alias":"target","bucket":"dst","objectKey":"new-name"},"distributionMode":"balanced"}]`)
	client := creationTestClient(t,
		creationExchange{method: "POST", path: "/local-transfers/validate", response: `{"success":true,"data":{"valid":true,"nodeBindings":[{"valid":true,"pathType":"FILE"}]}}`},
		creationExchange{method: "POST", path: "/local-transfers", response: `{"success":true,"data":{"acceptedCount":1,"parentTaskIds":[83]}}`, inspect: func(rows []map[string]interface{}) {
			if rows[0]["distributionMode"] != "affinity" {
				t.Fatal(rows)
			}
		}},
	)
	if _, err := runTaskImport(client, []string{"local", file}); err != nil {
		t.Fatal(err)
	}
}

func TestImportRejectsInvalidBatchBeforeNetwork(t *testing.T) {
	for _, test := range []struct{ kind, data string }{
		{"buckets", `[]`},
		{"buckets", `[null]`},
		{"buckets", `[[]]`},
		{"buckets", `[{"sourceBucket":"same","destBucket":"same"}]`},
		{"buckets", `[{"sourceBucket":"a","destBucket":"b","taskBandwidthLimitMbps":"10"}]`},
		{"buckets", `[{"sourceBucket":"a","destBucket":"b","keyMapping":{"mode":"EXACT_KEY","targetKey":"renamed"}}]`},
		{"tasks", `[{"sourceBucket":"a","destBucket":"b","sourceEndpoint":{"prefix":"backup/"}}]`},
		{"tasks", `[{"sourceBucket":"a","destBucket":"b","prefix":"x","sourceEndpoint":{"prefix":"y"}}]`},
		{"local", `[{"direction":"OSS_TO_OSS","sourceEndpoint":{},"destinationEndpoint":{}}]`},
	} {
		t.Run(test.kind+test.data, func(t *testing.T) {
			file := creationImportFile(t, "input.json", test.data)
			if _, err := runTaskImport(creationTestClient(t), []string{test.kind, file}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestImportRejectsInventoryScopeBeforeNetwork(t *testing.T) {
	for _, kind := range []string{"buckets", "tasks", "local"} {
		file := creationImportFile(t, "exported-inventory.json", `[{"sourceBucket":"a","destBucket":"b","inventoryManifestId":"sealed-manifest"}]`)
		_, err := runTaskImport(creationTestClient(t), []string{kind, file})
		if err == nil || !strings.Contains(err.Error(), "import-inventory") {
			t.Fatalf("%s import lost exact-key scope: %v", kind, err)
		}
	}
}

func TestCreatePartialRetainsAcceptedIDsAndDoesNotRetry(t *testing.T) {
	file := creationImportFile(t, "tasks.json", `[{"sourceBucket":"a","destBucket":"b"},{"sourceBucket":"c","destBucket":"d"}]`)
	client := creationTestClient(t, creationExchange{method: "POST", path: "/import", response: `{"success":true,"importedCount":1,"taskIds":[5],"partial":true,"unknownCount":1,"unconfirmedTaskIds":[6]}`})
	result, err := runTaskImport(client, []string{"tasks", file})
	if err == nil || result["submissionStatus"] != "partial" || result["taskIds"].([]interface{})[0] != float64(5) || result["unconfirmedTaskIds"].([]interface{})[0] != float64(6) {
		t.Fatalf("%#v %v", result, err)
	}
}

func TestCreateMissingAcceptanceCountIsNotSuccess(t *testing.T) {
	client := creationTestClient(t, creationNodesExchange(), creationExchange{method: "POST", path: "/add-buckets", response: `{"success":true,"creationIds":["c1"]}`})
	result, err := runTaskCreate(client, []string{"oss-to-oss", "source/src", "target/dst"})
	if err == nil || result["submissionStatus"] != "partial" {
		t.Fatalf("%#v %v", result, err)
	}
}

func TestLegacyImportKeepsSixColumnSchema(t *testing.T) {
	file := creationImportFile(t, "old.txt", "source/src,target/dst,2026-09-01T12:00:00,2026-09-02T12:00:00, preserved/prefix ,true\n")
	client := creationTestClient(t, creationExchange{
		method: "POST", path: "/add-buckets", response: `{"success":true,"addedCount":1,"creationIds":["legacy-1"]}`,
		inspect: func(rows []map[string]interface{}) {
			if rows[0]["dateBefore"] != "2026-09-02T12:00:00" || rows[0]["prefix"] != " preserved/prefix " || rows[0]["overwrite"] != true || rows[0]["distributionMode"] != "balanced" {
				t.Fatal(rows)
			}
		},
	})
	result, err := runLegacyTaskImport(client, []string{file, "--balanced"})
	if err != nil || result["creationIds"].([]interface{})[0] != "legacy-1" {
		t.Fatalf("%#v %v", result, err)
	}
}

func TestLegacyImportDoesNotSkipInvalidRows(t *testing.T) {
	file := creationImportFile(t, "old.txt", "source/src,target/dst\ninvalid\n")
	if _, err := runLegacyTaskImport(creationTestClient(t), []string{file}); err == nil {
		t.Fatal("an invalid row must block the whole submission")
	}
}

func TestCreateSubmissionErrorsRetainIDsWithoutRetry(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			file := creationImportFile(t, "tasks.json", `[{"sourceBucket":"a","destBucket":"b"}]`)
			client := creationTestClient(t, creationExchange{method: "POST", path: "/import", status: status, response: `{"success":false,"message":"dispatch unconfirmed","taskIds":[81],"unconfirmedTaskIds":[82],"partial":true,"unknownCount":1}`})
			result, err := runTaskImport(client, []string{"tasks", file})
			if err == nil || result["taskIds"].([]interface{})[0] != float64(81) || result["unconfirmedTaskIds"].([]interface{})[0] != float64(82) {
				t.Fatalf("result %#v, error %v", result, err)
			}
		})
	}
}

func TestCreatePrecheckBusinessFailureNeverSubmits(t *testing.T) {
	client := creationTestClient(t, creationExchange{method: "POST", path: "/local-transfers/validate", response: `{"success":false,"message":"node unavailable"}`})
	result, err := runTaskCreate(client, []string{"local-to-oss", "target/dst", "--binding", "10.0.0.1=/mnt/file"})
	if err == nil || result["message"] != "node unavailable" {
		t.Fatalf("result %#v, error %v", result, err)
	}
}

func TestImportExportedLocalDownloadWithDefaultName(t *testing.T) {
	for _, key := range []string{"", "one.bin"} {
		row := map[string]interface{}{
			"direction": "OSS_TO_LOCAL", "targetRelativePath": "",
			"sourceEndpoint":      map[string]interface{}{"type": "OSS", "s3Alias": "source", "bucket": "src"},
			"destinationEndpoint": map[string]interface{}{"type": "LOCAL", "nodeBindings": []interface{}{map[string]interface{}{"executionNodeIp": "10.0.0.1", "localPath": "/target"}}},
		}
		if key != "" {
			row["sourceEndpoint"].(map[string]interface{})["objectKey"] = key
		}
		encoded, _ := json.Marshal([]interface{}{row})
		file := creationImportFile(t, "exported.json", string(encoded))
		client := creationTestClient(t,
			creationExchange{method: "POST", path: "/local-transfers/validate", response: `{"success":true,"data":{"valid":true,"nodeBindings":[{"valid":true,"pathType":"DIRECTORY"}]}}`},
			creationExchange{method: "POST", path: "/local-transfers", response: `{"success":true,"data":{"acceptedCount":1,"parentTaskIds":[1]}}`},
		)
		if _, err := runTaskImport(client, []string{"local", file}); err != nil {
			t.Fatalf("default name failed for key %q: %v", key, err)
		}
		for _, invalid := range []interface{}{5, []interface{}{}, " ", "../outside", "/outside", `C:\outside`} {
			row["targetRelativePath"] = invalid
			if err := createValidateRow(row, "local"); err == nil {
				t.Fatalf("accepted invalid relative path %#v", invalid)
			}
		}
	}
}

func TestImportPreservesExportedDateTimezoneAndPrecision(t *testing.T) {
	for _, dates := range [][2]string{
		{"2026-09-01T12:00:00Z", "2026-09-01T13:00:00Z"},
		{"2026-09-01T12:00:00.123456789+08:00", "2026-09-01T05:00:00Z"},
		{"2026-09-01T12:00:00", "2026-09-01T13:00:00"},
	} {
		row := map[string]interface{}{"sourceBucket": "src", "destBucket": "dst", "dateAfter": dates[0], "dateBefore": dates[1]}
		if err := createValidateRow(row, "buckets"); err != nil {
			t.Fatal(err)
		}
		if row["dateAfter"] != dates[0] || row["dateBefore"] != dates[1] {
			t.Fatalf("date timezone/precision changed: %#v", row)
		}
	}
	row := map[string]interface{}{"sourceBucket": "src", "destBucket": "dst", "dateAfter": "2026-09-01T12:00:00+08:00", "dateBefore": "2026-09-01T03:59:59Z"}
	if err := createValidateRow(row, "buckets"); err == nil {
		t.Fatal("date range compared wall time instead of instants")
	}
}
