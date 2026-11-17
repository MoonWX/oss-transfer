package transferstore

// WritePhase is what the requests of one object write are waiting for.
type WritePhase int

const (
	// PhaseSending: a request is handing its body to the network or waiting
	// for the response.
	PhaseSending WritePhase = iota
	// PhaseRetryWait: a request failed and waits for its next attempt.
	PhaseRetryWait
	// PhaseRequestLimit: a request waits for the node or task request limit.
	PhaseRequestLimit
	// PhaseUploadBuffer: a part waits for the node's upload memory.
	PhaseUploadBuffer
)

// WriteProgress receives the destination-side byte counts of one object
// write (WriteOptions.Progress). Every method is safe for concurrent use and
// never blocks.
//
// Sending and Accepted describe the bytes counted towards the object: bytes
// the destination has confirmed, plus the bytes the current attempt of each
// request in flight has handed to the network. A failed attempt withdraws its
// bytes. Sent describes bytes sent, for the speed: it includes bytes sent
// again by a later attempt and is never withdrawn.
type WriteProgress interface {
	// Sending adds (or, negative, withdraws) request-body bytes the current
	// attempt of a request has handed to the network.
	Sending(delta int64)
	// Sent adds bytes put on the wire, resent bytes included.
	Sent(n int64)
	// Accepted records one confirmed request: withdraw is what it counted
	// with Sending, confirmed is what the destination now holds.
	Accepted(withdraw, confirmed int64)
	// Phase enters (+1) or leaves (-1) a state of one request or part.
	Phase(phase WritePhase, delta int)
}

// ProgressReporter is implemented by stores whose Write reports
// WriteOptions.Progress itself. A caller counts any other store's progress
// from its source reads.
type ProgressReporter interface {
	ReportsProgress() bool
}
