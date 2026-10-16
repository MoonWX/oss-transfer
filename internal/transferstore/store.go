// Package transferstore implements object I/O. It does not own task scheduling,
// durable completion records, or retrying an object after process termination.
package transferstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

var (
	ErrNotFound      = errors.New("object not found")
	ErrInvalidKey    = errors.New("invalid object key")
	ErrInvalidCursor = errors.New("invalid listing cursor")
	ErrSourceChanged = errors.New("source changed")
	ErrSizeMismatch  = errors.New("object size mismatch")
	ErrUnsupported   = errors.New("unsupported storage option")
)

// ETag is an opaque version fingerprint, not a content hash. For a local file it
// includes identity, size, and modification time; it is not a filesystem snapshot.
type Object struct {
	Key                string            `json:"key"`
	Size               int64             `json:"size"`
	ModifiedAt         time.Time         `json:"modifiedAt"`
	ETag               string            `json:"etag,omitempty"`
	VersionID          string            `json:"versionId,omitempty"`
	ContentType        string            `json:"contentType,omitempty"`
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	StorageClass       string            `json:"storageClass,omitempty"`
	ExpiresAt          time.Time         `json:"expiresAt,omitempty"`
	LifecycleExpiresAt time.Time         `json:"lifecycleExpiresAt,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

type ListOptions struct {
	Prefix string
	// StartAfter excludes keys less than or equal to this bytewise key. Keep
	// the same value when continuing a cursor; it is part of the cursor scope.
	StartAfter string
	Cursor     string
	Limit      int
}

type Page struct {
	Objects    []Object `json:"objects"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

type OpenOptions struct {
	// Expected is the prior LIST/HEAD result. A mismatch fails before returning
	// data. S3 additionally sends If-Match (and VersionId when available).
	Expected *Object
}

type WriteOptions struct {
	// Size must be exact and nonnegative. Unknown-length streams are rejected.
	Size               int64
	Overwrite          bool
	ContentType        string
	ContentEncoding    string
	ContentDisposition string
	CacheControl       string
	Metadata           map[string]string
}

type WriteResult struct {
	Object  Object
	Skipped bool
	// Committed can be true with an error if publication succeeded but a later
	// durability check failed. The caller must not treat this as an absent object.
	Committed bool
}

type Store interface {
	List(context.Context, ListOptions) (Page, error)
	// Scan streams discovery with synchronous callback backpressure. Transfer
	// execution must use this instead of repeatedly paging a local filesystem.
	Scan(context.Context, ListOptions, func(Object) error) error
	Stat(context.Context, string) (Object, error)
	Open(context.Context, string, OpenOptions) (io.ReadCloser, Object, error)
	// Write never closes input on success. Cancellation closes input if it
	// implements io.Closer, so an in-flight read can be interrupted. Otherwise
	// input itself must honor cancellation; an arbitrary blocking io.Reader
	// cannot safely be interrupted by this package.
	Write(context.Context, string, io.Reader, WriteOptions) (WriteResult, error)
	Close() error
}

func pageLimit(limit int) (int, error) {
	if limit == 0 {
		return 1000, nil
	}
	if limit < 1 || limit > 1000 {
		return 0, fmt.Errorf("listing limit must be between 1 and 1000")
	}
	return limit, nil
}

type cursor struct {
	Kind, Scope, Prefix, StartAfter, Position string
}

func makeCursor(kind, scope, prefix, startAfter, position string) string {
	data, _ := json.Marshal(cursor{kind, scope, prefix, startAfter, position})
	return base64.RawURLEncoding.EncodeToString(data)
}

func readCursor(value, kind, scope, prefix, startAfter string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(value) > 32768 {
		return "", ErrInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", ErrInvalidCursor
	}
	var c cursor
	if json.Unmarshal(data, &c) != nil || c.Kind != kind || c.Scope != scope || c.Prefix != prefix || c.StartAfter != startAfter || c.Position == "" {
		return "", ErrInvalidCursor
	}
	return c.Position, nil
}

// EqualETag ignores only the enclosing HTTP quotes. Some S3 providers omit
// them in LIST XML while returning them in HEAD/GET headers. The opaque value
// remains case-sensitive, including multipart suffixes.
func EqualETag(left, right string) bool {
	if left == right {
		return true
	}
	if left == "" || right == "" {
		return false
	}
	return unquoteETag(left) == unquoteETag(right)
}

func unquoteETag(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}

func checkExpected(actual Object, expected *Object) error {
	if expected == nil {
		return nil
	}
	if actual.Key != expected.Key || actual.Size != expected.Size ||
		(expected.ETag != "" && !EqualETag(actual.ETag, expected.ETag)) ||
		(expected.VersionID != "" && actual.VersionID != expected.VersionID) ||
		(expected.ETag == "" && expected.VersionID == "" && !expected.ModifiedAt.IsZero() && !actual.ModifiedAt.Equal(expected.ModifiedAt)) {
		return ErrSourceChanged
	}
	return nil
}

func copyMetadata(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	if cancelled := r.ctx.Err(); cancelled != nil {
		return n, cancelled
	}
	return n, err
}

func interruptInput(ctx context.Context, input io.Reader) func() {
	closer, ok := input.(io.Closer)
	if !ok {
		return func() {}
	}
	stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
	return func() { stop() }
}

// requireEOF rejects both short and oversized sources before destination commit.
func requireEOF(reader io.Reader) error {
	var one [1]byte
	n, err := io.ReadFull(reader, one[:])
	if n != 0 {
		return fmt.Errorf("%w: source exceeds declared length", ErrSizeMismatch)
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

type cancelReadCloser struct {
	ctx    context.Context
	reader io.ReadCloser
	stop   func() bool
	onEOF  func() error
	once   sync.Once
	err    error
}

func cancellable(ctx context.Context, reader io.ReadCloser, onEOF func() error) io.ReadCloser {
	result := &cancelReadCloser{ctx: ctx, reader: reader, onEOF: onEOF}
	result.stop = context.AfterFunc(ctx, result.closeReader)
	return result
}

func (r *cancelReadCloser) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if cancelled := r.ctx.Err(); cancelled != nil {
		return n, cancelled
	}
	if errors.Is(err, io.EOF) && r.onEOF != nil {
		if changed := r.onEOF(); changed != nil {
			return n, changed
		}
	}
	return n, err
}

func (r *cancelReadCloser) Close() error {
	r.stop()
	r.closeReader()
	return r.err
}

func (r *cancelReadCloser) closeReader() {
	r.once.Do(func() { r.err = r.reader.Close() })
}
