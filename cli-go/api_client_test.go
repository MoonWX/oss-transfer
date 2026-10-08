package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type apiTestTransport func(*http.Request) (*http.Response, error)

func (transport apiTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
func apiTestClient(transport apiTestTransport) *ApiClient {
	return &ApiClient{baseUrl: "http://example.test/api/master", client: &http.Client{Transport: transport, Timeout: time.Minute}}
}
func apiTestResponse(status int, contentType string, body io.Reader) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(body), ContentLength: -1}
}
func apiTestZIP(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.CreateHeader(&zip.FileHeader{Name: "file-results.csv", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("taskId,status\n224,COMPLETED\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestAPIErrorResultsRetainDetails(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body, want string
	}{
		{"unauthorized", 401, `{"success":false,"message":"会话已过期"}`, "401"},
		{"server error", 500, `{"success":false,"message":"节点故障","data":{"creationId":224}}`, "节点故障"},
		{"business error", 200, `{"success":false,"message":"部分节点创建失败","data":{"creationId":225}}`, "部分节点创建失败"},
		{"plain error", 502, "<html>Bad gateway</html>", "502"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := apiTestClient(func(*http.Request) (*http.Response, error) {
				return apiTestResponse(tc.status, "application/json", strings.NewReader(tc.body)), nil
			})
			result, err := client.Get("/status")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("result=%v error=%v; want %q", result, err, tc.want)
			}
			if strings.HasPrefix(tc.body, "{") && result["success"] != false {
				t.Fatalf("discarded API error result: %v", result)
			}
			if strings.Contains(tc.body, "creationId") && result["data"] == nil {
				t.Fatalf("discarded partial creation details: %v", result)
			}
		})
	}
}
func TestAPIInvalidJSONAndEmptyResponse(t *testing.T) {
	for _, body := range []string{`{"success":true`, `<html>Login</html>`, `[]`, `null`, ""} {
		t.Run(body, func(t *testing.T) {
			client := apiTestClient(func(*http.Request) (*http.Response, error) {
				return apiTestResponse(200, "application/json", strings.NewReader(body)), nil
			})
			if _, err := client.Get("/status"); err == nil {
				t.Fatalf("invalid response accepted: %q", body)
			}
		})
	}
	client := apiTestClient(func(*http.Request) (*http.Response, error) {
		return apiTestResponse(204, "", strings.NewReader("")), nil
	})
	if _, err := client.Delete("/example"); err != nil {
		t.Fatalf("204 should succeed: %v", err)
	}
}
func TestAPIIncompleteNodesRemainVisible(t *testing.T) {
	client := apiTestClient(func(*http.Request) (*http.Response, error) {
		return apiTestResponse(200, "application/json", strings.NewReader(`{"success":true,"data":{"complete":false,"unreachableNodes":["node-2"]}}`)), nil
	})
	result, err := client.Get("/deleted-tasks")
	if err != nil || result["data"].(map[string]interface{})["complete"] != false {
		t.Fatalf("incomplete node information must remain available: %v, %v", result, err)
	}
}
func TestAPIPathsAndMethods(t *testing.T) {
	client := apiTestClient(func(request *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(request.Body)
		if request.Method == "PUT" && string(data) != `{"enabled":true}` {
			t.Errorf("PUT body=%q", data)
		}
		return apiTestResponse(200, "application/json", strings.NewReader(`{"success":true}`)), nil
	})
	cases := []struct{ path, expected string }{
		{"/status?limit=10", "http://example.test/api/master/status?limit=10"},
		{"status", "http://example.test/api/master/status"},
		{"/api/nodes", "http://example.test/api/nodes"},
		{"/s3-config/a%2Fb", "http://example.test/api/master/s3-config/a%2Fb"},
	}
	for _, tc := range cases {
		request, err := client.newRequest("GET", tc.path, nil)
		if err != nil || request.URL.String() != tc.expected {
			t.Fatalf("path %q: request=%v error=%v", tc.path, request, err)
		}
	}
	if _, err := client.Put("/enabled", map[string]interface{}{"enabled": true}); err != nil {
		t.Fatal(err)
	}
	var workerPath string
	client.client.Transport = apiTestTransport(func(request *http.Request) (*http.Response, error) {
		workerPath = request.URL.Path
		return apiTestResponse(200, "application/json", strings.NewReader(`{"success":true}`)), nil
	})
	if _, err := client.PostWorker("/import-inventory", nil); err != nil || workerPath != "/api/worker/import-inventory" {
		t.Fatalf("workerPath=%q error=%v", workerPath, err)
	}
	for _, path := range []string{"https://untrusted.test/api/x", "//untrusted.test/api/x", "/status#fragment"} {
		if _, err := client.Get(path); err == nil {
			t.Fatalf("accepted external or ambiguous API path %q", path)
		}
	}
}
func TestAPIAuthSelectionAndTrailingSlash(t *testing.T) {
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "internal-secret")
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", " user-secret ")
	client := NewApiClient("http://example.test///")
	request, err := client.newRequest("GET", "/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if request.URL.String() != "http://example.test/api/master/status" || request.Header.Get("Authorization") != "Bearer user-secret" || request.Header.Get("X-Internal-Token") != "" {
		t.Fatal("user session and trailing slash were not normalized")
	}
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "")
	client = NewApiClient("http://example.test")
	request, _ = client.newRequest("GET", "/status", nil)
	if request.Header.Get("X-Internal-Token") != "internal-secret" {
		t.Fatal("existing internal token loading was lost")
	}
	client.SetAuthToken("saved-session")
	request, _ = client.newRequest("GET", "/status", nil)
	if request.Header.Get("Authorization") != "Bearer saved-session" || request.Header.Get("X-Internal-Token") != "" {
		t.Fatal("session authentication must replace internal authentication")
	}
}
func TestAPIVerboseDoesNotPrintCredentials(t *testing.T) {
	client := apiTestClient(func(*http.Request) (*http.Response, error) {
		return apiTestResponse(200, "application/json", strings.NewReader(`{"success":true,"data":{"secretKey":"response-secret"}}`)), nil
	})
	client.internalToken, client.authToken = "internal-secret", "auth-secret"
	client.SetVerbose(true)
	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	defer func() { os.Stderr = original }()
	_, requestErr := client.Post("/s3-config?ticket=query-secret", map[string]interface{}{"secretKey": "request-secret"})
	writer.Close()
	output, _ := io.ReadAll(reader)
	reader.Close()
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	if !bytes.Contains(output, []byte("POST /api/master/s3-config HTTP 200")) {
		t.Fatalf("missing request status: %q", output)
	}
	for _, secret := range []string{"query-secret", "request-secret", "response-secret", "internal-secret", "auth-secret"} {
		if bytes.Contains(output, []byte(secret)) {
			t.Fatalf("verbose leaked %q", secret)
		}
	}
}
func TestAPICrossOriginRedirectIsBlocked(t *testing.T) {
	externalCalls := 0
	client := apiTestClient(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "example.test" {
			externalCalls++
			return apiTestResponse(200, "application/json", strings.NewReader(`{"success":true}`)), nil
		}
		response := apiTestResponse(http.StatusTemporaryRedirect, "", strings.NewReader(""))
		response.Header.Set("Location", "http://external.test/capture")
		return response, nil
	})
	client.internalToken, client.authToken = "internal-secret", "auth-secret"
	if _, err := client.Get("/status"); err == nil || !strings.Contains(err.Error(), "跨源") {
		t.Fatalf("cross-origin redirect not rejected: %v", err)
	}
	if err := client.Download("GET", "/report", nil, filepath.Join(t.TempDir(), "report.zip")); err == nil {
		t.Fatal("download followed cross-origin redirect")
	}
	if externalCalls != 0 {
		t.Fatal("redirect destination received an authenticated request")
	}
}

