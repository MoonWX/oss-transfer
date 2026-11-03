package execution

// Fault-injecting S3 server used by the ranged multipart integrity fuzz tests.
// It serves one versioned "source" object with conditional ranges and accepts
// a multipart upload to any other key. Every stored part is checked against
// the original source range, and Complete records whether the assembled
// object is byte-identical to the original version.

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type fsGet int

const (
	gOK          fsGet = iota
	gFull200           // 200 with the whole object, no Content-Range
	gFull200CR         // 200, chunked whole object, but with the requested Content-Range
	gWrongCR           // 206 with Content-Range and body shifted by one byte
	gNoCR              // 206 without Content-Range
	gWrongTotal        // 206 with Content-Range total off by one
	gTruncate          // Content-Length full, body cut short, connection closed
	gResetHead         // TCP RST before any response
	gResetBody         // TCP RST after part of the body
	gChunkShort        // chunked body cleanly terminated early
	gChunkLong         // chunked body with one extra byte
	gBadLength         // Content-Length one short, body one short
	g500               // InternalError before body
	g503               // SlowDown before body
	g412               // PreconditionFailed regardless of If-Match
	gNoETag            // 206 without ETag
	gSlow              // random delays before and during body
	gShiftedBody       // 206 correct headers, body from offset+length (lying server; undetectable)
	gNumGet
)

var fsGetNames = [...]string{"ok", "full200", "full200-cr", "wrong-cr", "no-cr", "wrong-total", "truncate", "reset-head", "reset-body", "chunk-short", "chunk-long", "bad-length", "500", "503-slowdown", "412", "no-etag", "slow", "shifted-body"}

type fsPut int

const (
	pOK           fsPut = iota
	p500                // InternalError without storing
	p503                // SlowDown after reading body, not stored
	pResetMid           // RST after part of the request body
	pLostResponse       // part stored, then RST instead of a response
	pStored500          // part stored, then 500 (client retries)
	pSlow               // random delay before responding (out-of-order completion)
	pNumPut
)

var fsPutNames = [...]string{"ok", "500", "503-slowdown", "reset-mid", "lost-response", "stored-500", "slow"}

type faultConfig struct {
	GetRate, PutRate float64
	GetKinds         []fsGet // empty: no GET faults
	PutKinds         []fsPut
	ProbeFaults      bool
	MutateAt         int  // mutate the source after this many range GETs; <0 never
	IgnoreIfMatch    bool // serve the mutated version even when If-Match is stale
	MaxSlow          time.Duration
}

type faultS3 struct {
	mu         sync.Mutex
	cfg        faultConfig
	rng        *rand.Rand
	orig       []byte
	origSum    [32]byte
	data       []byte
	etag       string
	partSize   int64
	parts      map[int][]byte
	target     []byte
	targetSum  [32]byte
	completed  int
	aborted    int
	rangeGets  int
	mutated    bool
	violations []string
	counts     map[string]int
	server     *httptest.Server
}

func fsETag(data []byte) string {
	sum := md5.Sum(data)
	return "\"" + hex.EncodeToString(sum[:]) + "\""
}

func fsError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, code)
}

func newFaultS3(data []byte, partSize int64, cfg faultConfig, seed uint64) *faultS3 {
	f := &faultS3{cfg: cfg, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), orig: data, origSum: sha256.Sum256(data), data: data, etag: fsETag(data), partSize: partSize, parts: map[int][]byte{}, counts: map[string]int{}}
	if f.cfg.MaxSlow <= 0 {
		f.cfg.MaxSlow = 20 * time.Millisecond
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *faultS3) Close() { f.server.Close() }

func (f *faultS3) violate(format string, args ...any) {
	f.violations = append(f.violations, fmt.Sprintf(format, args...))
}

func (f *faultS3) Violations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.violations...)
}

func (f *faultS3) Counts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.counts))
	for k, v := range f.counts {
		out[k] = v
	}
	return out
}

func (f *faultS3) State() (completed, aborted int, targetOK bool, mutated bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.completed, f.aborted, f.completed > 0 && f.targetSum == f.origSum, f.mutated
}

func (f *faultS3) sleep() {
	f.mu.Lock()
	d := time.Duration(f.rng.Int64N(int64(f.cfg.MaxSlow)))
	f.mu.Unlock()
	time.Sleep(d)
}

