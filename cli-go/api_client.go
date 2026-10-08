package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ApiClient struct {
	requestContext context.Context
	baseUrl        string
	client         *http.Client
	verbose        bool
	internalToken  string
	authToken      string
}

func NewApiClient(host string) *ApiClient {
	client := &ApiClient{
		baseUrl:       strings.TrimRight(host, "/") + "/api/master",
		client:        &http.Client{Timeout: 60 * time.Second},
		internalToken: loadInternalToken(),
	}
	client.SetAuthToken(strings.TrimSpace(os.Getenv("OSS_TRANSFER_AUTH_TOKEN")))
	return client
}

// loadInternalToken uses an explicit environment token or node configuration.
// An explicit config file never falls back to another node's credentials.
func loadInternalToken() string {
	if t := os.Getenv("OSS_TRANSFER_INTERNAL_TOKEN"); t != "" {
		return t
	}
	if configured := strings.TrimSpace(os.Getenv("OSS_TRANSFER_CONFIG_FILE")); configured != "" {
		return readTokenFromYml(configured)
	}
	for _, p := range []string{"/etc/oss-transfer/application.yml", "application.yml", "/export/oss-transfer-tool/application.yml"} {
		t, agent := readDeploymentToken(p)
		if agent {
			return ""
		}
		if t != "" {
			return t
		}
	}
	return ""
}

func readTokenFromYml(path string) string {
	token, _ := readDeploymentToken(path)
	return token
}

func readDeploymentToken(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	token, agent := "", false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "transfer.role:") {
			value := strings.SplitN(strings.TrimPrefix(line, "transfer.role:"), "#", 2)[0]
			role := strings.Trim(strings.TrimSpace(value), "\"'")
			agent = strings.EqualFold(role, "agent")
		}
		if strings.HasPrefix(line, "transfer.internal-token:") {
			token = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "transfer.internal-token:")), "\"'")
		}
	}
	if agent {
		return "", true
	}
	return token, false
}

func (c *ApiClient) SetVerbose(verbose bool) { c.verbose = verbose }

// A selected user session must not silently fall back to node credentials.
func (c *ApiClient) SetAuthToken(token string) {
	c.authToken = strings.TrimSpace(token)
	if c.authToken != "" {
		c.internalToken = ""
	}
}

func (c *ApiClient) Get(path string) (map[string]interface{}, error) {
	return c.Request(http.MethodGet, path, nil)
}

func (c *ApiClient) Post(path string, body interface{}) (map[string]interface{}, error) {
	return c.Request(http.MethodPost, path, body)
}

func (c *ApiClient) Put(path string, body interface{}) (map[string]interface{}, error) {
	return c.Request(http.MethodPut, path, body)
}

func (c *ApiClient) Delete(path string) (map[string]interface{}, error) {
	return c.Request(http.MethodDelete, path, nil)
}

func (c *ApiClient) PostWorker(path string, body interface{}) (map[string]interface{}, error) {
	return c.Request(http.MethodPost, "/api/worker/"+strings.TrimLeft(path, "/"), body)
}

