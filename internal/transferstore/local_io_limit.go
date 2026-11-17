package transferstore

import (
	"context"
	"io"
)

// LocalIOLimiter paces an Agent's use of local storage, so a migration leaves
// a production file system enough capacity for its own workload. It is
// separate from the network limits: a local file operation is an open, a
// create, a metadata lookup or a directory read, and local bytes are the data
// read from and written to local files.
type LocalIOLimiter interface {
	WaitLocalOperation(ctx context.Context) error
	WaitLocalBytes(ctx context.Context, n int) error
}

// SetLocalIOLimiter installs the Agent's local limiter on this store. Without
// one, local access is unlimited.
func (s *Local) SetLocalIOLimiter(limiter LocalIOLimiter) { s.localIO = limiter }

func (s *Local) localOperation(ctx context.Context) error {
	if s.localIO != nil {
		return s.localIO.WaitLocalOperation(ctx)
	}
	return ctx.Err()
}

// localIOChunk bounds one paced read or write, so a large buffer does not
// wait for a whole second's budget before moving any data.
const localIOChunk = 256 << 10

// localPacedReader charges the bytes it reads from a local file to the local
// bandwidth budget before handing them on.
type localPacedReader struct {
	ctx     context.Context
	limiter LocalIOLimiter
	reader  io.ReadCloser
}

func (s *Local) pacedReader(ctx context.Context, reader io.ReadCloser) io.ReadCloser {
	if s.localIO == nil {
		return reader
	}
	return &localPacedReader{ctx: ctx, limiter: s.localIO, reader: reader}
}

func (r *localPacedReader) Read(p []byte) (int, error) {
	if len(p) > localIOChunk {
		p = p[:localIOChunk]
	}
	n, err := r.reader.Read(p)
	if n > 0 {
		if waitErr := r.limiter.WaitLocalBytes(r.ctx, n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

func (r *localPacedReader) Close() error { return r.reader.Close() }

// localPacedWriter waits for the local bandwidth budget before each write to a
// local file.
type localPacedWriter struct {
	ctx     context.Context
	limiter LocalIOLimiter
	writer  io.Writer
}

func (s *Local) pacedWriter(ctx context.Context, writer io.Writer) io.Writer {
	if s.localIO == nil {
		return writer
	}
	return &localPacedWriter{ctx: ctx, limiter: s.localIO, writer: writer}
}

func (w *localPacedWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), localIOChunk)]
		if err := w.limiter.WaitLocalBytes(w.ctx, len(chunk)); err != nil {
			return written, err
		}
		n, err := w.writer.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}
