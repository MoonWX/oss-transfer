package comparison

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// File-backed results (Protocol 2).
//
// A comparison owns <Options.Directory>/<first 32 hex digits of sha256(ID)>/
// rows.jsonl: one JSON Row per line, numbered 1..Rows in key order. Batches are
// appended, fsynced and then committed by one bbolt transaction that writes the
// record (counts, checkpoint and the committed file length) together with the
// index blocks describing the new rows. That transaction is the only commit
// point: readers never look past the committed length, and recovery truncates
// whatever a crash left after it.

const (
	runsBucket       = "comparison_runs"
	blocksBucket     = "comparison_blocks"
	runtimeBucket    = "comparison_runtime"
	legacyBucket     = "comparisons"
	legacyRowsBucket = "comparison_rows"

	resultsFile = "rows.jsonl"
	// blockRows consecutive rows form one index block.
	blockRows = 1024
	// A batch is committed once it holds flushRows rows or flushBytes bytes,
	// and at least every flushInterval while the comparison runs.
	flushRows     = 1024
	flushBytes    = 256 << 10
	flushInterval = time.Second
	// Scan budgets of one Rows call.
	scanBlocks     = 64
	legacyScanRows = 65536
	// Retention counts each legacy bbolt row as this many bytes.
	legacyRowBytes = 512
	maxDetailBytes = 512
	maxDetailKey   = 256
)

// Fixed sentences; the Master localizes them.
const (
	errorFailed      = "Comparison could not finish; check storage access and retry."
	errorSave        = "Cannot save comparison results"
	errorUnavailable = "Comparison results are unavailable on this node"
)

var (
	errResultsMissing = errors.New("result file missing")
	errResultsShort   = errors.New("result file is shorter than its committed length")
	errResultsDamaged = errors.New("comparison results are damaged")
)

var resultDirectoryPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// resultDirectory names the private result directory of a comparison ID.
func resultDirectory(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

func blockKey(id string, index int64) string { return fmt.Sprintf("%s/%012d", id, index) }

// runRecord is the comparison_runs value: the public Progress plus the state
// needed to commit, recover, resume and delete file-backed results.
type runRecord struct {
	Progress
	// Committed is the length of rows.jsonl covered by this record. Bytes after
	// it were never committed.
	Committed int64 `json:"committed"`
	// Directory is the result directory name under Options.Directory.
	Directory string `json:"directory,omitempty"`
	// AutoResume marks an INTERRUPTED comparison the next start resumes.
	AutoResume bool `json:"autoResume,omitempty"`
	// UncleanResumes counts automatic resumes after a stop that recorded no
	// terminal state. It is cleared once a run commits progress and by every
	// terminal state.
	UncleanResumes int `json:"uncleanResumes,omitempty"`
	// CheckpointBytes keeps the exact checkpoint when it is not valid UTF-8;
	// JSON would otherwise replace the invalid bytes.
	CheckpointBytes []byte `json:"checkpointBytes,omitempty"`
	// Deleting hides the record while its results are removed.
	Deleting bool `json:"deleting,omitempty"`
	// Legacy marks the tombstone of a pre-Protocol 2 record whose rows are
	// still being removed from comparison_rows.
	Legacy bool `json:"legacy,omitempty"`
	// Unavailable marks results whose file is missing or damaged.
	Unavailable bool `json:"unavailable,omitempty"`
}

func (r *runRecord) checkpoint() string {
	if r.CheckpointBytes != nil {
		return string(r.CheckpointBytes)
	}
	return r.Checkpoint
}

func (r *runRecord) setCheckpoint(key string) {
	r.Checkpoint, r.CheckpointBytes = key, nil
	if !utf8.ValidString(key) {
		r.CheckpointBytes = []byte(key)
	}
}

// resultBlock indexes rows First..Last, stored at Offset..Offset+Length of the
// result file. Block b holds rows b*blockRows+1 through (b+1)*blockRows.
type resultBlock struct {
	Offset   int64            `json:"offset"`
	Length   int64            `json:"length"`
	First    int64            `json:"first"`
	Last     int64            `json:"last"`
	FirstKey string           `json:"firstKey"`
	LastKey  string           `json:"lastKey"`
	Counts   map[string]int64 `json:"counts"`
}

type indexedBlock struct {
	index int64
	resultBlock
}

// resultError is a failure of this package whose detail is safe to show: it
// names the operation and the local cause, never a path or credential.
type resultError struct {
	detail string
	err    error
}

func (e *resultError) Error() string { return e.detail }
func (e *resultError) Unwrap() error { return e.err }

func localFailure(operation string, err error) error {
	return &resultError{detail: clip(operation+": "+localCause(err), maxDetailBytes), err: err}
}

// localCause describes a file or database error without the path it names.
func localCause(err error) string {
	var pathError *fs.PathError
	if errors.As(err, &pathError) {
		return pathError.Op + ": " + pathError.Err.Error()
	}
	var syscallError *os.SyscallError
	if errors.As(err, &syscallError) {
		return syscallError.Err.Error()
	}
	return err.Error()
}

// failureDetail is Progress.ErrorDetail for a failed run.
func failureDetail(err error) string {
	var local *resultError
	if errors.As(err, &local) {
		return local.detail
	}
	return clip(ErrorDetail(err), maxDetailBytes)
}

func clip(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit], "")
}

