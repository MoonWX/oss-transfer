package transferstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	minPartSize   int64 = 5 * 1024 * 1024
	maxPartSize   int64 = 5 * 1024 * 1024 * 1024
	maxMemoryPart int64 = 64 * 1024 * 1024
)

type S3Config struct {
	Endpoint, Region, Bucket           string
	AccessKey, SecretKey, SessionToken string
	PathStyle                          bool
	// Empty selects V4; v2 enables the compatible S3 HMAC-SHA1 signature.
	SignatureVersion string
	PartSize         int64
	// PartConcurrency bounds concurrent upload requests; default 4, maximum 16.
	PartConcurrency int
	// MaxBufferedBytes bounds part payload buffers per Write; default 32 MiB.
	// Larger parts use disk spooling; source reads remain sequential.
	MaxBufferedBytes int64
	// Parts exceeding their buffer share or 64 MiB are spooled here for retries.
	SpoolDirectory   string
	RetryMaxAttempts int
	HTTPClient       *http.Client
}

type S3 struct {
	client                        *s3.Client
	bucket, scope, spoolDirectory string
	partSize                      int64
	partConcurrency               int
	maxBufferedBytes              int64
	connection                    S3ConnectionIdentity
	alias                         string
	uploadRegistry                S3UploadRegistry
	tempRegistry                  LocalTempRegistry
}