func resetConnection(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("no hijacker")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

func (f *faultS3) serve(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	key := strings.TrimPrefix(req.URL.Path, "/bucket/")
	switch {
	case req.Method == "GET" && key == "source" && !q.Has("uploadId"):
		f.serveSource(w, req)
	case req.Method == "HEAD" && key == "source":
		f.mu.Lock()
		data, etag := f.data, f.etag
		f.mu.Unlock()
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Last-Modified", time.Unix(1700000000, 0).UTC().Format(http.TimeFormat))
	case req.Method == "POST" && q.Has("uploads"):
		f.mu.Lock()
		f.parts = map[int][]byte{}
		f.mu.Unlock()
		fmt.Fprint(w, "<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>"+key+"</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>")
	case req.Method == "PUT" && q.Get("uploadId") != "":
		f.servePart(w, req)
	case req.Method == "POST" && q.Get("uploadId") != "":
		f.serveComplete(w, req)
	case req.Method == "DELETE" && q.Get("uploadId") != "":
		f.mu.Lock()
		f.aborted++
		f.parts = map[int][]byte{}
		f.mu.Unlock()
		w.WriteHeader(204)
	case req.Method == "GET" && q.Get("uploadId") != "":
		fsError(w, 404, "NoSuchUpload")
	default:
		fsError(w, 400, "UnsupportedTestRequest")
	}
}

func (f *faultS3) pickGet(probe bool) fsGet {
	if len(f.cfg.GetKinds) == 0 || (probe && !f.cfg.ProbeFaults) {
		return gOK
	}
	if f.rng.Float64() >= f.cfg.GetRate {
		return gOK
	}
	return f.cfg.GetKinds[f.rng.IntN(len(f.cfg.GetKinds))]
}

func (f *faultS3) pickPut() fsPut {
	if len(f.cfg.PutKinds) == 0 || f.rng.Float64() >= f.cfg.PutRate {
		return pOK
	}
	return f.cfg.PutKinds[f.rng.IntN(len(f.cfg.PutKinds))]
}

func (f *faultS3) serveSource(w http.ResponseWriter, req *http.Request) {
	rangeHeader := req.Header.Get("Range")
	probe := rangeHeader == "bytes=0-0"
	f.mu.Lock()
	if !probe && rangeHeader != "" {
		if f.cfg.MutateAt >= 0 && f.rangeGets == f.cfg.MutateAt && !f.mutated {
			changed := append([]byte(nil), f.data...)
			at := f.rng.IntN(len(changed))
			for i := at; i < len(changed) && i < at+4096; i++ {
				changed[i] ^= 0x5a
			}
			f.data, f.etag, f.mutated = changed, fsETag(changed), true
		}
		f.rangeGets++
	}
	data, etag := f.data, f.etag
	fault := f.pickGet(probe)
	f.counts["get:"+fsGetNames[fault]]++
	partial := f.rng.IntN(1 << 20)
	f.mu.Unlock()

	if match := req.Header.Get("If-Match"); match != "" && match != etag && !f.cfg.IgnoreIfMatch {
		fsError(w, 412, "PreconditionFailed")
		return
	}
	start, end := int64(0), int64(len(data)-1)
	ranged := false
	if rangeHeader != "" {
		first, last, _ := strings.Cut(strings.TrimPrefix(rangeHeader, "bytes="), "-")
		start, _ = strconv.ParseInt(first, 10, 64)
		end, _ = strconv.ParseInt(last, 10, 64)
		ranged = true
	}
	if end >= int64(len(data)) {
		end = int64(len(data)) - 1
	}
	if !ranged {
		switch fault {
		case gFull200, gFull200CR, gWrongCR, gNoCR, gWrongTotal:
			fault = gOK // range-specific faults make no sense on a plain GET
		}
	}
	body := data[start : end+1]
	header := w.Header()
	if fault != gNoETag {
		header.Set("ETag", etag)
	}
	header.Set("Content-Type", "application/x-test")
	header.Set("Last-Modified", time.Unix(1700000000, 0).UTC().Format(http.TimeFormat))
	contentRange := fmt.Sprintf("bytes %d-%d/%d", start, end, len(data))
	switch fault {
	case g500:
		fsError(w, 500, "InternalError")
		return
	case g503:
		fsError(w, 503, "SlowDown")
		return
	case g412:
		fsError(w, 412, "PreconditionFailed")
		return
	case gResetHead:
		resetConnection(w)
		return
	case gFull200:
		header.Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(200)
		_, _ = w.Write(data)
		return
	case gFull200CR:
		header.Set("Content-Range", contentRange)
		w.WriteHeader(200)
		_, _ = w.Write(data)
		return
	case gWrongCR:
		if end+1 < int64(len(data)) {
			header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start+1, end+1, len(data)))
			body = data[start+1 : end+2]
		} else {
			header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start-1, end-1, len(data)))
			body = data[start-1 : end]
		}
		header.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(206)
		_, _ = w.Write(body)
		return
	case gNoCR:
		header.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(206)
		_, _ = w.Write(body)
		return
	case gWrongTotal:
		header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)+1))
		header.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(206)
		_, _ = w.Write(body)
		return
	case gShiftedBody:
		// A lying server: correct headers, but bytes of a different range.
		// Undetectable without checksums; only used when explicitly selected.
		other := (end + 1) % int64(len(data))
		shifted := make([]byte, len(body))
		for i := range shifted {
			shifted[i] = data[(other+int64(i))%int64(len(data))]
		}
		body = shifted
	}
	status := 200
	if ranged {
		status = 206
		header.Set("Content-Range", contentRange)
	}
	switch fault {
	case gChunkShort:
		w.WriteHeader(status)
		_, _ = w.Write(body[:len(body)-1-partial%len(body)])
		return
	case gChunkLong:
		w.WriteHeader(status)
		_, _ = w.Write(body)
		_, _ = w.Write([]byte{0x42})
		return
	case gBadLength:
		header.Set("Content-Length", strconv.Itoa(len(body)-1))
		w.WriteHeader(status)
		_, _ = w.Write(body[:len(body)-1])
		return
	}
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	switch fault {
	case gTruncate:
		_, _ = w.Write(body[:partial%len(body)])
		return
	case gResetBody:
		_, _ = w.Write(body[:partial%len(body)])
		resetConnection(w)
		return
	case gSlow:
		f.sleep()
		half := len(body) / 2
		_, _ = w.Write(body[:half])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		f.sleep()
		_, _ = w.Write(body[half:])
		return
	}
	_, _ = w.Write(body)
}

