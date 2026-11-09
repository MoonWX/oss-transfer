package transferstore

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// queryString matches the query of a URL in error text. Requests are signed in
// headers, but a query can still carry an upload ID or, on a presigned or
// proxied request, a signature: logs never need it.
var queryString = regexp.MustCompile(`(https?://[^\s"?]*)\?[^\s"]*`)

// LogText shortens an error for one service log line: its first line, without
// URL queries, at most 400 bytes. Storage errors never include the secret key
// (see s3DiagnosticClient), and signatures travel in headers.
func LogText(text string) string {
	text, _, _ = strings.Cut(strings.TrimSpace(text), "\n")
	text = queryString.ReplaceAllString(text, "$1?…")
	const limit = 400
	if len(text) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

// retryLog writes storage retries to the service log, at most one line per
// retryLogInterval: an outage retries every request in flight, so the
// lines that are not written are counted in the next one.
var retryLog = struct {
	sync.Mutex
	write      func(string)
	now        func() time.Time
	last       time.Time
	suppressed int
}{now: time.Now}

const retryLogInterval = 30 * time.Second

// SetRetryLogger directs the retry lines of all S3 clients of this process to
// write, or discards them when write is nil.
func SetRetryLogger(write func(string)) {
	retryLog.Lock()
	defer retryLog.Unlock()
	retryLog.write, retryLog.last, retryLog.suppressed = write, time.Time{}, 0
}

func logRetry(line string) {
	retryLog.Lock()
	defer retryLog.Unlock()
	if retryLog.write == nil {
		return
	}
	now := retryLog.now()
	if !retryLog.last.IsZero() && now.Sub(retryLog.last) < retryLogInterval {
		retryLog.suppressed++
		return
	}
	if retryLog.suppressed > 0 {
		line = fmt.Sprintf("%s (and %d more storage request retries since the last report)", line, retryLog.suppressed)
	}
	retryLog.last, retryLog.suppressed = now, 0
	retryLog.write(line)
}