func TestAPISameOriginRedirectRetainsAuthentication(t *testing.T) {
	client := apiTestClient(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/master/status" {
			response := apiTestResponse(http.StatusTemporaryRedirect, "", strings.NewReader(""))
			response.Header.Set("Location", "/api/master/ready")
			return response, nil
		}
		if request.Header.Get("X-Internal-Token") != "node-key" {
			t.Error("same-origin redirect lost authentication")
		}
		return apiTestResponse(200, "application/json", strings.NewReader(`{"success":true}`)), nil
	})
	client.internalToken = "node-key"
	if _, err := client.Get("/status"); err != nil {
		t.Fatal(err)
	}
}

func TestAPIDownloadCompleteAndNoOverwrite(t *testing.T) {
	archive := apiTestZIP(t)
	var calls int
	client := apiTestClient(func(request *http.Request) (*http.Response, error) {
		calls++
		deadline, ok := request.Context().Deadline()
		if !ok || time.Until(deadline) < 29*time.Minute {
			t.Error("report download still uses short API timeout")
		}
		if request.Header.Get("X-Internal-Token") != "node-key" || request.Header.Get("Accept") != "application/zip" {
			t.Error("report download lost authentication or content negotiation")
		}
		return apiTestResponse(200, "application/zip", bytes.NewReader(archive)), nil
	})
	client.internalToken = "node-key"
	destination := filepath.Join(t.TempDir(), "report.zip")
	if err := client.Download("GET", "/task-reports/224", nil, destination); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(actual, archive) {
		t.Fatalf("download mismatch: %v", err)
	}
	if err := client.Download("GET", "/task-reports/224", nil, destination); err == nil || calls != 1 {
		t.Fatal("existing report must be refused before second request")
	}
	entries, _ := os.ReadDir(filepath.Dir(destination))
	if len(entries) != 1 {
		t.Fatal("temporary report file remained")
	}
}

