package storageconfig

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type SnapshotSource interface {
	Snapshot() (Snapshot, error)
}

type Factory struct {
	Source SnapshotSource
	// Transfer options are process settings, not part of connection identity.
	PartSize              int64
	PartConcurrency       int
	UploadMemory          *transferstore.UploadMemoryBudget
	SpoolDirectory        string
	RetryMaxAttempts      int
	HTTPClient            *http.Client
	HTTPClientForEndpoint func(context.Context, execution.Endpoint, Config) *http.Client
}

var _ execution.StoreFactory = (*Factory)(nil)

// S3Config only constructs client settings. It performs no network validation.
func (config Config) S3Config(bucket string) (transferstore.S3Config, error) {
	if err := Validate(config); err != nil {
		return transferstore.S3Config{}, err
	}
	if !validString(bucket, 255, true) || strings.ContainsAny(bucket, "/\\ ") || bucket == "." || bucket == ".." {
		return transferstore.S3Config{}, invalid("bucket")
	}
	signature := "v4"
	if config.UseV2SignerType != nil && *config.UseV2SignerType {
		signature = "v2"
	}
	return transferstore.S3Config{
		Endpoint: config.Endpoint, Region: config.Region, Bucket: bucket,
		AccessKey: config.AccessKey, SecretKey: config.SecretKey, SessionToken: config.SessionToken,
		PathStyle:        config.PathStyleEnable != nil && *config.PathStyleEnable,
		SignatureVersion: signature,
	}, nil
}

func (f *Factory) Open(ctx context.Context, job execution.Job) (transferstore.Store, transferstore.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	direction := "OSS_TO_OSS"
	if job.Source.Kind == "LOCAL" {
		direction = "LOCAL_TO_OSS"
	} else if job.Destination.Kind == "LOCAL" {
		direction = "OSS_TO_LOCAL"
	}
	ctx = nodecontrol.WithRequestContext(ctx, nodecontrol.RequestContext{TaskID: job.ParentID, Generation: job.Generation, Direction: direction})
	current, err := f.forEndpoints(job.Source, job.Destination)
	if err != nil {
		return nil, nil, err
	}
	source, err := f.open(ctx, current, job.Source)
	if err != nil {
		return nil, nil, err
	}
	destination, err := f.open(ctx, current, job.Destination)
	if err != nil {
		_ = source.Close()
		return nil, nil, err
	}
	return source, destination, nil
}

func (f *Factory) OpenEndpoint(ctx context.Context, endpoint execution.Endpoint) (transferstore.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, err := f.forEndpoints(endpoint)
	if err != nil {
		return nil, err
	}
	return f.open(ctx, current, endpoint)
}

func (f *Factory) forEndpoints(endpoints ...execution.Endpoint) (Snapshot, error) {
	for _, endpoint := range endpoints {
		if endpoint.Kind == "S3" {
			if f.Source == nil {
				return Snapshot{}, invalid("S3 configuration source is missing")
			}
			current, err := f.Source.Snapshot()
			if err != nil {
				return Snapshot{}, err
			}
			return canonical(current)
		}
	}
	return Snapshot{}, nil
}

func (f *Factory) open(ctx context.Context, snapshot Snapshot, endpoint execution.Endpoint) (transferstore.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch endpoint.Kind {
	case "LOCAL":
		if endpoint.Root == "" || !filepath.IsAbs(endpoint.Root) || strings.ContainsRune(endpoint.Root, '\x00') || endpoint.Ref != "" || endpoint.Bucket != "" {
			return nil, invalid("LOCAL endpoint requires an absolute root without a connection or bucket")
		}
		return transferstore.NewLocal(endpoint.Root)
	case "S3":
		if endpoint.Root != "" || !validAlias(endpoint.Ref) {
			return nil, invalid("S3 endpoint requires a connection alias without a local root")
		}
		for _, config := range snapshot.Connections {
			if config.Alias != endpoint.Ref {
				continue
			}
			settings, err := config.S3Config(endpoint.Bucket)
			if err != nil {
				return nil, err
			}
			settings.PartSize, settings.PartConcurrency = f.PartSize, f.PartConcurrency
			settings.UploadMemory = f.UploadMemory
			settings.SpoolDirectory, settings.RetryMaxAttempts = f.SpoolDirectory, f.RetryMaxAttempts
			settings.HTTPClient = f.HTTPClient
			if f.HTTPClientForEndpoint != nil {
				settings.HTTPClient = f.HTTPClientForEndpoint(ctx, endpoint, config)
			}
			return transferstore.NewS3(settings)
		}
		return nil, ErrNotFound
	default:
		return nil, invalid("endpoint kind must be S3 or LOCAL")
	}
}
