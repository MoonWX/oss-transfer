package transferstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Permission check operations, in the order they run.
const (
	PermissionList         = "LIST"
	PermissionHead         = "HEAD"
	PermissionGet          = "GET"
	PermissionPut          = "PUT"
	PermissionHeadWritten  = "HEAD_WRITTEN"
	PermissionMultipart    = "MULTIPART"
	PermissionDeleteProbe  = "DELETE_PROBE"
	permissionCheckTimeout = 45 * time.Second
	// PermissionProbePrefix holds the objects a write check creates.
	PermissionProbePrefix = ".oss-transfer-permission-check/"
)

// Permission check outcomes.
const (
	PermissionPassed  = "PASSED"
	PermissionFailed  = "FAILED"
	PermissionSkipped = "SKIPPED"
)

// PermissionResult is the outcome of one storage operation of a permission
// check. Key names the object the operation used, when there was one.
type PermissionResult struct {
	Operation string           `json:"operation"`
	Status    string           `json:"status"`
	Key       string           `json:"key,omitempty"`
	Reason    string           `json:"reason,omitempty"`
	Error     string           `json:"error,omitempty"`
	ErrorKind StorageErrorKind `json:"errorKind,omitempty"`
}

// Reasons for a skipped operation.
const (
	PermissionReasonEmptyBucket  = "EMPTY_BUCKET"
	PermissionReasonListFailed   = "LIST_FAILED"
	PermissionReasonPutFailed    = "PUT_FAILED"
	PermissionReasonNotRequested = "NOT_REQUESTED"
	// Writing to a missing bucket is not tried: some stores (SeaweedFS)
	// would create the bucket.
	PermissionReasonNoSuchBucket = "NO_SUCH_BUCKET"
)

// PermissionCheck reports the operations a migration uses on one bucket.
type PermissionCheck struct {
	Bucket    string             `json:"bucket"`
	PathStyle bool               `json:"pathStyle"`
	Write     bool               `json:"write"`
	Results   []PermissionResult `json:"results"`
	// LeftoverKey names a probe object the check wrote but could not delete.
	LeftoverKey string `json:"leftoverKey,omitempty"`
}

// CheckBucketPermissions runs, on config.Bucket, the requests a migration
// makes: listing objects, reading an object's metadata and content (a one-byte
// range of the first listed object) and, when write is set, a conditional
// PUT of a small probe object, reading it back, a multipart upload that is
// aborted, and deleting the probe. Only the probe object under
// PermissionProbePrefix is written; DeleteObject is not needed for migrations
// and is reported so the operator can remove a leftover probe.
func CheckBucketPermissions(ctx context.Context, config S3Config, write bool) (PermissionCheck, error) {
	ctx, cancel := context.WithTimeout(ctx, permissionCheckTimeout)
	defer cancel()
	// The operator waits for the answer: use the short budget of checks.
	if config.RetryMaxAttempts == 0 {
		config.RetryMaxAttempts = InteractiveRetryAttempts
	}
	if config.ThrottleRetryMaxAttempts == 0 {
		config.ThrottleRetryMaxAttempts = config.RetryMaxAttempts
	}
	store, err := NewS3(config)
	if err != nil {
		return PermissionCheck{}, err
	}
	defer store.Close()
	check := PermissionCheck{Bucket: config.Bucket, PathStyle: config.PathStyle, Write: write, Results: []PermissionResult{}}
	add := func(result PermissionResult, err error) PermissionResult {
		if err != nil {
			err = s3Error(err)
			result.Status, result.Error, result.ErrorKind = PermissionFailed, err.Error(), ClassifyStorageError(err)
		} else if result.Status == "" {
			result.Status = PermissionPassed
		}
		check.Results = append(check.Results, result)
		return result
	}
	skip := func(operation, reason string) {
		check.Results = append(check.Results, PermissionResult{Operation: operation, Status: PermissionSkipped, Reason: reason})
	}
	bucket := aws.String(config.Bucket)

	// Read: list one object, then read its metadata and its first byte.
	listed, err := store.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket, MaxKeys: aws.Int32(1)})
	list := add(PermissionResult{Operation: PermissionList}, err)
	sample := ""
	if list.Status == PermissionPassed && len(listed.Contents) > 0 {
		sample = aws.ToString(listed.Contents[0].Key)
	}
	switch {
	case list.Status != PermissionPassed:
		skip(PermissionHead, PermissionReasonListFailed)
		skip(PermissionGet, PermissionReasonListFailed)
	case sample == "":
		skip(PermissionHead, PermissionReasonEmptyBucket)
		skip(PermissionGet, PermissionReasonEmptyBucket)
	default:
		_, err = store.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String(sample)})
		add(PermissionResult{Operation: PermissionHead, Key: sample}, err)
		var out *s3.GetObjectOutput
		out, err = store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: aws.String(sample), Range: aws.String("bytes=0-0")})
		if err == nil {
			out.Body.Close()
		}
		add(PermissionResult{Operation: PermissionGet, Key: sample}, err)
	}
	if !write || list.ErrorKind == StorageErrorNoSuchBucket {
		reason := PermissionReasonNotRequested
		if write {
			reason = PermissionReasonNoSuchBucket
		}
		for _, operation := range []string{PermissionPut, PermissionHeadWritten, PermissionMultipart} {
			skip(operation, reason)
		}
		return check, ctx.Err()
	}

	// Write: a conditional PUT as transfers that skip existing objects make,
	// reading it back, an aborted multipart upload, and removing the probe.
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return check, err
	}
	probe := PermissionProbePrefix + time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(suffix)
	body := []byte("oss-transfer permission check\n")
	_, err = store.client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: aws.String(probe), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))), IfNoneMatch: aws.String("*")})
	put := add(PermissionResult{Operation: PermissionPut, Key: probe}, err)
	if put.Status == PermissionPassed {
		_, err = store.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String(probe)})
		add(PermissionResult{Operation: PermissionHeadWritten, Key: probe}, err)
	} else {
		skip(PermissionHeadWritten, PermissionReasonPutFailed)
	}
	multipartKey := probe + "-multipart"
	created, err := store.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: aws.String(multipartKey)})
	if err == nil {
		uploadID := created.UploadId
		_, err = store.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: bucket, Key: aws.String(multipartKey), UploadId: uploadID, PartNumber: aws.Int32(1), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body)))})
		// Abort even after a failed part, so nothing is left behind.
		_, abortErr := store.client.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{Bucket: bucket, Key: aws.String(multipartKey), UploadId: uploadID})
		err = errors.Join(err, abortErr)
	}
	add(PermissionResult{Operation: PermissionMultipart, Key: multipartKey}, err)
	if put.Status == PermissionPassed {
		_, err = store.client.DeleteObject(context.WithoutCancel(ctx), &s3.DeleteObjectInput{Bucket: bucket, Key: aws.String(probe)})
		if deleted := add(PermissionResult{Operation: PermissionDeleteProbe, Key: probe}, err); deleted.Status != PermissionPassed {
			check.LeftoverKey = probe
		}
	}
	return check, nil
}
