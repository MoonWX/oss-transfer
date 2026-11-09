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
	"sync/atomic"
	"time"
)

type ApiClient struct {
	locale             cliLocale
	output             io.Writer
	errors             io.Writer
	passwordPrompt     func(io.Reader, string) (string, error)
	requestContext     context.Context
	baseUrl            string
	client             *http.Client
	verbose            bool
	configurationError error
	authToken          string
}

func NewApiClient(host string) *ApiClient {
	client := &ApiClient{
		baseUrl: strings.TrimRight(host, "/") + "/api/master",
		client:  &http.Client{Timeout: 60 * time.Second},
	}
	if strings.TrimSpace(os.Getenv("OSS_TRANSFER_INTERNAL_TOKEN")) != "" {
		client.configurationError = cliErrorf("OSS_TRANSFER_INTERNAL_TOKEN 已停用；请取消该环境变量后运行 login，CLI 使用用户登录会话")
	}
	client.SetAuthToken(strings.TrimSpace(os.Getenv("OSS_TRANSFER_AUTH_TOKEN")))
	return client
}

func (c *ApiClient) SetVerbose(verbose bool) { c.verbose = verbose }

// Tokens come from an explicit user session or the CLI login store.
func (c *ApiClient) SetAuthToken(token string) {
	c.authToken = strings.TrimSpace(token)
}

func (c *ApiClient) Get(path string) (map[string]interface{}, error) {
	return c.Request(http.MethodGet, path, nil)
}

func (c *ApiClient) Post(path string, body interface{}) (map[string]interface{}, error) {
	return c.Request(http.MethodPost, path, body)
}

// PostCreation sends one explicit creation attempt. After an uncertain response,
// the caller can retry with the same request ID to retrieve its saved result.
func (c *ApiClient) PostCreation(path string, body interface{}, requestID string) (map[string]interface{}, error) {
	if err := validateCreationRequestID(requestID); err != nil {
		return nil, err
	}
	req, err := c.newRequest(http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Idempotency-Key", requestID)
	req.GetBody = nil
	return c.sendRequest(req)
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
	return c.sendRequest(req)
}

func (c *ApiClient) sendRequest(req *http.Request) (map[string]interface{}, error) {
	resp, err := c.do(req, -1)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, cliErrorf("读取响应失败: %w", err)
	}
	return decodeAPIResponse(resp.StatusCode, responseBody)
}

// Retain the original internal entry point for existing callers.
func (c *ApiClient) request(method, path string, body interface{}) (map[string]interface{}, error) {
	return c.Request(method, path, body)
}