func countRow(p *Progress, status string) {
	p.Compared++
	switch status {
	case StatusMatch:
		p.Matched++
	case StatusDifferent:
		p.Different++
	case StatusETagMismatch:
		p.ETagMismatch++
	case StatusMissingTarget:
		p.Missing++
	case StatusExtraTarget:
		p.Extra++
	case StatusChanged:
		p.Changed++
	case StatusBothMissing:
		p.BothMissing++
	default:
		p.Errors++
	}
}

func terminalState(status string) bool {
	switch status {
	case StateCompleted, StateFailed, StateCanceled, StateInterrupted:
		return true
	}
	return false
}

type pendingRow struct {
	number int64
	key    string
	status string
	length int64
}

// resultWriter owns the result file of the running comparison. emit (Run's
// goroutine), the periodic flush and Cancel share it under mu.
type resultWriter struct {
	m *Manager
	// startAfter is the exact checkpoint a resumed run lists after.
	startAfter string
	// hook runs inside every commit transaction (tests inject failures).
	hook func(*runRecord) error

	mu   sync.Mutex
	file *os.File
	// live counts every emitted row; committed is the last committed record.
	live, committed runRecord
	pending         []byte
	pendingRows     []pendingRow
	tail            indexedBlock
	hasTail         bool
	// storedKey is the key of the last row accepted for storing.
	storedKey   string
	failed      error
	skipRepeat  bool
	freeWarned  bool
	bytesRead   atomic.Int64
	elapsedBase int64
	started     time.Time
	// startCompared is Compared when this run began.
	startCompared int64
}

// openWriter opens the result file of rec. A fresh file is created empty;
// otherwise the file must hold at least the committed length and anything
// after it is truncated.
func (m *Manager) openWriter(rec runRecord, fresh bool) (*resultWriter, error) {
	if !resultDirectoryPattern.MatchString(rec.Directory) {
		return nil, errResultsMissing
	}
	directory := filepath.Join(m.dir, rec.Directory)
	path := filepath.Join(directory, resultsFile)
	var file *os.File
	created := false
	if fresh || rec.Committed == 0 {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return nil, err
		}
		flags := os.O_RDWR | os.O_CREATE
		if fresh {
			flags |= os.O_TRUNC
		}
		var err error
		if file, err = os.OpenFile(path, flags, 0600); err != nil {
			return nil, err
		}
		created = true
	} else {
		var err error
		file, err = os.OpenFile(path, os.O_RDWR, 0600)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errResultsMissing
		}
		if err != nil {
			return nil, err
		}
	}
	if err := truncateResults(file, rec.Committed); err != nil {
		_ = file.Close()
		return nil, err
	}
	if created {
		// The record committed next refers to this file; make its directory
		// entries durable first.
		if err := errors.Join(syncDirectory(directory), syncDirectory(m.dir)); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	w := &resultWriter{m: m, file: file, live: rec, committed: rec, hook: m.commitHook}
	if rec.Rows > 0 {
		index := (rec.Rows - 1) / blockRows
		err := m.db.View(func(tx *state.Tx) error {
			found, err := tx.Get(blocksBucket, blockKey(rec.ID, index), &w.tail.resultBlock)
			if err == nil && (!found || w.tail.Last != rec.Rows) {
				err = errResultsDamaged
			}
			return err
		})
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		w.tail.index, w.hasTail, w.storedKey = index, true, w.tail.LastKey
	}
	return w, nil
}

