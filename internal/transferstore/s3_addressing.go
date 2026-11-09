package transferstore

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
)

// AutoPathStyle is the addressing a connection uses when it does not choose
// one: path-style for endpoints that cannot give every bucket its own DNS
// name, virtual-hosted for public cloud endpoints. An endpoint is path-style
// when its URL has an explicit port, or its host is an IP address,
// "localhost", a name without a dot, or a name ending in .internal, .local or
// .lan. Self-hosted S3 (MinIO, SeaweedFS, Ceph) is normally reached this way;
// AWS, JD Cloud and other cloud endpoints (a DNS name without a port) stay
// virtual-hosted. An empty endpoint selects the AWS default: virtual-hosted.
func AutoPathStyle(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	if parsed.Port() != "" {
		return true
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil || host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".internal", ".local", ".lan"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// connectionCheckTimeout bounds a whole connection check, whatever retry budget
// the S3 client applies to transfers.
const connectionCheckTimeout = 20 * time.Second

// ConnectionCheck is the outcome of CheckConnection. Storage failures are part
// of the result, not an error, so the caller can tell a blocking failure from
// a warning: ListBuckets denied to bucket-scoped keys does not prove anything
// is wrong with the connection.
type ConnectionCheck struct {
	Buckets []string `json:"buckets"`
	// PathStyle is the addressing the checks used.
	PathStyle     bool             `json:"pathStyle"`
	ListError     string           `json:"listError,omitempty"`
	ListErrorKind StorageErrorKind `json:"listErrorKind,omitempty"`
	// Bucket is the bucket of the bucket-level check (ListObjectsV2 with
	// max-keys=1): the requested bucket, or else the first listed bucket.
	// Empty when no bucket was known or ListBuckets failed.
	Bucket          string           `json:"bucket,omitempty"`
	BucketError     string           `json:"bucketError,omitempty"`
	BucketErrorKind StorageErrorKind `json:"bucketErrorKind,omitempty"`
}

// CheckConnection lists the buckets and then lists at most one object of one
// bucket with the connection's effective addressing. ListBuckets reaches the
// endpoint itself, so only the bucket-level request shows whether the bucket
// can be addressed the configured way (for example a virtual-hosted bucket
// name that does not resolve). config.Bucket is ignored.
func CheckConnection(ctx context.Context, config S3Config, bucket string) (ConnectionCheck, error) {
	ctx, cancel := context.WithTimeout(ctx, connectionCheckTimeout)
	defer cancel()
	// A check answers a person waiting for it: do not spend the long
	// throttling budget of transfers.
	if config.RetryMaxAttempts == 0 {
		config.RetryMaxAttempts = 2
	}
	if config.ThrottleRetryMaxAttempts == 0 {
		config.ThrottleRetryMaxAttempts = config.RetryMaxAttempts
	}
	result := ConnectionCheck{Buckets: []string{}, PathStyle: config.PathStyle}
	buckets, err := ListBuckets(ctx, config)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return result, ctx.Err()
		}
		result.ListError, result.ListErrorKind = err.Error(), ClassifyStorageError(err)
		// Without ListBuckets permission the keys may still be valid for one
		// bucket: check the bucket the caller names.
		if result.ListErrorKind != StorageErrorAccessDenied || bucket == "" {
			return result, nil
		}
	} else {
		result.Buckets = buckets
		if bucket == "" && len(buckets) > 0 {
			bucket = buckets[0]
		}
	}
	if bucket == "" {
		return result, nil
	}
	result.Bucket = bucket
	config.Bucket = bucket
	store, err := NewS3(config)
	if err != nil {
		return result, err
	}
	defer store.Close()
	if _, err = store.List(ctx, ListOptions{Limit: 1}); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return result, ctx.Err()
		}
		result.BucketError, result.BucketErrorKind = err.Error(), ClassifyStorageError(err)
	}
	return result, nil
}
