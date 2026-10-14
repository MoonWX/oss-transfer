package storageconfig

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