// truncateResults discards bytes after the committed length. A file shorter
// than that lost committed rows.
func truncateResults(file *os.File, committed int64) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < committed {
		return errResultsShort
	}
	if info.Size() == committed {
		return nil
	}
	if err := file.Truncate(committed); err != nil {
		return err
	}
	return file.Sync()
}

// begin sets the record a (re)started run continues from.
func (w *resultWriter) begin(rec runRecord) {
	w.live, w.committed = rec, rec
	w.bytesRead.Store(rec.BytesRead)
	// Options.Now without UTC keeps the monotonic reading of time.Now, so
	// elapsed time does not follow wall-clock adjustments.
	w.elapsedBase, w.started = rec.ElapsedMillis, w.m.options.Now()
	w.startCompared = rec.Compared
	if rec.Compared > 0 && rec.Scope == "prefix" {
		w.startAfter, w.skipRepeat = rec.checkpoint(), true
	}
}

// elapsedMillis is the running time of all runs of the comparison.
func (w *resultWriter) elapsedMillis() int64 {
	return w.elapsedBase + max(0, w.m.options.Now().Sub(w.started).Milliseconds())
}

func (w *resultWriter) addBytes(n int64) {
	if n > 0 {
		w.bytesRead.Add(n)
	}
}

// emit counts a row and buffers it for storing. Run calls it in ascending key
// order; a resumed run may repeat the checkpoint key once (the empty key
// cannot be excluded by StartAfter).
func (w *resultWriter) emit(row Row) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed != nil {
		return w.failed
	}
	if w.live.Compared > 0 {
		checkpoint := w.live.checkpoint()
		if row.Key <= checkpoint {
			if w.skipRepeat && row.Key == checkpoint {
				w.skipRepeat = false
				return nil
			}
			return &resultError{detail: fmt.Sprintf("comparison rows are not in ascending key order after %q", clip(checkpoint, maxDetailKey))}
		}
	}
	w.skipRepeat = false
	countRow(&w.live.Progress, row.Status)
	w.live.setCheckpoint(row.Key)
	if row.Status != StatusMatch && !w.live.RowsTruncated {
		row.Number = w.live.Rows + 1
		line, err := json.Marshal(row)
		if err != nil {
			return w.failLocked(localFailure("encode comparison row", err))
		}
		line = append(line, '\n')
		if w.committed.Committed+int64(len(w.pending))+int64(len(line)) > w.m.options.BudgetBytes {
			w.truncateLocked(TruncatedStorageBudget)
		} else {
			w.pending = append(w.pending, line...)
			w.pendingRows = append(w.pendingRows, pendingRow{number: row.Number, key: row.Key, status: row.Status, length: int64(len(line))})
			w.live.Rows, w.storedKey = row.Number, row.Key
		}
	}
	if len(w.pendingRows) >= flushRows || len(w.pending) >= flushBytes {
		return w.flushLocked()
	}
	return nil
}

// truncateLocked stops storing rows for the rest of the comparison. Counting
// continues exactly.
func (w *resultWriter) truncateLocked(reason string) {
	w.live.RowsTruncated, w.live.TruncatedReason, w.live.LastStoredKey = true, reason, w.storedKey
}

func (w *resultWriter) failLocked(err error) error {
	if w.failed == nil {
		w.failed = err
	}
	return w.failed
}