func (f *faultS3) servePart(w http.ResponseWriter, req *http.Request) {
	number, _ := strconv.Atoi(req.URL.Query().Get("partNumber"))
	f.mu.Lock()
	fault := f.pickPut()
	f.counts["put:"+fsPutNames[fault]]++
	partial := f.rng.IntN(1 << 20)
	f.mu.Unlock()
	switch fault {
	case p500:
		_, _ = io.Copy(io.Discard, req.Body)
		fsError(w, 500, "InternalError")
		return
	case pResetMid:
		_, _ = io.CopyN(io.Discard, req.Body, int64(partial))
		resetConnection(w)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return
	}
	if req.ContentLength >= 0 && int64(len(body)) != req.ContentLength {
		return
	}
	if fault == p503 {
		fsError(w, 503, "SlowDown")
		return
	}
	if fault == pSlow {
		f.sleep()
	}
	f.mu.Lock()
	offset := int64(number-1) * f.partSize
	if number < 1 || offset >= int64(len(f.orig)) {
		f.violate("part %d outside the object (partSize %d)", number, f.partSize)
	} else {
		want := f.orig[offset:min(offset+f.partSize, int64(len(f.orig)))]
		if !bytes.Equal(body, want) {
			f.violate("part %d received %d bytes that differ from source range %d+%d (first diff at %d)", number, len(body), offset, len(want), firstDiff(body, want))
		}
	}
	f.parts[number] = body
	f.mu.Unlock()
	switch fault {
	case pLostResponse:
		resetConnection(w)
		return
	case pStored500:
		fsError(w, 500, "InternalError")
		return
	}
	w.Header().Set("ETag", fsETag(body))
}

func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func (f *faultS3) serveComplete(w http.ResponseWriter, req *http.Request) {
	var complete struct {
		Parts []struct {
			PartNumber int
			ETag       string
		} `xml:"Part"`
	}
	if err := xml.NewDecoder(req.Body).Decode(&complete); err != nil {
		fsError(w, 400, "MalformedXML")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	numbers := make([]int, 0, len(complete.Parts))
	var result []byte
	for index, part := range complete.Parts {
		numbers = append(numbers, part.PartNumber)
		if part.PartNumber != index+1 {
			fsError(w, 400, "InvalidPartOrder")
			return
		}
		stored, ok := f.parts[part.PartNumber]
		if !ok || part.ETag != fsETag(stored) {
			fsError(w, 400, "InvalidPart")
			return
		}
		result = append(result, stored...)
	}
	sort.Ints(numbers)
	f.target = result
	f.targetSum = sha256.Sum256(result)
	f.completed++
	if f.targetSum != f.origSum {
		f.violate("CompleteMultipartUpload assembled %d bytes (want %d) with sha256 %x, want %x; parts %v", len(result), len(f.orig), f.targetSum[:8], f.origSum[:8], numbers)
	}
	fmt.Fprint(w, "<CompleteMultipartUploadResult><ETag>\"multipart\"</ETag></CompleteMultipartUploadResult>")
}
