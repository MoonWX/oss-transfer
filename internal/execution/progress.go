package execution

import (
	"context"
	"io"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// ChildProgress is a volatile observation of one execution attempt. Ownership
// fields let callers reject samples collected before a retry or reassignment.
type ChildProgress struct {
	ChildID        string `json:"childId"`
	Number         int    `json:"number"`
	Generation     uint64 `json:"generation"`
	Assignment     uint64 `json:"assignment"`
	Attempt        uint64 `json:"attempt"`
	ActiveObjects  int    `json:"activeObjects"`
	SucceededBytes int64  `json:"succeededBytes"`
	InFlightBytes  int64  `json:"inFlightBytes"`
}

type ObjectProgress struct {
	ItemID         string `json:"itemId"`
	SourceKey      string `json:"sourceKey"`
	DestinationKey string `json:"destinationKey"`
	Size           int64  `json:"size"`
	// TransferredBytes is what Basis says. TARGET: bytes the destination
	// has confirmed, plus the bytes the current attempt of each request in
	// flight has handed to the network, at most Size-1 until every byte is
	// confirmed. SOURCE: the most bytes read from the source so far.
	TransferredBytes int64  `json:"transferredBytes"`
	Stage            string `json:"stage"`
	// ReadBytes are the bytes this attempt has read from the source; they
	// can run ahead of TransferredBytes (buffered parts and chunks).
	ReadBytes int64 `json:"readBytes,omitempty"`
	// Phase is what the object's requests wait for when none is sending:
	// RETRY_WAIT, WAITING_REQUEST_LIMIT or WAITING_UPLOAD_BUFFER. Empty
	// otherwise, and from older Agents.
	Phase string `json:"phase,omitempty"`
	// Basis is TARGET or SOURCE; older Agents omit it and count SOURCE.
	Basis string `json:"basis,omitempty"`
}

// Progress bases (ObjectProgress.Basis).
const (
	BasisTarget = "TARGET"
	BasisSource = "SOURCE"
)

// Object phases (ObjectProgress.Phase).
const (
	PhaseRetryWait    = "RETRY_WAIT"
	PhaseRequestLimit = "WAITING_REQUEST_LIMIT"
	PhaseUploadBuffer = "WAITING_UPLOAD_BUFFER"
)

// objectProgress counts one object of an attempt. When the target reports
// its own progress (transferstore.ProgressReporter), the object's bytes are
// what the destination has confirmed plus what the requests in flight have
// handed to the network in their current attempt; otherwise they are the
// bytes read from the source.
type objectProgress struct {
	item  ObjectProgress
	order int
	// targetCounted and rate are set before the object is visible and
	// never change. rate feeds the task speed (Options.CountTransferred).
	targetCounted bool
	rate          func(int64)
	// read counts the bytes this attempt read from the source. A retried
	// range withdraws its bytes until it reads them again (progressRange).
	read atomic.Int64
	// peak is the highest read count reported, for a target that does not
	// report its own progress.
	peak atomic.Int64
	// accepted are the bytes the destination has confirmed; pending are the
	// request-body bytes the current attempt of each request in flight has
	// handed to the network. A failed attempt withdraws its pending bytes.
	// One lock keeps a confirmation, which moves a request's bytes from
	// pending to accepted, a single step for every snapshot.
	counts            sync.Mutex
	accepted, pending int64
	// Requests and parts of the write in each state (transferstore.WritePhase).
	sending, retrying, admitting, buffering atomic.Int32
}

func (o *objectProgress) Sending(delta int64) {
	o.counts.Lock()
	o.pending += delta
	o.counts.Unlock()
}

// Sent feeds the task speed (Options.CountTransferred): bytes sent to the
// destination, resent bytes included, or, for a target that does not report
// its own progress, bytes read from the source.
func (o *objectProgress) Sent(n int64) {
	if o.rate != nil && n > 0 {
		o.rate(n)
	}
}

// readFromSource counts n bytes read from the source.
func (o *objectProgress) readFromSource(n int64) {
	o.read.Add(n)
	if !o.targetCounted {
		o.Sent(n)
	}
}

func (o *objectProgress) Accepted(withdraw, confirmed int64) {
	o.counts.Lock()
	o.accepted += confirmed
	o.pending -= withdraw
	o.counts.Unlock()
}

func (o *objectProgress) Phase(phase transferstore.WritePhase, delta int) {
	var counter *atomic.Int32
	switch phase {
	case transferstore.PhaseSending:
		counter = &o.sending
	case transferstore.PhaseRetryWait:
		counter = &o.retrying
	case transferstore.PhaseRequestLimit:
		counter = &o.admitting
	case transferstore.PhaseUploadBuffer:
		counter = &o.buffering
	default:
		return
	}
	counter.Add(int32(delta))
}

// transferred is the object's live byte count, never more than its size.
//
// Counted at the destination, it reaches the size only once the destination
// has confirmed every byte, so FINALIZING means the destination holds all
// data. It falls back when a request fails and its bytes are withdrawn.
//
// Counted from source reads, it is the highest count observed: a retried
// range withdraws its bytes until it reads them again, and reporting the
// peak keeps the progress of one object attempt from moving backwards.
func (o *objectProgress) transferred() int64 {
	size := o.item.Size
	if o.targetCounted {
		o.counts.Lock()
		accepted, pending := o.accepted, o.pending
		o.counts.Unlock()
		if accepted >= size {
			return size
		}
		return min(max(accepted+pending, 0), max(size-1, 0))
	}
	current := min(size, max(int64(0), o.read.Load()))
	for {
		peak := o.peak.Load()
		if current <= peak {
			return peak
		}
		if o.peak.CompareAndSwap(peak, current) {
			return current
		}
	}
}

// phase is the object's Phase (see ObjectProgress): a request sending hides
// another request's wait, and a failed request outranks the request limit,
// which outranks memory.
func (o *objectProgress) phase() string {
	switch {
	case !o.targetCounted || o.sending.Load() > 0:
		return ""
	case o.retrying.Load() > 0:
		return PhaseRetryWait
	case o.admitting.Load() > 0:
		return PhaseRequestLimit
	case o.buffering.Load() > 0:
		return PhaseUploadBuffer
	}
	return ""
}

type attemptProgress struct {
	ctx           context.Context
	mu            sync.Mutex
	ready         bool
	targetCounted bool
	rate          func(int64)
	child         ChildProgress
	order         map[string]int
	objects       map[string]*objectProgress
}

// newAttemptProgress observes one attempt. rate, when set, receives the
// bytes its objects send (objectProgress.Sent).
func newAttemptProgress(ctx context.Context, record Record, rate func(int64)) *attemptProgress {
	p := &attemptProgress{
		ctx: ctx, rate: rate,
		child: ChildProgress{ChildID: record.Job.ChildID, Number: record.Job.Number,
			Generation: record.Job.Generation, Assignment: record.Job.Assignment, Attempt: record.Attempt},
		order: make(map[string]int, len(record.Job.Items)), objects: make(map[string]*objectProgress),
	}
	for index, item := range record.Job.Items {
		p.order[item.ID] = index
	}
	return p
}

// initialize starts the attempt's observations. targetCounted reports
// whether the attempt's target counts the progress of its writes.
func (p *attemptProgress) initialize(succeeded int64, targetCounted bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.child.SucceededBytes, p.targetCounted, p.ready = succeeded, targetCounted, true
}

// reportsProgress reports whether target counts the progress of its writes.
func reportsProgress(target transferstore.Store) bool {
	reporter, ok := target.(transferstore.ProgressReporter)
	return ok && reporter.ReportsProgress()
}

func (p *attemptProgress) begin(item Item) *objectProgress {
	object := &objectProgress{item: ObjectProgress{ItemID: item.ID, SourceKey: item.Source.Key,
		DestinationKey: item.DestinationKey, Size: item.Source.Size}, order: p.order[item.ID]}
	p.mu.Lock()
	object.targetCounted, object.rate = p.targetCounted, p.rate
	p.objects[item.ID] = object
	p.mu.Unlock()
	return object
}

func (p *attemptProgress) finish(object *objectProgress, succeeded bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Removing the live bytes and adding confirmed bytes is one observation
	// boundary, so snapshots cannot count a completed object twice.
	if p.objects[object.item.ItemID] != object {
		return
	}
	delete(p.objects, object.item.ItemID)
	if succeeded {
		p.child.SucceededBytes += object.item.Size
	}
}

func (p *attemptProgress) snapshot(details bool) (ChildProgress, []ObjectProgress, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ready || p.ctx.Err() != nil {
		return ChildProgress{}, nil, false
	}
	child := p.child
	child.ActiveObjects = len(p.objects)
	var objects []*objectProgress
	if details {
		objects = make([]*objectProgress, 0, len(p.objects))
	}
	for _, object := range p.objects {
		if details {
			objects = append(objects, object)
		} else {
			child.InFlightBytes += object.transferred()
		}
	}
	var items []ObjectProgress
	if details {
		sort.Slice(objects, func(i, j int) bool { return objects[i].order < objects[j].order })
		items = make([]ObjectProgress, 0, len(objects))
		for _, object := range objects {
			item := object.item
			item.TransferredBytes = object.transferred()
			item.Stage = "TRANSFERRING"
			if item.TransferredBytes == item.Size {
				item.Stage = "FINALIZING"
			}
			item.ReadBytes = min(item.Size, max(0, object.read.Load()))
			item.Phase = object.phase()
			item.Basis = BasisSource
			if object.targetCounted {
				item.Basis = BasisTarget
			}
			child.InFlightBytes += item.TransferredBytes
			items = append(items, item)
		}
	}
	return child, items, true
}

// Progress never opens a ledger transaction. Its storage is bounded by the
// admitted attempts and their active objects, and is discarded on completion.
func (e *Engine) Progress(parentID int64, generation uint64) []ChildProgress {
	rows := []ChildProgress{}
	e.lifetimes.Range(func(_, value any) bool {
		attempt := value.(*activeAttempt)
		if attempt.parentID == parentID && attempt.generation == generation {
			if row, _, ok := attempt.progress.snapshot(false); ok {
				rows = append(rows, row)
			}
		}
		return true
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].Number < rows[j].Number })
	return rows
}

func (e *Engine) ProgressObjects(parentID int64, generation uint64, childID string) (ChildProgress, []ObjectProgress, bool) {
	value, ok := e.lifetimes.Load(childID)
	if !ok {
		return ChildProgress{}, nil, false
	}
	attempt := value.(*activeAttempt)
	if attempt.parentID != parentID || attempt.generation != generation {
		return ChildProgress{}, nil, false
	}
	return attempt.progress.snapshot(true)
}

type progressReader struct {
	io.ReadCloser
	progress *objectProgress
}

func (r *progressReader) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	if n > 0 {
		r.progress.readFromSource(int64(n))
	}
	return n, err
}