// flushLocked appends the buffered rows, fsyncs them and commits the live
// record with the blocks they touch. It also runs without rows to persist
// counts, checkpoint, bytes read and elapsed time.
func (w *resultWriter) flushLocked() error {
	if w.failed != nil {
		return w.failed
	}
	if len(w.pending) > 0 {
		free, err := w.m.options.FreeBytes(w.m.dir)
		if err != nil {
			if !w.freeWarned {
				w.freeWarned = true
				w.m.logf("comparison %s: cannot check free disk space: %s", w.live.ID, localCause(err))
			}
		} else if free < uint64(w.m.options.MinimumFreeBytes)+uint64(len(w.pending)) {
			// This batch would leave too little space: its rows are only counted.
			w.pending, w.pendingRows = w.pending[:0], w.pendingRows[:0]
			w.live.Rows, w.storedKey = w.committed.Rows, ""
			if w.hasTail {
				w.storedKey = w.tail.LastKey
			}
			w.truncateLocked(TruncatedLowDiskSpace)
		}
	}
	offset := w.committed.Committed
	if len(w.pending) > 0 {
		if _, err := w.file.WriteAt(w.pending, offset); err != nil {
			return w.failLocked(localFailure("write comparison results", err))
		}
		if err := w.file.Sync(); err != nil {
			return w.failLocked(localFailure("sync comparison results", err))
		}
	}
	if w.live.Compared > w.startCompared {
		// This run made progress: a later crash is not a restart loop.
		w.live.UncleanResumes = 0
	}
	next := w.live
	now := w.m.now()
	next.Committed = offset + int64(len(w.pending))
	next.StoredBytes = next.Committed
	next.BytesRead = w.bytesRead.Load()
	next.ElapsedMillis = w.elapsedMillis()
	next.UpdatedAt = now
	blocks := w.blocksFor(offset)
	err := w.m.db.Update(func(tx *state.Tx) error {
		if w.hook != nil {
			if err := w.hook(&next); err != nil {
				return err
			}
		}
		for _, block := range blocks {
			if err := tx.Put(blocksBucket, blockKey(next.ID, block.index), block.resultBlock); err != nil {
				return err
			}
		}
		if err := tx.Put(runsBucket, next.ID, next); err != nil {
			return err
		}
		if terminalState(next.Status) {
			return releaseActive(tx, next.ID)
		}
		return nil
	})
	if err != nil {
		return w.failLocked(localFailure("commit comparison results", err))
	}
	if len(blocks) > 0 {
		w.tail, w.hasTail = blocks[len(blocks)-1], true
	}
	w.pending, w.pendingRows = w.pending[:0], w.pendingRows[:0]
	w.committed = next
	w.live.Committed, w.live.StoredBytes, w.live.BytesRead, w.live.ElapsedMillis, w.live.UpdatedAt = next.Committed, next.StoredBytes, next.BytesRead, next.ElapsedMillis, next.UpdatedAt
	return nil
}

// blocksFor returns the blocks the buffered rows change, starting from a copy
// of the committed last block. A failed commit leaves w.tail untouched.
func (w *resultWriter) blocksFor(offset int64) []indexedBlock {
	var blocks []indexedBlock
	position := offset
	for _, row := range w.pendingRows {
		index := (row.number - 1) / blockRows
		if len(blocks) == 0 || blocks[len(blocks)-1].index != index {
			if len(blocks) == 0 && w.hasTail && w.tail.index == index {
				block := w.tail
				block.Counts = maps.Clone(w.tail.Counts)
				blocks = append(blocks, block)
			} else {
				blocks = append(blocks, indexedBlock{index: index, resultBlock: resultBlock{Offset: position, First: row.number, FirstKey: row.key, Counts: map[string]int64{}}})
			}
		}
		block := &blocks[len(blocks)-1]
		block.Length += row.length
		block.Last, block.LastKey = row.number, row.key
		block.Counts[row.status]++
		position += row.length
	}
	return blocks
}

// flushPeriodically commits at least every interval until stop is closed. A
// failed commit aborts the run.
func (w *resultWriter) flushPeriodically(interval time.Duration, stop <-chan struct{}, abort func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			w.mu.Lock()
			err := w.flushLocked()
			w.mu.Unlock()
			if err != nil {
				abort(err)
				return
			}
		}
	}
}

// committedProgress is the latest committed state of the running comparison.
func (w *resultWriter) committedProgress() Progress {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.m.public(w.committed)
}

