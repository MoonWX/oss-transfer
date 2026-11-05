package transferstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	defaultRangedMinSize int64 = 128 << 20
	// 16 MiB halves request count and put QPS for objects of 1 GiB and more,
	// while sixteen resident parts still fit the default 256 MiB node budget.
	largeObjectSize     int64 = 1 << 30
	largeObjectPartSize int64 = 16 << 20
	rangeReadAttempts         = 3
)

// multipartPartSize keeps the 10,000-part limit. Only ranged parts grow for
// large objects: they are always resident, whereas a larger streamed part
// would exceed its MaxBufferedBytes share and be spooled to disk.
func (s *S3) multipartPartSize(size int64, ranged bool) int64 {
	partSize := s.partSize
	if ranged && size >= largeObjectSize && partSize < largeObjectPartSize {
		partSize = largeObjectPartSize
	}
	needed := size / 10000
	if size%10000 != 0 {
		needed++
	}
	return max(partSize, needed)
}

// RangedWrite reports whether Write fetches this object through
// WriteOptions.Ranges. Every ranged part is held in the node memory budget, so
// objects whose parts cannot fit it keep the streaming path.
func (s *S3) RangedWrite(size int64) bool {
	if s.rangedMinSize < 0 || s.uploadMemory == nil || size < s.rangedMinSize {
		return false
	}
	if size <= s.partSize && size <= min(maxMemoryPart, s.maxBufferedBytes) {
		return false
	}
	partSize := s.multipartPartSize(size, true)
	return size > partSize && partSize <= maxMemoryPart && partSize <= s.uploadMemory.capacity
}

// uploadRangedParts gives every part slot its own source range, so no single
// sequential read limits the object. PartConcurrency slots are guaranteed and
// wait for memory in FIFO order; extra slots are borrowed only while the node
// is idle and memory is immediately free, and return when their part ends.
func (s *S3) uploadRangedParts(ctx context.Context, key string, ranges RangeFunc, size, partSize int64, uploadID *string) ([]types.CompletedPart, error) {
	uploadContext, cancel := context.WithCancel(ctx)
	defer cancel()
	count := int((size + partSize - 1) / partSize)
	parts := make([]types.CompletedPart, count)
	slots := newRangedSlots(s.partLender, s.partConcurrency, count, s.uploadMemory)
	var group sync.WaitGroup
	var failure sync.Once
	var firstError error
	fail := func(err error) {
		if err != nil {
			failure.Do(func() { firstError = err; cancel() })
		}
	}
	for number := 1; number <= count; number++ {
		offset := int64(number-1) * partSize
		length := min(partSize, size-offset)
		release, err := slots.acquire(uploadContext, length)
		if err != nil {
			fail(err)
			break
		}
		group.Add(1)
		go func(number int32, offset, length int64) {
			defer group.Done()
			defer slots.done()
			defer release()
			etag, err := s.uploadRange(uploadContext, key, ranges, uploadID, number, offset, length)
			if err != nil {
				fail(err)
				return
			}
			parts[number-1] = types.CompletedPart{ETag: etag, PartNumber: aws.Int32(number)}
		}(int32(number), offset, length)
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

// uploadRange buffers the range before sending it, so SDK retries replay the
// same bytes. Interrupted body reads are retried with a fresh conditional
// range; a changed source, a length mismatch and cancellation are not.
func (s *S3) uploadRange(ctx context.Context, key string, ranges RangeFunc, uploadID *string, number int32, offset, length int64) (*string, error) {
	data := make([]byte, int(length))
	if err := readRangeRetrying(ctx, ranges, offset, data); err != nil {
		return nil, err
	}
	output, err := s.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: uploadID, PartNumber: aws.Int32(number), Body: bytes.NewReader(data), ContentLength: aws.Int64(length)})
	if err != nil {
		return nil, s3Error(err)
	}
	if aws.ToString(output.ETag) == "" {
		return nil, errors.New("S3 returned an empty ETag for part " + strconv.Itoa(int(number)))
	}
	return output.ETag, nil
}

// readRangeRetrying fills data from one range. An interrupted body is read
// again with a fresh conditional range, at most rangeReadAttempts times; a
// changed source, a length mismatch and cancellation are final.
func readRangeRetrying(ctx context.Context, ranges RangeFunc, offset int64, data []byte) error {
	for attempt := 1; ; attempt++ {
		err := readRange(ctx, ranges, offset, data)
		var interrupted rangeBodyError
		if err == nil || attempt >= rangeReadAttempts || ctx.Err() != nil || !errors.As(err, &interrupted) {
			return err
		}
	}
}

// rangeBodyError marks a transport failure after a range was opened.
type rangeBodyError struct{ err error }

func (e rangeBodyError) Error() string { return e.err.Error() }
func (e rangeBodyError) Unwrap() error { return e.err }

