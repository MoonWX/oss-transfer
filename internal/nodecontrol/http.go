package nodecontrol

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type requestContextKey struct{}

func WithRequestContext(ctx context.Context, info RequestContext) context.Context {
	return context.WithValue(ctx, requestContextKey{}, info)
}

func RequestContextFrom(ctx context.Context) RequestContext {
	info, _ := ctx.Value(requestContextKey{}).(RequestContext)
	return info
}

type transport struct {
	controller *Controller
	base       http.RoundTripper
	info       RequestContext
}

func (c *Controller) Transport(base http.RoundTripper, info RequestContext) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{c, base, info}
}

func (t *transport) RoundTrip(request *http.Request) (*http.Response, error) {
	info := t.info
	if value, ok := request.Context().Value(requestContextKey{}).(RequestContext); ok {
		info.TaskID = value.TaskID
		info.Generation = value.Generation
		info.Direction = value.Direction
	}
	info.Method = request.Method
	info.ContextSource = "HTTP_ATTEMPT"
	path := strings.TrimPrefix(request.URL.Path, "/")
	if info.PathStyle && info.Bucket != "" {
		if path == info.Bucket {
			path = ""
		} else {
			path = strings.TrimPrefix(path, info.Bucket+"/")
		}
	}
	info.Key = path
	query := request.URL.Query()
	switch request.Method {
	case "HEAD":
		info.Operation = "HeadObject"
	case "PUT":
		info.Operation = "PutObject"
		if query.Has("partNumber") {
			info.Operation = "UploadPart"
			info.PartNumber, _ = strconv.Atoi(query.Get("partNumber"))
		}
	case "POST":
		info.Operation = "CompleteMultipartUpload"
		if query.Has("uploads") {
			info.Operation = "CreateMultipartUpload"
		}
	case "GET":
		info.Operation = "GetObject"
		if info.Key == "" || query.Has("list-type") || query.Has("prefix") || query.Has("delimiter") {
			info.Operation = "ListObjects"
			if info.Bucket == "" && path == "" {
				info.Operation = "ListBuckets"
			}
		} else if query.Has("uploadId") {
			// Multipart bookkeeping, not an object read; still task LIST-charged.
			info.Operation = "ListParts"
		}
	case "DELETE":
		info.Operation = "AbortMultipartUpload"
	}
	// Object GETs, multipart listings and aborts have no node request limit.
	kind := operationLimit(info.Operation)
	taskKind := kind
	if request.Method == "GET" && taskKind == "" {
		// V4's task LIST budget also covered object reads during downloads.
		taskKind = "list"
	}
	if kind != "" || info.TaskID > 0 && info.Generation > 0 && taskKind != "" {
		if err := t.controller.BeforeRequest(request.Context(), info, kind, taskKind); err != nil {
			return nil, err
		}
		t.controller.requests.add(info)
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	info.StatusCode = response.StatusCode
	// Every attempt the SDK makes passes here, retries and the final one
	// included. Error codes are read for all error statuses because services
	// also report throttling such as SlowDown with statuses other than 503.
	if response.StatusCode >= 400 && response.Body != nil && request.Method != http.MethodHead {
		body := response.Body
		prefix, readErr := io.ReadAll(io.LimitReader(body, 64<<10))
		response.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
		if readErr == nil {
			var problem struct {
				Code string `xml:"Code"`
			}
			if xml.Unmarshal(prefix, &problem) == nil && safeCode(problem.Code) {
				info.ErrorCode = problem.Code
			}
		}
	}
	if transferstore.IsThrottleResponse(response.StatusCode, info.ErrorCode) {
		info.Message = "Storage service returned HTTP " + strconv.Itoa(response.StatusCode)
		info.RequestID = bounded(response.Header.Get("X-Amz-Request-Id"), 256)
		if info.RequestID == "" {
			info.RequestID = bounded(response.Header.Get("X-Oss-Request-Id"), 256)
		}
		if info.RequestID == "" {
			info.RequestID = bounded(response.Header.Get("X-Jdcloud-Request-Id"), 256)
		}
		info.ExtendedRequestID = bounded(response.Header.Get("X-Amz-Id-2"), 512)
	}
	if err := t.controller.ObserveResponse(info); err != nil {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, err
	}
	return response, nil
}

type replayBody struct {
	io.Reader
	io.Closer
}

func bounded(value string, max int) string {
	if len(value) > max {
		return value[:max]
	}
	return value
}
func safeCode(value string) bool {
	if len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-.", r)) {
			return false
		}
	}
	return true
}

type observedReader struct {
	io.ReadCloser
	controller *Controller
}

func (r *observedReader) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	r.controller.ObserveBytes(n)
	return n, err
}
func (c *Controller) Decorator(next execution.ReaderDecorator) execution.ReaderDecorator {
	return func(ctx context.Context, job execution.Job, item execution.Item, input io.ReadCloser) (io.ReadCloser, error) {
		reader, err := next(ctx, job, item, input)
		if err != nil {
			return nil, err
		}
		return &observedReader{reader, c}, nil
	}
}
