package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestTaskDetailShowsTheMastersAttentionSentence(t *testing.T) {
	task := map[string]interface{}{"taskIndex": 16, "status": "FAILED", "direction": "OSS_TO_LOCAL", "failedObjectCount": 18,
		"attention": map[string]interface{}{"code": "OBJECTS_FAILED", "message": "All 18 objects failed: The node has no permission to read or write the local directory"}}
	for locale, label := range map[cliLocale]string{localeZH: "需要关注", localeEN: "Attention", localeJA: "要確認"} {
		var buffer bytes.Buffer
		renderTaskDetail(&cliLocaleWriter{Writer: &buffer, locale: locale}, task)
		if !strings.Contains(buffer.String(), label) || !strings.Contains(buffer.String(), "All 18 objects failed") {
			t.Fatalf("%s:\n%s", locale, buffer.String())
		}
	}
}

func TestExceptionReasonCodesAreExplained(t *testing.T) {
	item := map[string]interface{}{"key": "medium/m8.bin", "reasonCode": "SOURCE_CHANGED", "retryReasonCode": "SOURCE_DELETED", "errorCode": "TRANSFER_FAILED"}
	var buffer bytes.Buffer
	renderFields(&cliLocaleWriter{Writer: &buffer, locale: localeEN}, item, "")
	output := buffer.String()
	for _, want := range []string{"Reason code: The source object changed after the scan; a retry copies its current version (SOURCE_CHANGED)", "Retry reason: The source object was deleted at the source and was skipped (SOURCE_DELETED)"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in:\n%s", want, output)
		}
	}
	// Unknown codes stay as they are.
	buffer.Reset()
	renderFields(&cliLocaleWriter{Writer: &buffer, locale: localeEN}, map[string]interface{}{"reasonCode": "TRANSFER_FAILED"}, "")
	if !strings.Contains(buffer.String(), "Reason code: TRANSFER_FAILED") {
		t.Fatal(buffer.String())
	}
}