func NewS3(config S3Config) (*S3, error) {
	if config.SignatureVersion != "" && !strings.EqualFold(config.SignatureVersion, "v4") && !strings.EqualFold(config.SignatureVersion, "v2") {
		return nil, fmt.Errorf("%w: S3 signature version (use v2 or v4)", ErrUnsupported)
	}
	if config.PartConcurrency == 0 {
		config.PartConcurrency = 4
	}
	if config.PartConcurrency < 1 || config.PartConcurrency > 16 {
		return nil, errors.New("S3 part concurrency must be between 1 and 16")
	}
	if config.MaxBufferedBytes == 0 {
		config.MaxBufferedBytes = 32 * 1024 * 1024
	}
	if config.MaxBufferedBytes < 1024*1024 || config.MaxBufferedBytes > 1024*1024*1024 {
		return nil, errors.New("S3 buffered part budget must be between 1 MiB and 1 GiB")
	}
	if config.Bucket == "" || strings.ContainsAny(config.Bucket, "/\\\x00") || config.AccessKey == "" || config.SecretKey == "" {
		return nil, errors.New("S3 bucket and static credentials are required")
	}
	if config.Endpoint != "" {
		endpoint, err := url.Parse(config.Endpoint)
		if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
			return nil, errors.New("S3 endpoint must be an http or https origin")
		}
	}
	if config.Region == "" {
		config.Region = "us-east-1"
	}
	if config.PartSize == 0 {
		config.PartSize = 8 * 1024 * 1024
	}
	if config.PartSize < minPartSize || config.PartSize > maxPartSize {
		return nil, errors.New("S3 part size must be between 5 MiB and 5 GiB")
	}
	if config.RetryMaxAttempts == 0 {
		config.RetryMaxAttempts = 3
	}
	if config.RetryMaxAttempts < 1 || config.RetryMaxAttempts > 20 {
		return nil, errors.New("S3 retry attempts must be between 1 and 20")
	}
	sdkConfig := aws.Config{
		Region:      config.Region,
		Credentials: credentials.NewStaticCredentialsProvider(config.AccessKey, config.SecretKey, config.SessionToken),
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = config.RetryMaxAttempts })
		},
	}
	if config.HTTPClient != nil {
		sdkConfig.HTTPClient = config.HTTPClient
	}
	client := s3.NewFromConfig(sdkConfig, func(o *s3.Options) {
		if config.Endpoint != "" {
			o.BaseEndpoint = aws.String(config.Endpoint)
		}
		o.UsePathStyle = config.PathStyle
		if strings.EqualFold(config.SignatureVersion, "v2") {
			endpointHost := ""
			if config.Endpoint != "" {
				endpoint, _ := url.Parse(config.Endpoint)
				endpointHost = endpoint.Hostname()
			}
			o.HTTPSignerV4 = signatureV2{bucket: config.Bucket, forcePathStyle: config.PathStyle, endpointHost: endpointHost}
		}
		// Do not introduce optional AWS checksum trailers into compatible S3 APIs.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &S3{client: client, bucket: config.Bucket, scope: config.Endpoint + "/" + config.Region + "/" + config.Bucket, partSize: config.PartSize, partConcurrency: config.PartConcurrency, maxBufferedBytes: config.MaxBufferedBytes, spoolDirectory: config.SpoolDirectory, connection: s3ConnectionIdentity(config)}, nil
}

func (s *S3) Close() error { return nil }

func (s *S3) Scan(ctx context.Context, options ListOptions, emit func(Object) error) error {
	if emit == nil {
		return errors.New("scan callback is required")
	}
	for {
		page, err := s.List(ctx, options)
		if err != nil {
			return err
		}
		for _, object := range page.Objects {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := emit(object); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		options.Cursor = page.NextCursor
	}
}

func s3Key(key string, allowEmpty bool) error {
	if (!allowEmpty && key == "") || len(key) > 1024 || strings.ContainsRune(key, '\x00') || !utf8.ValidString(key) {
		return ErrInvalidKey
	}
	return nil
}

func s3Error(err error) error {
	if err == nil {
		return nil
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchVersion":
			return fmt.Errorf("%w: %w", ErrNotFound, err)
		case "PreconditionFailed":
			return fmt.Errorf("%w: %w", ErrSourceChanged, err)
		}
	}
	return err
}

func isPrecondition(err error) bool {
	var apiError smithy.APIError
	return errors.As(err, &apiError) && apiError.ErrorCode() == "PreconditionFailed"
}

func (s *S3) List(ctx context.Context, options ListOptions) (Page, error) {
	limit, err := pageLimit(options.Limit)
	if err != nil {
		return Page{}, err
	}
	if err = s3Key(options.Prefix, true); err != nil {
		return Page{}, err
	}
	if err = s3Key(options.StartAfter, true); err != nil {
		return Page{}, err
	}
	token, err := readCursor(options.Cursor, "s3", s.scope, options.Prefix, options.StartAfter)
	if err != nil {
		return Page{}, err
	}
	input := &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(options.Prefix), MaxKeys: aws.Int32(int32(limit))}
	if token != "" {
		input.ContinuationToken = aws.String(token)
	} else if options.StartAfter != "" {
		input.StartAfter = aws.String(options.StartAfter)
	}
	output, err := s.client.ListObjectsV2(ctx, input)
	if err != nil {
		return Page{}, s3Error(err)
	}
	page := Page{Objects: make([]Object, 0, len(output.Contents))}
	for _, entry := range output.Contents {
		page.Objects = append(page.Objects, Object{Key: aws.ToString(entry.Key), Size: aws.ToInt64(entry.Size), ETag: aws.ToString(entry.ETag), ModifiedAt: aws.ToTime(entry.LastModified)})
	}
	if aws.ToBool(output.IsTruncated) {
		next := aws.ToString(output.NextContinuationToken)
		if next == "" || next == token {
			return Page{}, errors.New("S3 returned a truncated listing without an advancing continuation token")
		}
		page.NextCursor = makeCursor("s3", s.scope, options.Prefix, options.StartAfter, next)
	}
	return page, nil
}

func (s *S3) Stat(ctx context.Context, key string) (Object, error) {
	if err := s3Key(key, false); err != nil {
		return Object{}, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return Object{}, s3Error(err)
	}
	return Object{Key: key, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag), VersionID: aws.ToString(out.VersionId), ModifiedAt: aws.ToTime(out.LastModified), ContentType: aws.ToString(out.ContentType), ContentEncoding: aws.ToString(out.ContentEncoding), ContentDisposition: aws.ToString(out.ContentDisposition), CacheControl: aws.ToString(out.CacheControl), StorageClass: string(out.StorageClass), ExpiresAt: aws.ToTime(out.Expires), LifecycleExpiresAt: lifecycleExpiry(aws.ToString(out.Expiration)), Metadata: copyMetadata(out.Metadata)}, nil
}