type apiInterruptedReader struct{ sent bool }

func (reader *apiInterruptedReader) Read(buffer []byte) (int, error) {
	if !reader.sent {
		reader.sent = true
		return copy(buffer, "PK"), nil
	}
	return 0, errors.New("connection interrupted")
}
func TestAPIDownloadFailureLeavesNoFile(t *testing.T) {
	archive := apiTestZIP(t)
	corrupt := append([]byte(nil), archive...)
	corrupt[bytes.Index(corrupt, []byte("COMPLETED"))] = 'X'
	cases := []struct {
		name   string
		status int
		mime   string
		body   func() io.Reader
		length int64
	}{
		{"unauthorized", 401, "application/json", func() io.Reader { return strings.NewReader(`{"success":false,"message":"unauthorized"}`) }, -1},
		{"server error", 500, "text/html", func() io.Reader { return strings.NewReader("failure") }, -1},
		{"json success is not report", 200, "application/json", func() io.Reader { return strings.NewReader(`{"success":true}`) }, -1},
		{"html login page", 200, "text/html", func() io.Reader { return strings.NewReader("<html>Login</html>") }, -1},
		{"wrong content", 200, "application/zip", func() io.Reader { return strings.NewReader(`{"success":false}`) }, -1},
		{"interrupted stream", 200, "application/zip", func() io.Reader { return &apiInterruptedReader{} }, -1},
		{"truncated zip", 200, "application/zip", func() io.Reader { return bytes.NewReader(archive[:len(archive)/2]) }, -1},
		{"length mismatch", 200, "application/zip", func() io.Reader { return bytes.NewReader(archive) }, int64(len(archive) + 1)},
		{"checksum mismatch", 200, "application/zip", func() io.Reader { return bytes.NewReader(corrupt) }, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := apiTestClient(func(*http.Request) (*http.Response, error) {
				response := apiTestResponse(tc.status, tc.mime, tc.body())
				response.ContentLength = tc.length
				return response, nil
			})
			directory := t.TempDir()
			if err := client.Download("GET", "/task-reports/224", nil, filepath.Join(directory, "report.zip")); err == nil {
				t.Fatal("invalid download accepted")
			}
			entries, _ := os.ReadDir(directory)
			if len(entries) != 0 {
				t.Fatalf("failed download left partial files: %v", entries)
			}
		})
	}
}
func TestAPIDownloadDoesNotOverwriteConcurrentDestination(t *testing.T) {
	archive := apiTestZIP(t)
	destination := filepath.Join(t.TempDir(), "report.zip")
	client := apiTestClient(func(*http.Request) (*http.Response, error) {
		if err := os.WriteFile(destination, []byte("existing report"), 0600); err != nil {
			t.Fatal(err)
		}
		return apiTestResponse(200, "application/zip", bytes.NewReader(archive)), nil
	})
	if err := client.Download("GET", "/task-reports/224", nil, destination); err == nil {
		t.Fatal("concurrent destination overwritten")
	}
	contents, _ := os.ReadFile(destination)
	if string(contents) != "existing report" {
		t.Fatalf("existing destination changed: %q", contents)
	}
	entries, _ := os.ReadDir(filepath.Dir(destination))
	if len(entries) != 1 {
		t.Fatal("failed publication left temporary file")
	}
}
