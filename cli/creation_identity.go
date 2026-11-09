package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
)

func validateCreationRequestID(id string) error {
	if len(id) == 0 || len(id) > 220 {
		return cliErrorf("--request-id 须为 1–220 个字符")
	}
	for _, c := range id {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == ':' {
			continue
		}
		return cliErrorf("--request-id 仅支持字母、数字和 - _ . :")
	}
	return nil
}

func creationRequestIDOption(opts commandOptions) (string, error) {
	if len(opts.Values["request-id"]) > 1 {
		return "", cliErrorf("--request-id 只能填写一次")
	}
	id := opts.Value("request-id")
	if opts.Has("request-id") {
		return id, validateCreationRequestID(id)
	}
	return "", nil
}

// Allocate only after local validation, immediately before the mutation. Each
// invocation gets its own identity; equal payloads may be intentional new tasks.
func newCreationRequestID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", cliErrorf("生成请求标识失败，尚未提交创建请求: %w", err)
	}
	return "cli-" + hex.EncodeToString(random[:]), nil
}

// creationSubmissionError explains a creation whose outcome is unknown: the
// same request ID lets a retry return the saved result instead of creating
// the tasks again. A definite rejection needs no such advice.
func creationSubmissionError(err error, requestID string) error {
	var problem *apiHTTPError
	if errors.As(err, &problem) && problem.Status >= 400 && problem.Status < 500 && problem.Status != http.StatusRequestTimeout {
		return err
	}
	return cliErrorf("%w；请求标识 %s（重试时请保留原参数并添加 --request-id %s，以免重复创建；先用 task list 核对是否已创建）", err, requestID, requestID)
}