func (s *S3) Open(ctx context.Context, key string, options OpenOptions) (io.ReadCloser, Object, error) {
	if err := s3Key(key, false); err != nil {
		return nil, Object{}, err
	}
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if expected := options.Expected; expected != nil {
		if expected.Key != key {
			return nil, Object{}, ErrSourceChanged
		}
		if expected.ETag != "" {
			input.IfMatch = aws.String(expected.ETag)
		} else if !expected.ModifiedAt.IsZero() {
			input.IfUnmodifiedSince = aws.Time(expected.ModifiedAt)
		}
		if expected.VersionID != "" {
			input.VersionId = aws.String(expected.VersionID)
		}
	}
	out, err := s.client.GetObject(ctx, input)
	if err != nil {
		return nil, Object{}, s3Error(err)
	}
	object := Object{Key: key, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag), VersionID: aws.ToString(out.VersionId), ModifiedAt: aws.ToTime(out.LastModified), ContentType: aws.ToString(out.ContentType), ContentEncoding: aws.ToString(out.ContentEncoding), ContentDisposition: aws.ToString(out.ContentDisposition), CacheControl: aws.ToString(out.CacheControl), StorageClass: string(out.StorageClass), ExpiresAt: aws.ToTime(out.Expires), LifecycleExpiresAt: lifecycleExpiry(aws.ToString(out.Expiration)), Metadata: copyMetadata(out.Metadata)}
	if err = checkExpected(object, options.Expected); err != nil {
		_ = out.Body.Close()
		return nil, Object{}, err
	}
	return cancellable(ctx, out.Body, nil), object, nil
}

func writtenObject(key string, options WriteOptions, etag, version string) Object {
	return Object{Key: key, Size: options.Size, ETag: etag, VersionID: version, ContentType: options.ContentType, ContentEncoding: options.ContentEncoding, ContentDisposition: options.ContentDisposition, CacheControl: options.CacheControl, Metadata: copyMetadata(options.Metadata)}
}

func (s *S3) existing(ctx context.Context, key string, overwrite bool) (WriteResult, bool, error) {
	if overwrite {
		return WriteResult{}, false, nil
	}
	object, err := s.Stat(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return WriteResult{}, false, nil
	}
	if err != nil {
		return WriteResult{}, false, err
	}
	return WriteResult{Object: object, Skipped: true}, true, nil
}

func (s *S3) concurrentTarget(ctx context.Context, key string) (WriteResult, error) {
	result, found, err := s.existing(ctx, key, false)
	if err == nil && !found {
		err = fmt.Errorf("%w: concurrent destination disappeared", ErrSourceChanged)
	}
	return result, err
}

func (s *S3) Write(ctx context.Context, key string, input io.Reader, options WriteOptions) (WriteResult, error) {
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	if err := s3Key(key, false); err != nil {
		return WriteResult{}, err
	}
	if options.Size < 0 || input == nil {
		return WriteResult{}, errors.New("nonnegative size and input are required")
	}
	if result, found, err := s.existing(ctx, key, options.Overwrite); err != nil || found {
		return result, err
	}
	stop := interruptInput(ctx, input)
	defer stop()
	reader := contextReader{ctx, input}
	// Even an unusually large configured part must not cause a whole-file buffer.
	if options.Size <= s.partSize && options.Size <= min(maxMemoryPart, s.maxBufferedBytes) {
		body, err := readPart(reader, options.Size, "")
		if err != nil {
			return WriteResult{}, err
		}
		defer body.close()
		if err = requireEOF(reader); err != nil {
			return WriteResult{}, err
		}
		request := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: body.reader, ContentLength: aws.Int64(options.Size), Metadata: copyMetadata(options.Metadata)}
		setPutMetadata(request, options)
		if !options.Overwrite {
			request.IfNoneMatch = aws.String("*")
		}
		out, err := s.client.PutObject(ctx, request)
		if isPrecondition(err) && !options.Overwrite {
			return s.concurrentTarget(ctx, key)
		}
		if err != nil {
			return WriteResult{}, s3Error(err)
		}
		return WriteResult{Object: writtenObject(key, options, aws.ToString(out.ETag), aws.ToString(out.VersionId)), Committed: true}, nil
	}
	return s.multipart(ctx, key, input, options)
}

func setPutMetadata(request *s3.PutObjectInput, options WriteOptions) {
	if options.ContentType != "" {
		request.ContentType = aws.String(options.ContentType)
	}
	if options.ContentEncoding != "" {
		request.ContentEncoding = aws.String(options.ContentEncoding)
	}
	if options.ContentDisposition != "" {
		request.ContentDisposition = aws.String(options.ContentDisposition)
	}
	if options.CacheControl != "" {
		request.CacheControl = aws.String(options.CacheControl)
	}
}

