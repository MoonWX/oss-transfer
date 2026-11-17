package transferstore

import (
	"context"
	"io"
	"os"
	"slices"
	"sync"
	"sync/atomic"
)

const (
	// localScanStreamAfter is the listing length past which a directory is
	// examined while it is still being read. Smaller directories are read
	// whole and sorted first, which keeps their cursors exactly as before.
	localScanStreamAfter = 4096
	// localScanStreamBuffer bounds the names one scan reads ahead of its
	// lookups: a million-name directory still lists at the speed of ls.
	localScanStreamBuffer = 64 << 20
	// localScanLookupsWhileListing lets lookups run while a streamed
	// listing is still open. It is the release gate of streaming: if those
	// lookups switch Linux NFSv3 or NFSv4.1 clients to READDIRPLUS and make a
	// million-name directory slower than a sorted scan at 20 ms, set it to
	// false, and lookups start only at the end of the listing or once the
	// buffer is full.
	localScanLookupsWhileListing = true
)

// nameBytes is what a name costs the read-ahead buffer: its bytes and its
// string header.
func nameBytes(name string) int { return len(name) + 16 }

// nameStream reads the rest of a directory's names ahead of the scan, in
// readdir order, holding at most limit bytes of names not yet consumed. Only
// the scan's goroutine consumes; one reader goroutine appends.
type nameStream struct {
	mu      sync.Mutex
	names   []string // names[i] is the name at index base+i
	base    int
	bytes   int
	limit   int
	done    bool  // the listing ended
	err     error // why, unless at its end
	arrived chan struct{}
	room    chan struct{}
	stop    context.CancelFunc
	exited  chan struct{}
	file    *os.File
	reads   listingReads // the reader's, continuing the scan's reads
	// phase is lookupWaitingLimit while the reader waits for the local
	// operation limit before a read, so a scan waiting for its names shows
	// what they wait for.
	phase atomic.Int32
}

// startStream hands the rest of the frame's listing to a reader goroutine.
// The names already read come first; the frame's indexes count from them.
func (scan *localScan) startStream(frame *localScanFrame, file *os.File, names []string, reads listingReads) {
	stream := &nameStream{names: names, limit: scan.streamBuffer, arrived: make(chan struct{}, 1), room: make(chan struct{}, 1),
		exited: make(chan struct{}), file: file, reads: reads}
	for _, name := range names {
		stream.bytes += nameBytes(name)
	}
	ctx, stop := context.WithCancel(scan.ctx)
	stream.stop = stop
	frame.stream, frame.Streamed, frame.listed = stream, true, len(names)
	frame.next, frame.issue, frame.pageKeys = 0, 0, map[string]struct{}{}
	scan.activity.start(ScanModeStreaming)
	go scan.readAhead(ctx, frame.key, stream)
}

func (scan *localScan) readAhead(ctx context.Context, key string, stream *nameStream) {
	defer close(stream.exited)
	for {
		if err := stream.awaitRoom(ctx); err != nil {
			stream.end(err)
			return
		}
		charge := func() error {
			stream.phase.Store(lookupWaitingLimit)
			defer stream.phase.Store(lookupPending)
			return scan.s.localOperation(ctx)
		}
		if err := stream.reads.before(charge); err != nil {
			stream.end(err)
			return
		}
		end := scan.activity.call()
		batch, err := scan.fs.ReadNames(stream.file, localScanNames)
		end()
		if err := stream.reads.after(len(batch), charge); err != nil {
			stream.end(err)
			return
		}
		scan.activity.read(len(batch))
		stream.add(batch)
		if err == io.EOF {
			scan.activity.ended(key)
			stream.end(nil)
			return
		}
		if err != nil {
			stream.end(scanPathError(err, key))
			return
		}
	}
}