func readRange(ctx context.Context, ranges RangeFunc, offset int64, data []byte) error {
	input, err := ranges(ctx, offset, int64(len(data)))
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("range source returned a nil reader")
	}
	stop := interruptInput(ctx, input)
	defer stop()
	defer input.Close()
	reader := contextReader{ctx, input}
	// Unlike io.ReadFull, keep a clean early EOF (a short source) apart from a
	// truncated HTTP body (io.ErrUnexpectedEOF), which is worth another read.
	for read := 0; read < len(data) && err == nil; {
		var n int
		n, err = reader.Read(data[read:])
		read += n
		if errors.Is(err, io.EOF) {
			err = nil
			if read < len(data) {
				return fmt.Errorf("%w: source range shorter than declared length", ErrSizeMismatch)
			}
		}
	}
	if err == nil {
		err = requireEOF(reader)
	}
	return rangeReadError(ctx, err)
}

// rangeReadError marks a failure while reading an opened range as worth
// another conditional read, unless it is final: cancellation, a changed or
// missing source, or a length the source cannot satisfy.
func rangeReadError(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil || errors.Is(err, ErrSourceChanged) || errors.Is(err, ErrSizeMismatch) || errors.Is(err, ErrNotFound) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return rangeBodyError{err}
}

// OpenRanges pins the object with a one-byte conditional GET. It returns the
// same metadata as Open and proves the provider honours Range before any part
// depends on it; a provider that ignores Range yields ErrUnsupported.
func (s *S3) OpenRanges(ctx context.Context, key string, options OpenOptions) (RangeSource, Object, error) {
	if err := s3Key(key, false); err != nil {
		return nil, Object{}, err
	}
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Range: aws.String("bytes=0-0")}
	if expected := options.Expected; expected != nil {
		if expected.Key != key {
			return nil, Object{}, ErrSourceChanged
		}
		if expected.ETag != "" {
			input.IfMatch = aws.String("\"" + unquoteETag(expected.ETag) + "\"")
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
	_ = out.Body.Close()
	start, end, total, ok := contentRange(aws.ToString(out.ContentRange))
	if !ok || start != 0 || end != 0 || aws.ToString(out.ETag) == "" {
		return nil, Object{}, fmt.Errorf("%w: provider did not serve a conditional byte range", ErrUnsupported)
	}
	object := Object{Key: key, Size: total, ETag: aws.ToString(out.ETag), VersionID: aws.ToString(out.VersionId), ModifiedAt: aws.ToTime(out.LastModified), ContentType: aws.ToString(out.ContentType), ContentEncoding: aws.ToString(out.ContentEncoding), ContentDisposition: aws.ToString(out.ContentDisposition), CacheControl: aws.ToString(out.CacheControl), StorageClass: string(out.StorageClass), ExpiresAt: aws.ToTime(out.Expires), LifecycleExpiresAt: lifecycleExpiry(aws.ToString(out.Expiration)), Metadata: copyMetadata(out.Metadata)}
	if err = checkExpected(object, options.Expected); err != nil {
		return nil, Object{}, err
	}
	return &s3Ranges{s: s, key: key, etag: object.ETag, version: object.VersionID, size: total}, object, nil
}

// s3Ranges pins later ranges to the probed ETag (and version), even when the
// caller's expectation only had a modification time.
type s3Ranges struct {
	s                  *S3
	key, etag, version string
	size               int64
}

func (r *s3Ranges) Close() error { return nil }

func (r *s3Ranges) OpenRange(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length <= 0 || offset > r.size-length {
		return nil, errors.New("source range is outside the object")
	}
	end := offset + length - 1
	input := &s3.GetObjectInput{Bucket: aws.String(r.s.bucket), Key: aws.String(r.key), Range: aws.String("bytes=" + strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(end, 10)), IfMatch: aws.String("\"" + unquoteETag(r.etag) + "\"")}
	if r.version != "" {
		input.VersionId = aws.String(r.version)
	}
	out, err := r.s.client.GetObject(ctx, input)
	if err != nil {
		return nil, s3Error(err)
	}
	start, last, total, ok := contentRange(aws.ToString(out.ContentRange))
	if !EqualETag(aws.ToString(out.ETag), r.etag) || (r.version != "" && aws.ToString(out.VersionId) != r.version) || (ok && total != r.size) {
		_ = out.Body.Close()
		return nil, ErrSourceChanged
	}
	if !ok || start != offset || last != end || (out.ContentLength != nil && *out.ContentLength != length) {
		_ = out.Body.Close()
		return nil, fmt.Errorf("%w: provider returned bytes %d-%d instead of %d-%d", ErrSizeMismatch, start, last, offset, end)
	}
	return cancellable(ctx, out.Body, nil), nil
}

// contentRange parses "bytes start-end/total"; an unknown total is rejected.
func contentRange(value string) (start, end, total int64, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(value), "bytes ")
	if !found {
		return 0, 0, 0, false
	}
	span, size, found := strings.Cut(rest, "/")
	first, last, split := strings.Cut(span, "-")
	if !found || !split {
		return 0, 0, 0, false
	}
	var err1, err2, err3 error
	start, err1 = strconv.ParseInt(first, 10, 64)
	end, err2 = strconv.ParseInt(last, 10, 64)
	total, err3 = strconv.ParseInt(size, 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, false
	}
	return start, end, total, true
}