type partBody struct {
	reader  io.ReadSeeker
	file    *os.File
	name    string
	cleanup func() error
}

func (p *partBody) close() error {
	if p.cleanup != nil {
		return p.cleanup()
	}
	if p.file == nil {
		return nil
	}
	return errors.Join(p.file.Close(), os.Remove(p.name))
}

func readPart(reader io.Reader, size int64, spool string) (*partBody, error) {
	return readPartWithLimit(reader, size, spool, maxMemoryPart)
}

func readPartWithLimit(reader io.Reader, size int64, spool string, memoryLimit int64) (*partBody, error) {
	if size <= memoryLimit {
		data := make([]byte, int(size))
		_, err := io.ReadFull(reader, data)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = fmt.Errorf("%w: source shorter than declared length", ErrSizeMismatch)
		}
		if err != nil {
			return nil, err
		}
		return &partBody{reader: bytes.NewReader(data)}, nil
	}
	file, err := os.CreateTemp(spool, "oss-transfer-part-*")
	if err != nil {
		return nil, err
	}
	body := &partBody{reader: file, file: file, name: file.Name()}
	count, err := io.CopyBuffer(file, io.LimitReader(reader, size), make([]byte, 256*1024))
	if err == nil && count != size {
		err = fmt.Errorf("%w: source shorter than declared length", ErrSizeMismatch)
	}
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil {
		return nil, errors.Join(err, body.close())
	}
	return body, nil
}

func (s *S3) multipart(ctx context.Context, key string, reader io.Reader, options WriteOptions) (result WriteResult, err error) {
	partSize := s.partSize
	needed := options.Size / 10000
	if options.Size%10000 != 0 {
		needed++
	}
	if needed > partSize {
		partSize = needed
	}
	if partSize > maxPartSize {
		return result, errors.New("object exceeds S3 multipart capacity")
	}
	create := &s3.CreateMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Metadata: copyMetadata(options.Metadata)}
	if options.ContentType != "" {
		create.ContentType = aws.String(options.ContentType)
	}
	if options.ContentEncoding != "" {
		create.ContentEncoding = aws.String(options.ContentEncoding)
	}
	if options.ContentDisposition != "" {
		create.ContentDisposition = aws.String(options.ContentDisposition)
	}
	if options.CacheControl != "" {
		create.CacheControl = aws.String(options.CacheControl)
	}
	upload, err := s.client.CreateMultipartUpload(ctx, create)
	if err != nil {
		return result, s3Error(err)
	}
	if aws.ToString(upload.UploadId) == "" {
		return result, errors.New("S3 returned an empty multipart upload ID")
	}
	completed := false
	registered := false
	var record S3UploadRecord
	defer func() {
		if completed {
			return
		}
		abortContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		var abortErr error
		if registered {
			abortErr = s.AbortRecordedUpload(abortContext, record)
		} else {
			_, abortErr = s.client.AbortMultipartUpload(abortContext, &s3.AbortMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: upload.UploadId})
			if noSuchUpload(abortErr) {
				abortErr = nil
			}
		}
		if abortErr != nil {
			err = errors.Join(err, fmt.Errorf("abort multipart upload: %w", abortErr))
		} else if registered {
			err = errors.Join(err, s.uploadRegistry.ForgetUpload(context.WithoutCancel(ctx), record.ID))
		}
	}()
	if s.uploadRegistry != nil {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return result, err
		}
		record = S3UploadRecord{Owner: s.uploadRegistry.OwnerID(), ID: hex.EncodeToString(nonce[:]), Connection: s.ConnectionIdentity(s.alias), Bucket: s.bucket, Key: key, UploadID: aws.ToString(upload.UploadId)}
		// No part payload is sent until this record has been durably committed.
		if err := s.uploadRegistry.RegisterUpload(ctx, record); err != nil {
			return result, err
		}
		registered = true
	}
	parts, uploadErr := s.uploadParts(ctx, key, reader, options.Size, partSize, upload.UploadId)
	if uploadErr != nil {
		return result, uploadErr
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	complete := &s3.CompleteMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: upload.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}}
	if !options.Overwrite {
		complete.IfNoneMatch = aws.String("*")
	}
	output, err := s.client.CompleteMultipartUpload(ctx, complete)
	if isPrecondition(err) && !options.Overwrite {
		result, err = s.concurrentTarget(ctx, key)
		return result, err
	}
	if err != nil {
		return result, s3Error(err)
	}
	completed = true
	result = WriteResult{Object: writtenObject(key, options, aws.ToString(output.ETag), aws.ToString(output.VersionId)), Committed: true}
	if registered {
		err = s.uploadRegistry.ForgetUpload(context.WithoutCancel(ctx), record.ID)
	}
	return result, err
}

