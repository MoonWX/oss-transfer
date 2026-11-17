package transferstore

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// sentFlushBytes batches the gross bytes a meter reports to Sent, which feeds
// the task speed and takes a lock there.
const sentFlushBytes = 256 << 10

type meterState int

const (
	meterIdle meterState = iota
	meterAdmitting
	meterSending
	meterRetrying
)

func (s meterState) phase() (WritePhase, bool) {
	switch s {
	case meterAdmitting:
		return PhaseRequestLimit, true
	case meterSending:
		return PhaseSending, true
	case meterRetrying:
		return PhaseRetryWait, true
	}
	return 0, false
}

// requestMeter counts the body of one PutObject or UploadPart call per HTTP
// attempt. s3DiagnosticClient.Do, the one place every attempt passes after
// it was signed, starts a generation for the attempt, and another for each
// body net/http asks GetBody for to send it again on a new connection. Only
// the current generation's reads count towards the object; every read counts
// as sent. A failed attempt withdraws its bytes; when the call ends, accept
// confirms them or abandon withdraws them, and later reads count as sent only.
//
// The bytes counted are those the transport has taken from the body. The
// difference to bytes that left the host (TLS and socket buffers) is not
// measured; the destination's confirmation settles it.
type requestMeter struct {
	progress WriteProgress
	mu       sync.Mutex
	gen      uint64
	counted  int64 // counted towards the object in the current generation
	unsent   int64 // read but not yet reported to Sent
	state    meterState
	closed   bool
}

type requestMeterKey struct{}

// meterRequest returns ctx carrying a new meter for one upload call, or ctx
// and a nil meter when the write counts no progress. A nil meter's methods
// do nothing.
func meterRequest(ctx context.Context, progress WriteProgress) (context.Context, *requestMeter) {
	if progress == nil {
		return ctx, nil
	}
	meter := &requestMeter{progress: progress}
	return context.WithValue(ctx, requestMeterKey{}, meter), meter
}

func requestMeterFrom(ctx context.Context) *requestMeter {
	meter, _ := ctx.Value(requestMeterKey{}).(*requestMeter)
	return meter
}

// RequestAdmitted is called by a request limiter below the S3 client when it
// lets an upload request go: the request no longer waits for the limit.
func RequestAdmitted(ctx context.Context) {
	if meter := requestMeterFrom(ctx); meter != nil {
		meter.admitted()
	}
}

func (m *requestMeter) setStateLocked(state meterState) {
	if m.state == state {
		return
	}
	if phase, ok := m.state.phase(); ok {
		m.progress.Phase(phase, -1)
	}
	if phase, ok := state.phase(); ok {
		m.progress.Phase(phase, 1)
	}
	m.state = state
}

func (m *requestMeter) withdrawLocked() {
	if m.counted != 0 {
		m.progress.Sending(-m.counted)
		m.counted = 0
	}
}

// begin starts the generation of a new attempt, withdrawing what the last
// one counted. An attempt that may still wait for the request limit starts
// in that state.
func (m *requestMeter) begin(admission bool) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gen++
	if m.closed {
		return m.gen
	}
	m.withdrawLocked()
	if admission {
		m.setStateLocked(meterAdmitting)
	} else {
		m.setStateLocked(meterSending)
	}
	return m.gen
}

func (m *requestMeter) admitted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed && m.state == meterAdmitting {
		m.setStateLocked(meterSending)
	}
}

// read counts n body bytes the transport took in generation gen. The check
// and the count are one step, so a read racing a new attempt can never be
// counted in the new attempt's generation.
func (m *requestMeter) read(gen uint64, n int) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	m.unsent += int64(n)
	if !m.closed && gen == m.gen {
		m.counted += int64(n)
		m.progress.Sending(int64(n))
		// Reading the body means the request was let go.
		if m.state == meterAdmitting {
			m.setStateLocked(meterSending)
		}
	}
	var sent int64
	if m.unsent >= sentFlushBytes || m.closed {
		sent, m.unsent = m.unsent, 0
	}
	m.mu.Unlock()
	if sent > 0 {
		m.progress.Sent(sent)
	}
}

// failed withdraws an attempt that returned an error or a status of 300 or
// more; reads its body still sees, such as net/http writing the rest of a
// body the server already rejected, count as sent only. The request waits
// for a retry until the next attempt begins or the call ends.
func (m *requestMeter) failed() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gen++
	if m.closed {
		return
	}
	m.withdrawLocked()
	m.setStateLocked(meterRetrying)
}

// finish ends the call: a confirmed call turns its counted bytes into length
// confirmed bytes, any other withdraws them.
func (m *requestMeter) finish(confirmed bool, length int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	if confirmed {
		m.progress.Accepted(m.counted, length)
		m.counted = 0
	} else {
		m.withdrawLocked()
	}
	m.setStateLocked(meterIdle)
	sent := m.unsent
	m.unsent = 0
	m.mu.Unlock()
	if sent > 0 {
		m.progress.Sent(sent)
	}
}

type meteredBody struct {
	io.ReadCloser
	meter *requestMeter
	gen   uint64
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.meter.read(b.gen, n)
	return n, err
}

// meterBody installs the meter of an upload call on one HTTP attempt of it:
// the body, and any body GetBody returns to send it again, count in their
// own generation. Other requests are returned unchanged with a nil meter.
func meterBody(request *http.Request) *requestMeter {
	meter := requestMeterFrom(request.Context())
	if meter == nil || request.Method != http.MethodPut || request.Body == nil || request.Body == http.NoBody || request.ContentLength <= 0 {
		return nil
	}
	request.Body = &meteredBody{ReadCloser: request.Body, meter: meter, gen: meter.begin(true)}
	if getBody := request.GetBody; getBody != nil {
		request.GetBody = func() (io.ReadCloser, error) {
			body, err := getBody()
			if err != nil {
				return nil, err
			}
			return &meteredBody{ReadCloser: body, meter: meter, gen: meter.begin(false)}, nil
		}
	}
	return meter
}