// Request accepts an absolute /api/... path or a path relative to /api/master.
// A decoded error response is retained so callers can show partial results and IDs.
func (c *ApiClient) Request(method, path string, body interface{}) (map[string]interface{}, error) {
	req, err := c.newRequest(method, path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	return decodeAPIResponse(resp.StatusCode, responseBody)
}

// Retain the original internal entry point for existing callers.
func (c *ApiClient) request(method, path string, body interface{}) (map[string]interface{}, error) {
	return c.Request(method, path, body)
}

func (c *ApiClient) newRequest(method, path string, body interface{}) (*http.Request, error) {
	base, err := url.Parse(c.baseUrl)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil {
		return nil, fmt.Errorf("服务地址无效，请使用 http://主机:端口 或 https://主机:端口")
	}
	relative, err := url.Parse(path)
	if err != nil || relative.IsAbs() || relative.Host != "" || relative.Fragment != "" {
		return nil, fmt.Errorf("API 路径无效")
	}
	if strings.HasPrefix(path, "/api/") {
		base.Path, base.RawPath = relative.Path, relative.RawPath
	} else {
		base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(relative.Path, "/")
		if relative.RawPath != "" {
			// Preserve escaped slashes in identifiers instead of decoding them into route separators.
			prefix, _ := url.Parse(c.baseUrl)
			base.RawPath = strings.TrimRight(prefix.EscapedPath(), "/") + "/" + strings.TrimLeft(relative.EscapedPath(), "/")
		} else {
			base.RawPath = ""
		}
	}
	base.RawQuery = relative.RawQuery
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("请求参数无法编码: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	ctx := c.requestContext
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, base.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.internalToken != "" {
		req.Header.Set("X-Internal-Token", c.internalToken)
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	return req, nil
}

func (c *ApiClient) do(req *http.Request, download bool) (*http.Response, error) {
	client := http.Client{Timeout: 60 * time.Second}
	if c.client != nil {
		client = *c.client
	}
	if download {
		client.Timeout = 30 * time.Minute
	}
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) > 0 && (!strings.EqualFold(next.URL.Scheme, via[0].URL.Scheme) || !strings.EqualFold(next.URL.Host, via[0].URL.Host)) {
			return fmt.Errorf("拒绝跨源重定向，避免泄露认证信息")
		}
		if previousRedirect != nil {
			return previousRedirect(next, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("重定向次数过多")
		}
		return nil
	}
	if c.verbose {
		fmt.Fprintf(os.Stderr, "[VERBOSE] %s %s\n", req.Method, req.URL.EscapedPath())
	}
	resp, err := client.Do(req)
	if c.verbose && resp != nil {
		fmt.Fprintf(os.Stderr, "[VERBOSE] %s %s HTTP %d\n", req.Method, req.URL.EscapedPath(), resp.StatusCode)
	}
	if err != nil {
		// url.Error includes the complete URL; omit it because query values may be sensitive.
		var requestError *url.Error
		if errors.As(err, &requestError) {
			err = requestError.Err
		}
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	return resp, nil
}

func decodeAPIResponse(status int, body []byte) (map[string]interface{}, error) {
	var result map[string]interface{}
	var decodeErr error
	if len(bytes.TrimSpace(body)) > 0 {
		decodeErr = json.Unmarshal(body, &result)
	}
	if status < 200 || status >= 300 {
		if message := apiResponseMessage(result); message != "" {
			return result, fmt.Errorf("HTTP %d: %s", status, message)
		}
		return result, fmt.Errorf("HTTP %d: %s", status, http.StatusText(status))
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("响应不是有效的 JSON 对象: %w", decodeErr)
	}
	if result == nil && status != http.StatusNoContent {
		return nil, fmt.Errorf("服务器返回空响应，未确认操作结果")
	}
	if success, exists := result["success"].(bool); exists && !success {
		if message := apiResponseMessage(result); message != "" {
			return result, fmt.Errorf("%s", message)
		}
		return result, fmt.Errorf("服务器未能完成操作")
	}
	return result, nil
}

func apiResponseMessage(result map[string]interface{}) string {
	for _, field := range []string{"message", "error", "code"} {
		if message, ok := result[field].(string); ok && strings.TrimSpace(message) != "" {
			return message
		}
	}
	return ""
}

// Download streams a ZIP report to a temporary file beside the destination.
// The verified complete archive is published atomically without replacing any file.
func (c *ApiClient) Download(method, path string, body interface{}, destination string) error {
	if strings.TrimSpace(destination) == "" {
		return fmt.Errorf("请指定报告保存路径")
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("目标文件已存在，请选择其他保存路径: %s", destination)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查目标文件失败: %w", err)
	}
	req, err := c.newRequest(method, path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/zip")
	resp, err := c.do(req, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || (mediaType != "application/zip" && mediaType != "application/octet-stream" && mediaType != "application/x-zip-compressed" && mediaType != "") {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if readErr != nil {
			return fmt.Errorf("读取下载错误信息失败: %w", readErr)
		}
		if _, responseErr := decodeAPIResponse(resp.StatusCode, data); responseErr != nil {
			return responseErr
		}
		return fmt.Errorf("服务器未返回 ZIP 报告（Content-Type: %s）", mediaType)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".part-*")
	if err != nil {
		return fmt.Errorf("创建报告临时文件失败: %w", err)
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	written, err := io.Copy(temporary, resp.Body)
	if err != nil {
		return fmt.Errorf("报告下载中断: %w", err)
	}
	if resp.ContentLength >= 0 && written != resp.ContentLength {
		return fmt.Errorf("报告下载不完整：预期 %d 字节，实际 %d 字节", resp.ContentLength, written)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("保存报告失败: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("保存报告失败: %w", err)
	}
	if err := validateReportArchive(temporary.Name()); err != nil {
		return err
	}
	// A same-directory hard link gives atomic no-replace publication; Rename can
	// silently replace a target created between the initial check and publication.
	if err := os.Link(temporary.Name(), destination); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("目标文件已存在，请选择其他保存路径: %s", destination)
		}
		return fmt.Errorf("发布报告文件失败: %w", err)
	}
	return nil
}

func validateReportArchive(path string) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("下载内容不是完整的 ZIP 报告: %w", err)
	}
	defer archive.Close()
	for _, entry := range archive.File {
		reader, err := entry.Open()
		if err != nil {
			return fmt.Errorf("ZIP 报告校验失败: %w", err)
		}
		_, copyErr := io.Copy(io.Discard, reader)
		closeErr := reader.Close()
		if copyErr != nil {
			return fmt.Errorf("ZIP 报告校验失败: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("ZIP 报告校验失败: %w", closeErr)
		}
	}
	return nil
}

func (c *ApiClient) PrintJson(result map[string]interface{}) error {
	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(output))
	return nil
}

func (c *ApiClient) PrintJsonWithFilter(result map[string]interface{}, filterKey string) error {
	if data, ok := result[filterKey]; ok {
		result = map[string]interface{}{filterKey: data}
	}
	return c.PrintJson(result)
}

func parseBucketPair(input string) (map[string]interface{}, error) {
	parts := strings.Split(input, ",")
	if len(parts) < 2 {
		return nil, fmt.Errorf("格式错误，至少需要: 源S3别名/源bucket,目标S3别名/目标bucket")
	}

	srcParts := strings.SplitN(parts[0], "/", 2)
	if len(srcParts) != 2 {
		return nil, fmt.Errorf("源格式错误，应为: 别名/bucket名")
	}

	destParts := strings.SplitN(parts[1], "/", 2)
	if len(destParts) != 2 {
		return nil, fmt.Errorf("目标格式错误，应为: 别名/bucket名")
	}

	result := map[string]interface{}{
		"sourceS3Alias": srcParts[0],
		"sourceBucket":  srcParts[1],
		"destS3Alias":   destParts[0],
		"destBucket":    destParts[1],
	}

	if len(parts) >= 3 && parts[2] != "" {
		result["dateAfter"] = parts[2]
	}

	if len(parts) >= 4 && parts[3] != "" {
		result["dateBefore"] = parts[3]
	}

	if len(parts) >= 5 && parts[4] != "" {
		result["prefix"] = parts[4]
	}

	if len(parts) >= 6 && parts[5] != "" {
		result["overwrite"] = parts[5] == "true"
	}

	return result, nil
}
