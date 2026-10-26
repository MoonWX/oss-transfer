package transferstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Smithy rewinds streams for SDK retries, but its HTTP request builder does not
// set GetBody. The HTTP transport also needs it when an HTTP/2 GOAWAY or a lost
// connection requires replaying a request within the same SDK attempt.
func addS3RequestReplay(stack *middleware.Stack) error {
	return stack.Finalize.Add(s3RequestReplay{}, middleware.After)
}

type s3RequestReplay struct{}

func (s3RequestReplay) ID() string { return "S3RequestReplay" }

func (s3RequestReplay) HandleFinalize(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
	switch middleware.GetOperationName(ctx) {
	case "PutObject", "UploadPart":
	default:
		return next.HandleFinalize(ctx, in)
	}
	request, ok := in.Request.(*smithyhttp.Request)
	if !ok || request.Method != http.MethodPut || request.ContentLength <= 0 {
		return next.HandleFinalize(ctx, in)
	}
	stream, ok := request.GetStream().(interface {
		io.ReaderAt
		io.Seeker
	})
	if !ok {
		// Streaming/checksum wrappers without random access cannot be replayed
		// this way. Do not buffer them or change their retry behavior.
		return next.HandleFinalize(ctx, in)
	}
	// This runs after signing and the SDK's per-attempt rewind. A source may
	// start at a nonzero offset; seeking to zero would upload different bytes.
	start, err := stream.Seek(0, io.SeekCurrent)
	if err != nil {
		return middleware.FinalizeOutput{}, middleware.Metadata{}, fmt.Errorf("S3 request replay position: %w", err)
	}
	source := &s3ReplaySource{reader: stream, start: start, length: request.ContentLength}
	request.GetBody = source.body
	// As with Smithy's original body, finish outstanding reads before the
	// caller releases its buffer/file, even if the transport closes bodies
	// asynchronously. Drop references retained by the request's GetBody too.
	defer source.close()
	return next.HandleFinalize(ctx, in)
}

type s3ReplaySource struct {
	mu            sync.Mutex
	reader        io.ReaderAt
	start, length int64
	bodies        []*s3ReplayBody
}

func (s *s3ReplaySource) body() (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reader == nil {
		return nil, io.ErrClosedPipe
	}
	// ReadAt leaves the source cursor untouched. Each replay owns its own
	// offset and uses the already-budgeted memory buffer or spool file.
	body := &s3ReplayBody{reader: io.NewSectionReader(s.reader, s.start, s.length)}
	s.bodies = append(s.bodies, body)
	return body, nil
}

func (s *s3ReplaySource) close() {
	s.mu.Lock()
	s.reader = nil
	bodies := s.bodies
	s.bodies = nil
	s.mu.Unlock()
	for _, body := range bodies {
		body.Close()
	}
}

// The transport can close a body while its write goroutine is still running.
// Closing a replay must stop further reads without closing the shared spool
// file, which remains owned by the upload until the SDK operation returns.
type s3ReplayBody struct {
	mu     sync.Mutex
	reader *io.SectionReader
}

func (b *s3ReplayBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reader == nil {
		return 0, io.EOF
	}
	return b.reader.Read(p)
}

func (b *s3ReplayBody) Close() error {
	b.mu.Lock()
	b.reader = nil
	b.mu.Unlock()
	return nil
}