// uploadParts reads the source once in order. A slot is acquired before allocating
// or spooling a part, so a slow server cannot accumulate unbounded queued bodies.
func (s *S3) uploadParts(ctx context.Context, key string, input io.Reader, size, partSize int64, uploadID *string) ([]types.CompletedPart, error) {
	uploadContext, cancel := context.WithCancel(ctx)
	stop := interruptInput(uploadContext, input)
	defer func() { stop(); cancel() }()
	reader := contextReader{uploadContext, input}
	// Each active slot has its own share of the payload budget, including the
	// last (smaller) part of an otherwise disk-spooled upload.
	concurrency := min(s.partConcurrency, int(s.maxBufferedBytes/(256*1024)))
	memoryLimit := min(maxMemoryPart, s.maxBufferedBytes/int64(concurrency))
	slots := make(chan struct{}, concurrency)
	count := int((size + partSize - 1) / partSize)
	parts := make([]types.CompletedPart, count)
	var group sync.WaitGroup
	var failure sync.Once
	var firstError error
	fail := func(err error) {
		if err != nil {
			failure.Do(func() { firstError = err; cancel() })
		}
	}
	for remaining, number := size, int32(1); remaining > 0; number++ {
		acquired := false
		select {
		case slots <- struct{}{}:
			acquired = true
		case <-uploadContext.Done():
			fail(uploadContext.Err())
		}
		if uploadContext.Err() != nil {
			// A slot may have been acquired concurrently with cancellation.
			if acquired {
				<-slots
			}
			fail(uploadContext.Err())
			break
		}
		partLength := min(remaining, partSize)
		body, err := s.readPart(uploadContext, reader, partLength, memoryLimit)
		if err != nil {
			<-slots
			fail(err)
			break
		}
		group.Add(1)
		go func(number int32, body *partBody, length int64) {
			defer group.Done()
			defer func() { <-slots }()
			output, err := s.client.UploadPart(uploadContext, &s3.UploadPartInput{Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: uploadID, PartNumber: aws.Int32(number), Body: body.reader, ContentLength: aws.Int64(length)})
			cleanupErr := body.close()
			if err != nil || cleanupErr != nil {
				fail(errors.Join(s3Error(err), cleanupErr))
				return
			}
			if aws.ToString(output.ETag) == "" {
				fail(errors.New("S3 returned an empty ETag for part " + strconv.Itoa(int(number))))
				return
			}
			parts[int(number)-1] = types.CompletedPart{ETag: output.ETag, PartNumber: aws.Int32(number)}
		}(number, body, partLength)
		remaining -= partLength
	}
	if uploadContext.Err() == nil {
		fail(requireEOF(reader))
	}
	group.Wait()
	if firstError != nil {
		return nil, firstError
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return parts, nil
}

// ListBuckets validates the connection using the same authentication and endpoint
// settings as object transfers. Permission and service failures propagate.
func ListBuckets(ctx context.Context, config S3Config) ([]string, error) {
	config.Bucket = "oss-transfer-connection-check"
	store, err := NewS3(config)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	names := []string{}
	input := &s3.ListBucketsInput{}
	for {
		output, err := store.client.ListBuckets(ctx, input)
		if err != nil {
			return nil, s3Error(err)
		}
		for _, bucket := range output.Buckets {
			if name := aws.ToString(bucket.Name); name != "" {
				names = append(names, name)
			}
		}
		next := aws.ToString(output.ContinuationToken)
		if next == "" {
			return names, nil
		}
		if next == aws.ToString(input.ContinuationToken) {
			return nil, errors.New("S3 returned a non-advancing bucket continuation token")
		}
		input.ContinuationToken = aws.String(next)
	}
}

func lifecycleExpiry(value string) time.Time {
	const prefix = `expiry-date="`
	_, rest, ok := strings.Cut(value, prefix)
	if !ok {
		return time.Time{}
	}
	raw, _, ok := strings.Cut(rest, `"`)
	if !ok {
		return time.Time{}
	}
	parsed, _ := http.ParseTime(raw)
	return parsed
}