func releaseActive(tx *state.Tx, id string) error {
	var active string
	if _, err := tx.Get(runtimeBucket, "active", &active); err != nil {
		return err
	}
	if active != id {
		return nil
	}
	return tx.Delete(runtimeBucket, "active")
}

// statusFilter matches rows by status. A JSON-encoded Row contains the exact
// byte sequence "status":"<status>" only in its status field: every quote
// inside a string value is escaped. The needle avoids decoding skipped rows.
type statusFilter map[string][]byte

func newStatusFilter(statuses []string) statusFilter {
	if len(statuses) == 0 {
		return nil
	}
	filter := statusFilter{}
	for _, status := range statuses {
		filter[status] = []byte(`"status":"` + status + `"`)
	}
	return filter
}

func (f statusFilter) block(counts map[string]int64) bool {
	for status := range f {
		if counts[status] > 0 {
			return true
		}
	}
	return false
}

func (f statusFilter) line(line []byte) bool {
	for _, needle := range f {
		if bytes.Contains(line, needle) {
			return true
		}
	}
	return false
}

// readResultRows collects rows after q.After from the committed blocks. It
// returns the number of the last examined row.
func readResultRows(path string, blocks []indexedBlock, q RowsQuery, filter statusFilter, maxBytes int) ([]Row, int64, error) {
	items, last := []Row{}, q.After
	if len(blocks) == 0 {
		return items, last, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	size := 0
	var reader *bufio.Reader
	var long []byte
	for _, block := range blocks {
		if block.First != block.index*blockRows+1 || block.Last < block.First || block.Last-block.First >= blockRows {
			return nil, 0, errResultsDamaged
		}
		if filter != nil && !filter.block(block.Counts) {
			last = block.Last
			continue
		}
		section := io.NewSectionReader(file, block.Offset, block.Length)
		if reader == nil {
			reader = bufio.NewReaderSize(section, 64<<10)
		} else {
			reader.Reset(section)
		}
		for number := block.First; number <= block.Last; number++ {
			var line []byte
			line, long, err = readLine(reader, long)
			if err != nil {
				return nil, 0, fmt.Errorf("%w: row %d: %v", errResultsDamaged, number, err)
			}
			if number <= q.After {
				continue
			}
			if filter == nil || filter.line(line) {
				if len(items) >= q.Limit || len(items) > 0 && size+len(line) > maxBytes {
					return items, last, nil
				}
				var row Row
				if err := json.Unmarshal(line, &row); err != nil || row.Number != number {
					return nil, 0, fmt.Errorf("%w: row %d", errResultsDamaged, number)
				}
				if _, wanted := filter[row.Status]; filter == nil || wanted {
					items = append(items, row)
					size += len(line)
				}
			}
			last = number
		}
	}
	return items, last, nil
}

// readLine returns the next '\n'-terminated line. It is valid until the next
// read; lines longer than the reader's buffer are assembled in long.
func readLine(reader *bufio.Reader, long []byte) ([]byte, []byte, error) {
	line, err := reader.ReadSlice('\n')
	if err != bufio.ErrBufferFull {
		return line, long, err
	}
	long = append(long[:0], line...)
	for err == bufio.ErrBufferFull {
		line, err = reader.ReadSlice('\n')
		long = append(long, line...)
	}
	return long, long, err
}

var errStopScan = errors.New("stop scan")

// legacyResultRows pages rows of a comparison stored before Protocol 2.
func legacyResultRows(tx *state.Tx, q RowsQuery, filter statusFilter, maxBytes int) ([]Row, int64, error) {
	items, last, size := []Row{}, q.After, 0
	err := tx.Scan(legacyRowsBucket, q.ID+"/", fmt.Sprintf("%s/%020d", q.ID, q.After), legacyScanRows, func(_ string, raw json.RawMessage) error {
		var row Row
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		if _, wanted := filter[row.Status]; filter == nil || wanted {
			if len(items) >= q.Limit || len(items) > 0 && size+len(raw)+1 > maxBytes {
				return errStopScan
			}
			items = append(items, row)
			size += len(raw) + 1
		}
		last = row.Number
		return nil
	})
	if err != nil && !errors.Is(err, errStopScan) {
		return nil, 0, err
	}
	return items, last, nil
}
