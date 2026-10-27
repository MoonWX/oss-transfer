package execution

import (
	"context"
	"io"
	"sort"
	"sync"
	"sync/atomic"
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
	ItemID           string `json:"itemId"`
	SourceKey        string `json:"sourceKey"`
	DestinationKey   string `json:"destinationKey"`
	Size             int64  `json:"size"`
	TransferredBytes int64  `json:"transferredBytes"`
	Stage            string `json:"stage"`
}

type objectProgress struct {
	item  ObjectProgress
	order int
	bytes atomic.Int64
}

type attemptProgress struct {
	ctx     context.Context
	mu      sync.Mutex
	ready   bool
	child   ChildProgress
	order   map[string]int
	objects map[string]*objectProgress
}

func newAttemptProgress(ctx context.Context, record Record) *attemptProgress {
	p := &attemptProgress{
		ctx: ctx,
		child: ChildProgress{ChildID: record.Job.ChildID, Number: record.Job.Number,
			Generation: record.Job.Generation, Assignment: record.Job.Assignment, Attempt: record.Attempt},
		order: make(map[string]int, len(record.Job.Items)), objects: make(map[string]*objectProgress),
	}
	for index, item := range record.Job.Items {
		p.order[item.ID] = index
	}
	return p
}

func (p *attemptProgress) initialize(succeeded int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.child.SucceededBytes, p.ready = succeeded, true
}

func (p *attemptProgress) begin(item Item) *objectProgress {
	object := &objectProgress{item: ObjectProgress{ItemID: item.ID, SourceKey: item.Source.Key,
		DestinationKey: item.DestinationKey, Size: item.Source.Size}, order: p.order[item.ID]}
	p.mu.Lock()
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
			child.InFlightBytes += min(object.item.Size, max(int64(0), object.bytes.Load()))
		}
	}
	var items []ObjectProgress
	if details {
		sort.Slice(objects, func(i, j int) bool { return objects[i].order < objects[j].order })
		items = make([]ObjectProgress, 0, len(objects))
		for _, object := range objects {
			item := object.item
			item.TransferredBytes = min(item.Size, max(int64(0), object.bytes.Load()))
			item.Stage = "TRANSFERRING"
			if item.TransferredBytes == item.Size {
				item.Stage = "FINALIZING"
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
		r.progress.bytes.Add(int64(n))
	}
	return n, err
}