func (s *nameStream) awaitRoom(ctx context.Context) error {
	for {
		s.mu.Lock()
		full := s.bytes >= s.limit
		s.mu.Unlock()
		if !full {
			return nil
		}
		select {
		case <-s.room:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *nameStream) add(batch []string) {
	if len(batch) == 0 {
		return
	}
	s.mu.Lock()
	s.names = append(s.names, batch...)
	for _, name := range batch {
		s.bytes += nameBytes(name)
	}
	s.mu.Unlock()
	notify(s.arrived)
}

func (s *nameStream) end(err error) {
	s.mu.Lock()
	s.done, s.err = true, err
	s.mu.Unlock()
	notify(s.arrived)
}

// available is the index after the last name read so far, and whether the
// listing has ended, with the error that ended it early.
func (s *nameStream) available() (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base + len(s.names), s.done, s.err
}

// full reports whether the reader waits for the scan to consume names.
func (s *nameStream) full() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes >= s.limit
}

func (s *nameStream) at(index int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.names[index-s.base]
}

// release drops the names before index, which the scan has consumed, and
// lets the reader read on. The dropped names are cleared at once; the array
// that held them goes when the reader's next append outgrows it.
func (s *nameStream) release(index int) {
	s.mu.Lock()
	if drop := index - s.base; drop > 0 {
		for _, name := range s.names[:drop] {
			s.bytes -= nameBytes(name)
		}
		clear(s.names[:drop])
		s.names, s.base = s.names[drop:], index
	}
	s.mu.Unlock()
	notify(s.room)
}

// close stops the reader, waits for a directory read in progress to return,
// and closes the listing.
func (s *nameStream) close() {
	s.stop()
	<-s.exited
	_ = s.file.Close()
}

// awaitName waits until the name at the frame's head has been read. It
// reports false at the end of the listing, and the error that ended it
// early. While lookups wait for the end of the listing (see
// localScanLookupsWhileListing), it also waits for that end or a full
// buffer.
func (scan *localScan) awaitName(frame *localScanFrame) (bool, error) {
	stream := frame.stream
	for {
		available, done, err := stream.available()
		ready := frame.next < available && (scan.lookupsWhileListing || done || stream.full())
		switch {
		case ready:
			return true, nil
		case done && frame.next >= available:
			return false, err
		}
		stop := scan.activity.await(&stream.phase)
		select {
		case <-stream.arrived:
			stop()
		case <-scan.ctx.Done():
			stop()
			return false, scan.ctx.Err()
		}
	}
}

// finishStream ends a streamed frame's listing. Its files have been
// examined; the subdirectories it deferred are now taken in sorted order, as
// in a sorted directory, each with the identity its lookup found.
func (scan *localScan) finishStream(frame *localScanFrame) {
	frame.listed, _, _ = frame.stream.available()
	frame.stream.close()
	frame.stream = nil
	slices.SortFunc(frame.deferred, func(a, b deferredDirectory) int {
		switch {
		case a.name < b.name:
			return -1
		case a.name > b.name:
			return 1
		}
		return 0
	})
	frame.deferred = slices.CompactFunc(frame.deferred, func(a, b deferredDirectory) bool { return a.name == b.name })
	frame.names = make([]string, len(frame.deferred))
	frame.deferredIDs = make([]lookupIdentity, len(frame.deferred))
	for i, d := range frame.deferred {
		frame.names[i], frame.deferredIDs[i] = d.name, d.id
	}
	frame.deferred, frame.pageKeys = nil, nil
	frame.next, frame.issue, frame.read = 0, 0, true
	frame.ring, frame.done = nil, localDoneAnswers{}
	scan.activity.start(ScanModeSorted)
	names, next := frame.progress()
	scan.activity.enter(frame.key, false, names, next)
}

// deferredDirectory is a subdirectory found while its parent streams, entered
// once the parent's listing has ended. It keeps only its lookup's identity,
// so a directory of millions of subdirectories holds their names and little
// more.
type deferredDirectory struct {
	name string
	id   lookupIdentity
}
