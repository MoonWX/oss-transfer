package storageconfig

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type countingSnapshots struct {
	value Snapshot
	calls int
}

func (s *countingSnapshots) Snapshot() (Snapshot, error) { s.calls++; return s.value, nil }

func TestFactoryKeepsCredentialValuesAndOneSnapshotPerJob(t *testing.T) {
	config := testConfig("qa")
	v2 := true
	config.UseV2SignerType = &v2
	settings, err := config.S3Config("test-bucket")
	if err != nil {
		t.Fatal(err)
	}
	if settings.AccessKey != config.AccessKey || settings.SecretKey != config.SecretKey || settings.SessionToken != config.SessionToken || settings.Region != config.Region || !settings.PathStyle || settings.SignatureVersion != "v2" {
		t.Fatal("client configuration altered credentials or ignored signer/path settings")
	}
	// Constructing clients must not be represented as network validation. This
	// endpoint has no server, and NewS3 performs no request during construction.
	config.Endpoint = "http://127.0.0.1:1"
	config.UseV2SignerType = nil
	provider := &countingSnapshots{value: Snapshot{Revision: 1, Connections: []Config{config}}}
	factory := &Factory{Source: provider}
	endpoint := execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "test-bucket"}
	source, destination, err := factory.Open(context.Background(), execution.Job{Source: endpoint, Destination: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	defer destination.Close()
	if provider.calls != 1 || source == destination {
		t.Fatal("job opened endpoints from separate snapshots or shared a handle")
	}
	if _, ok := source.(*transferstore.S3); !ok {
		t.Fatal("S3 endpoint did not create an S3 store")
	}
	endpoint.Ref = "absent"
	if _, err := factory.OpenEndpoint(context.Background(), endpoint); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown alias did not fail", err)
	}
}

func TestFactoryLocalHandlesAndEndpointValidation(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "source.txt"), []byte("real local file"), 0600); err != nil {
		t.Fatal(err)
	}
	factory := &Factory{}
	endpoint := execution.Endpoint{Kind: "LOCAL", Root: directory}
	source, destination, err := factory.Open(context.Background(), execution.Job{Source: endpoint, Destination: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	reader, object, err := destination.Open(context.Background(), "source.txt", transferstore.OpenOptions{})
	if err != nil {
		t.Fatal("closing the source also closed the independent destination", err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(data) != "real local file" || object.Size != int64(len(data)) {
		t.Fatal("local factory returned wrong file", err)
	}
	result, err := destination.Write(context.Background(), "copied.txt", strings.NewReader(string(data)), transferstore.WriteOptions{Size: int64(len(data))})
	if err != nil || !result.Committed {
		t.Fatal("local factory could not write a real file", err)
	}
	actual, err := os.ReadFile(filepath.Join(directory, "copied.txt"))
	if err != nil || string(actual) != string(data) {
		t.Fatal("written destination bytes differ", err)
	}
	for _, bad := range []execution.Endpoint{
		{Kind: "LOCAL", Root: "relative"}, {Kind: "LOCAL", Root: directory, Ref: "ignored"},
		{Kind: "LOCAL", Root: directory, Bucket: "ignored"}, {Kind: "unknown", Root: directory},
		{Kind: "S3", Ref: "qa", Bucket: "bucket"},
	} {
		if opened, err := factory.OpenEndpoint(context.Background(), bad); err == nil {
			opened.Close()
			t.Fatal("invalid endpoint was accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := factory.Open(ctx, execution.Job{Source: endpoint, Destination: endpoint}); !errors.Is(err, context.Canceled) {
		t.Fatal("factory ignored cancellation", err)
	}
}

func TestS3ConfigResolvesAutomaticAddressing(t *testing.T) {
	on, off := true, false
	for _, test := range []struct {
		endpoint string
		explicit *bool
		want     bool
	}{
		// Automatic: self-hosted endpoints use path-style, cloud DNS names
		// stay virtual-hosted.
		{"http://host.docker.internal:18333", nil, true},
		{"http://10.0.0.5", nil, true},
		{"http://seaweed", nil, true},
		{"https://s3.cn-north-1.jdcloud-oss.com", nil, false},
		{"https://s3.us-east-1.amazonaws.com", nil, false},
		// An explicit choice always wins, in both directions.
		{"http://10.0.0.5:9000", &off, false},
		{"https://s3.cn-north-1.jdcloud-oss.com", &on, true},
	} {
		config := testConfig("qa")
		config.Endpoint, config.PathStyleEnable = test.endpoint, test.explicit
		settings, err := config.S3Config("bucket")
		if err != nil {
			t.Fatal(err)
		}
		if settings.PathStyle != test.want || config.EffectivePathStyle() != test.want {
			t.Errorf("%s (explicit %v): path-style %v, want %v", test.endpoint, test.explicit, settings.PathStyle, test.want)
		}
	}
}

// An object lookup that a person waits for gives up after a short budget,
// so the caller sees the storage's own error rather than its deadline. A
// transfer handle keeps the long budget that rides out throttling.
func TestInteractiveHandleRetriesBriefly(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>SlowDown</Code><Message>throttled</Message></Error>`)
	}))
	defer server.Close()
	config := testConfig("qa")
	config.Endpoint = server.URL
	factory := &Factory{Source: &countingSnapshots{value: Snapshot{Revision: 1, Connections: []Config{config}}}, HTTPClient: server.Client()}
	endpoint := execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "test-bucket"}
	store, err := factory.OpenInteractive(context.Background(), endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// The deadline only guards a hang; the budget, not the deadline, must end the lookup.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := store.Stat(ctx, "object"); err == nil || ctx.Err() != nil {
		t.Fatalf("lookup: %v (context %v)", err, ctx.Err())
	}
	if got := requests.Load(); got != transferstore.InteractiveRetryAttempts {
		t.Fatalf("%d requests, want %d", got, transferstore.InteractiveRetryAttempts)
	}
	if factory.interactive {
		t.Fatal("an interactive handle changed the shared factory")
	}
}