func (c *ApiClient) newRequest(method, path string, body interface{}) (*http.Request, error) {
	if c.configurationError != nil {
		return nil, c.configurationError
	}
	base, err := url.Parse(c.baseUrl)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil {
		return nil, cliErrorf("服务地址无效，请使用 http://主机:端口 或 https://主机:端口")
	}
	relative, err := url.Parse(path)
	if err != nil || relative.IsAbs() || relative.Host != "" || relative.Fragment != "" {
		return nil, cliErrorf("API 路径无效")
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
			return nil, cliErrorf("请求参数无法编码: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	ctx := c.requestContext
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, base.String(), reader)
	if err != nil {
		return nil, cliErrorf("创建请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.locale != "" {
		req.Header.Set("Accept-Language", string(c.locale))
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	return req, nil
}

// do sends req. A negative timeout keeps the client's default; zero removes the
// total deadline, and the caller then bounds the request itself.
func (c *ApiClient) do(req *http.Request, timeout time.Duration) (*http.Response, error) {
	client := http.Client{Timeout: 60 * time.Second}
	if c.client != nil {
		client = *c.client
	}
	if timeout >= 0 {
		client.Timeout = timeout
	}
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) > 0 && (!strings.EqualFold(next.URL.Scheme, via[0].URL.Scheme) || !strings.EqualFold(next.URL.Host, via[0].URL.Host)) {
			return cliErrorf("拒绝跨源重定向，避免泄露认证信息")
		}
		if previousRedirect != nil {
			return previousRedirect(next, via)
		}
		if len(via) >= 10 {
			return cliErrorf("重定向次数过多")
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
		return nil, cliErrorf("请求失败: %w", err)
	}
	return resp, nil
}

type apiHTTPError struct {
	Status  int
	Message string
}

func (e *apiHTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

func decodeAPIResponse(status int, body []byte) (map[string]interface{}, error) {
	var result map[string]interface{}
	var decodeErr error
	if len(bytes.TrimSpace(body)) > 0 {
		decodeErr = json.Unmarshal(body, &result)
	}
	if status < 200 || status >= 300 {
		if message := apiResponseMessage(result); message != "" {
			return result, &apiHTTPError{status, message}
		}
		return result, &apiHTTPError{status, http.StatusText(status)}
	}
	if decodeErr != nil {
		return nil, cliErrorf("响应不是有效的 JSON 对象: %w", decodeErr)
	}
	if result == nil && status != http.StatusNoContent {
		return nil, cliErrorf("服务器返回空响应，未确认操作结果")
	}
	if success, exists := result["success"].(bool); exists && !success {
		if message := apiResponseMessage(result); message != "" {
			return result, fmt.Errorf("%s", message)
		}
		return result, cliErrorf("服务器未能完成操作")
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

// downloadSpec describes one kind of downloaded file.
type downloadSpec struct {
	accept string
	// mediaTypes lists the accepted response types; "" accepts a response
	// without Content-Type.
	mediaTypes []string
	// verify checks the complete temporary file before it is published.
	verify func(path string) error
	// timeout bounds the whole download. When idle is set instead, the
	// download has no total deadline and stops after idle without data.
	timeout, idle time.Duration
	text          downloadText
}

// downloadText holds the catalog messages that name the downloaded file.
type downloadText struct {
	wrongType, temporary, interrupted, short, save, publish, idle string
}

var reportDownload = downloadSpec{
	accept:     "application/zip",
	mediaTypes: []string{"application/zip", "application/octet-stream", "application/x-zip-compressed", ""},
	verify:     validateReportArchive,
	timeout:    30 * time.Minute,
	text: downloadText{
		wrongType:   "服务器未返回 ZIP 报告（Content-Type: %s）",
		temporary:   "创建报告临时文件失败: %w",
		interrupted: "报告下载中断: %w",
		short:       "报告下载不完整：预期 %d 字节，实际 %d 字节",
		save:        "保存报告失败: %w",
		publish:     "发布报告文件失败: %w",
	},
}

// Download streams a ZIP report to a temporary file beside the destination.
// The verified complete archive is published atomically without replacing any file.
func (c *ApiClient) Download(method, path string, body interface{}, destination string) error {
	if strings.TrimSpace(destination) == "" {
		return cliErrorf("请指定报告保存路径")
	}
	return c.download(method, path, body, destination, reportDownload)
}

// download streams a response to a temporary file beside the destination,
// verifies the complete file and publishes it without replacing any file.
func (c *ApiClient) download(method, path string, body interface{}, destination string, spec downloadSpec) error {
	if _, err := os.Lstat(destination); err == nil {
		return cliErrorf("目标文件已存在，请选择其他保存路径: %s", destination)
	} else if !os.IsNotExist(err) {
		return cliErrorf("检查目标文件失败: %w", err)
	}
	req, err := c.newRequest(method, path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", spec.accept)
	var watchdog *idleWatchdog
	if spec.idle > 0 {
		ctx, cancel := context.WithCancel(req.Context())
		defer cancel()
		watchdog = startIdleWatchdog(spec.idle, cancel)
		defer watchdog.stop()
		req = req.WithContext(ctx)
	}
	resp, err := c.do(req, spec.timeout)
	if err != nil {
		if watchdog.expired() {
			return cliErrorf(spec.text.idle, spec.idle)
		}
		return err
	}
	defer resp.Body.Close()
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	accepted := false
	for _, allowed := range spec.mediaTypes {
		accepted = accepted || mediaType == allowed
	}
	if failed := resp.StatusCode < 200 || resp.StatusCode >= 300; failed || !accepted {
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if readErr != nil {
			return cliErrorf("读取下载错误信息失败: %w", readErr)
		}
		// API errors are JSON. A successful page of another type, such as a
		// login page, is simply not the requested file.
		if failed || mediaType == "application/json" {
			if _, responseErr := decodeAPIResponse(resp.StatusCode, data); responseErr != nil {
				return responseErr
			}
		}
		return cliErrorf(spec.text.wrongType, mediaType)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".part-*")
	if err != nil {
		return cliErrorf(spec.text.temporary, err)
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	var source io.Reader = resp.Body
	if watchdog != nil {
		source = watchdog.reader(resp.Body)
	}
	written, err := io.Copy(temporary, source)
	if err != nil {
		if watchdog.expired() {
			return cliErrorf(spec.text.idle, spec.idle)
		}
		return cliErrorf(spec.text.interrupted, err)
	}
	watchdog.stop()
	if resp.ContentLength >= 0 && written != resp.ContentLength {
		return cliErrorf(spec.text.short, resp.ContentLength, written)
	}
	if err := temporary.Sync(); err != nil {
		return cliErrorf(spec.text.save, err)
	}
	if err := temporary.Close(); err != nil {
		return cliErrorf(spec.text.save, err)
	}
	if err := spec.verify(temporary.Name()); err != nil {
		return err
	}
	// A same-directory hard link gives atomic no-replace publication; Rename can
	// silently replace a target created between the initial check and publication.
	if err := os.Link(temporary.Name(), destination); err != nil {
		if os.IsExist(err) {
			return cliErrorf("目标文件已存在，请选择其他保存路径: %s", destination)
		}
		return cliErrorf(spec.text.publish, err)
	}
	return nil
}

// idleWatchdog cancels a download that receives no data for its period.
type idleWatchdog struct {
	period time.Duration
	timer  *time.Timer
	fired  atomic.Bool
}

func startIdleWatchdog(period time.Duration, cancel context.CancelFunc) *idleWatchdog {
	watchdog := &idleWatchdog{period: period}
	watchdog.timer = time.AfterFunc(period, func() {
		watchdog.fired.Store(true)
		cancel()
	})
	return watchdog
}

// expired reports whether the watchdog canceled the download; a nil watchdog
// never does.
func (w *idleWatchdog) expired() bool { return w != nil && w.fired.Load() }

func (w *idleWatchdog) stop() {
	if w != nil {
		w.timer.Stop()
	}
}

// reader restarts the period whenever data arrives.
func (w *idleWatchdog) reader(body io.Reader) io.Reader {
	return idleWatchdogReader{body, w}
}

type idleWatchdogReader struct {
	body     io.Reader
	watchdog *idleWatchdog
}

func (r idleWatchdogReader) Read(buffer []byte) (int, error) {
	n, err := r.body.Read(buffer)
	if n > 0 && !r.watchdog.expired() {
		r.watchdog.timer.Reset(r.watchdog.period)
	}
	return n, err
}

func validateReportArchive(path string) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return cliErrorf("下载内容不是完整的 ZIP 报告: %w", err)
	}
	defer archive.Close()
	for _, entry := range archive.File {
		reader, err := entry.Open()
		if err != nil {
			return cliErrorf("ZIP 报告校验失败: %w", err)
		}
		_, copyErr := io.Copy(io.Discard, reader)
		closeErr := reader.Close()
		if copyErr != nil {
			return cliErrorf("ZIP 报告校验失败: %w", copyErr)
		}
		if closeErr != nil {
			return cliErrorf("ZIP 报告校验失败: %w", closeErr)
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
		return nil, cliErrorf("格式错误，至少需要: 源S3别名/源bucket,目标S3别名/目标bucket")
	}

	srcParts := strings.SplitN(parts[0], "/", 2)
	if len(srcParts) != 2 {
		return nil, cliErrorf("源格式错误，应为: 别名/bucket名")
	}

	destParts := strings.SplitN(parts[1], "/", 2)
	if len(destParts) != 2 {
		return nil, cliErrorf("目标格式错误，应为: 别名/bucket名")
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
