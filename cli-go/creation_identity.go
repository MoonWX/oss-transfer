package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func validateCreationRequestID(id string) error {
	if len(id) == 0 || len(id) > 220 {
		return fmt.Errorf("--request-id 须为 1–220 个字符")
	}
	for _, c := range id {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == ':' {
			continue
		}
		return fmt.Errorf("--request-id 仅支持字母、数字和 - _ . :")
	}
	return nil
}

func creationRequestIDOption(opts commandOptions) (string, error) {
	if len(opts.Values["request-id"]) > 1 {
		return "", fmt.Errorf("--request-id 只能填写一次")
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
		return "", fmt.Errorf("生成请求标识失败，尚未提交创建请求: %w", err)
	}
	return "cli-" + hex.EncodeToString(random[:]), nil
}

func creationSubmissionError(err error, requestID string) error {
	return fmt.Errorf("%w；请求标识 %s（Go 服务重试请保留原参数并添加 --request-id %s；旧 V4 请先核对任务列表）", err, requestID, requestID)
}
