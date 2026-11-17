package discovery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// stalledListFactory opens the S3 source of a scan through client.
type stalledListFactory struct {
	url    string
	client *http.Client
}

func (f stalledListFactory) OpenEndpoint(ctx context.Context, endpoint execution.Endpoint) (transferstore.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return transferstore.NewS3(transferstore.S3Config{Endpoint: f.url, Region: "test", Bucket: endpoint.Bucket,
		AccessKey: "test-access", SecretKey: "test-secret", PathStyle: true, RetryMaxAttempts: 1, HTTPClient: f.client})
}

// respondedTransport reports each listing whose response headers arrived.
type respondedTransport struct {
	base      http.RoundTripper
	responded chan<- struct{}
}

func (t respondedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err == nil && request.URL.Query().Has("list-type") {
		select {
		case t.responded <- struct{}{}:
		default:
		}
	}
	return response, err
}

// A scan its lease ends stays RECOVERABLE, whatever the lease's cause. An
// Agent's lease ends with transferstore.ErrInterrupted (a shutdown, an expiry
// or a new lease) or ErrMoved (retire, rejoin), and net/http returns that
// cause in place of context.Canceled from a response body it cuts off. A
// FAILED scan would survive a restart and never be scanned again.
func TestScanEndedByItsLeaseStaysRecoverable(t *testing.T) {
	for _, cause := range []error{nil, transferstore.ErrInterrupted, transferstore.ErrMoved} {
		t.Run(fmt.Sprint(cause), func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Half a listing, then nothing until the client goes away.
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>a`)
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			defer close(release)
			responded := make(chan struct{}, 1)
			client := server.Client()
			client.Transport = respondedTransport{base: client.Transport, responded: responded}
			lease, cancelLease := context.WithCancelCause(context.Background())
			defer cancelLease(nil)
			scanner, _ := newScanner(t, filepath.Join(t.TempDir(), "agent.db"), stalledListFactory{url: server.URL, client: client},
				Options{SpecContext: func(Spec) (context.Context, error) { return lease, nil }})
			spec := Spec{ParentID: 1, Generation: 1, Source: execution.Endpoint{Kind: "S3", Ref: "saved", Bucket: "bucket"}}
			if _, err := scanner.Start(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			select {
			case <-responded:
			case <-time.After(time.Minute): // hang guard only
				t.Fatal("the listing never responded")
			}
			// The SDK now reads the body that never ends.
			cancelLease(cause)
			if current := waitStatus(t, scanner, Recoverable); current.ScanComplete {
				t.Fatalf("an interrupted scan completed: %+v", current)
			}
		})
	}
}
