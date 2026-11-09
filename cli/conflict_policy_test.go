package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCreateConflictChangedReachesEveryDirection(t *testing.T) {
	for _, value := range []string{"changed", "CHANGED", "overwrite-changed", "overwrite_changed"} {
		t.Run(value, func(t *testing.T) {
			client := creationTestClient(t, creationNodesExchange(), creationExchange{
				method: "POST", path: "/add-buckets", response: `{"success":true,"addedCount":1,"creationIds":["c1"]}`,
				inspect: func(rows []map[string]interface{}) {
					if rows[0]["conflictPolicy"] != "OVERWRITE_CHANGED" || rows[0]["overwrite"] != false {
						t.Fatal(rows)
					}
				},
			})
			if _, err := runTaskCreate(client, []string{"oss-to-oss", "source/src", "target/dst", "--conflict", value}); err != nil {
				t.Fatal(err)
			}
		})
	}
	// SKIP and OVERWRITE keep the existing OSS payload, without conflictPolicy.
	for _, value := range []string{"skip", "overwrite"} {
		client := creationTestClient(t, creationNodesExchange(), creationExchange{
			method: "POST", path: "/add-buckets", response: `{"success":true,"addedCount":1,"creationIds":["c1"]}`,
			inspect: func(rows []map[string]interface{}) {
				if _, ok := rows[0]["conflictPolicy"]; ok || rows[0]["overwrite"] != (value == "overwrite") {
					t.Fatal(rows)
				}
			},
		})
		if _, err := runTaskCreate(client, []string{"oss-to-oss", "source/src", "target/dst", "--conflict", value}); err != nil {
			t.Fatal(err)
		}
	}
	client := creationTestClient(t, creationExchange{
		method: "POST", path: "/local-transfers/validate", response: `{"success":true,"data":{"valid":true,"nodeBindings":[{"valid":true,"pathType":"DIRECTORY"}]}}`,
		inspect: func(rows []map[string]interface{}) {
			if rows[0]["conflictPolicy"] != "OVERWRITE_CHANGED" {
				t.Fatal(rows)
			}
		},
	})
	if _, err := runTaskCreate(client, []string{"local-to-oss", "target/dst", "--binding", "10.0.0.1=/mnt/data", "--conflict", "changed", "--validate-only"}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateConflictRejectsInvalidCombinations(t *testing.T) {
	for _, args := range [][]string{
		{"--conflict", "newer"},
		{"--conflict", ""},
		{"--conflict", "changed", "--overwrite"},
		{"--conflict", "skip", "--overwrite"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := runTaskCreate(creationTestClient(t), append([]string{"oss-to-oss", "source/src", "target/dst"}, args...))
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestImportLocalAcceptsOverwriteChanged(t *testing.T) {
	row := func(policy string) map[string]interface{} {
		return map[string]interface{}{"direction": "LOCAL_TO_OSS", "conflictPolicy": policy, "sourceEndpoint": map[string]interface{}{"type": "LOCAL", "nodeBindings": []interface{}{map[string]interface{}{"executionNodeIp": "10.0.0.1", "localPath": "/mnt/data"}}}, "destinationEndpoint": map[string]interface{}{"type": "OSS", "s3Alias": "target", "bucket": "dst"}}
	}
	for _, policy := range []string{"SKIP", "OVERWRITE", "OVERWRITE_CHANGED"} {
		if err := createValidateLocalRow(row(policy)); err != nil {
			t.Fatalf("%s rejected: %v", policy, err)
		}
	}
	if err := createValidateLocalRow(row("CHANGED")); err == nil {
		t.Fatal("unknown policy accepted")
	}
}

func TestTaskDetailTranslatesConflictPolicyAndExplainsSkips(t *testing.T) {
	labels := map[cliLocale][3]string{
		localeZH: {"跳过同名对象或文件", "覆盖同名对象或文件", "覆盖有变化的对象"},
		localeEN: {"Skip existing objects or files", "Overwrite existing objects or files", "Overwrite changed objects"},
		localeJA: {"既存のオブジェクトまたはファイルをスキップ", "既存のオブジェクトまたはファイルを上書き", "変更されたオブジェクトを上書き"},
	}
	// The note covers both causes of a skip: a name match at the destination
	// and an object deleted at the source after the scan.
	notes := map[cliLocale]string{localeZH: "未比较内容", localeEN: "content was not compared", localeJA: "内容は比較していません"}
	deleted := map[cliLocale]string{localeZH: "源端被删除的对象也计为跳过", localeEN: "deleted at the source after the scan are also counted as skipped", localeJA: "スキャン後に転送元で削除されたオブジェクトもスキップ"}
	for locale, want := range labels {
		for i, policy := range []string{"SKIP", "OVERWRITE", "OVERWRITE_CHANGED"} {
			for _, skipped := range []int{0, 3} {
				var buffer bytes.Buffer
				renderTaskDetail(&cliLocaleWriter{Writer: &buffer, locale: locale}, map[string]interface{}{"taskIndex": 7, "direction": "OSS_TO_OSS", "status": "COMPLETED", "conflictPolicy": policy, "skippedObjectCount": skipped})
				text := buffer.String()
				if !strings.Contains(text, want[i]) || strings.Contains(text, " "+policy+"\n") {
					t.Fatalf("%s %s: policy not translated: %s", locale, policy, text)
				}
				if noted := strings.Contains(text, notes[locale]) && strings.Contains(text, deleted[locale]) && strings.Contains(text, "SOURCE_DELETED"); noted != (policy == "SKIP" && skipped > 0) {
					t.Fatalf("%s %s skipped=%d: note shown=%v: %s", locale, policy, skipped, noted, text)
				}
			}
		}
	}
}

func TestTaskCreateHelpDescribesChanged(t *testing.T) {
	for _, locale := range []cliLocale{localeZH, localeEN, localeJA} {
		text := locale.text(cliTaskCreateHelp)
		if !strings.Contains(text, "skip|overwrite|changed") || !strings.Contains(text, "ETag") {
			t.Fatalf("%s help misses the changed policy: %s", locale, text)
		}
	}
}
