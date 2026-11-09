package master

import (
	"context"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// rangedSupportTTL bounds how long a positive capability answer is reused. A
// field sent to an Agent that no longer understands it (a downgrade) only
// fails deliveries until the answer expires.
const rangedSupportTTL = time.Minute

// deliveryProtocols are the Agent capabilities a child delivery depends on.
type deliveryProtocols struct {
	RangedDownloadProtocol int `json:"rangedDownloadProtocol"`
	SourceRefreshProtocol  int `json:"sourceRefreshProtocol"`
}

// deliverySupport is what a node's Agent understands of a child delivery:
// the job's rangedDownload field, and sourceRefresh (see
// agentservice.SourceRefreshProtocol).
type deliverySupport struct {
	ranged, refresh bool
}

// rangedDownloadSupport remembers Agents that advertised
// agentservice.RangedDownloadProtocol or agentservice.SourceRefreshProtocol.
// Negative answers are not kept: a child delivered without the ranged field
// streams for good (see execution.Job), so an Agent upgraded in place must be
// asked again on its next delivery rather than be served a remembered
// "unsupported". Only an Agent that is still old is asked on every delivery.
// A slow query that started before an upgrade cannot erase a newer positive
// answer, because nothing negative is stored.
type rangedDownloadSupport struct {
	mu      sync.Mutex
	until   map[string]time.Time
	refresh map[string]time.Time
}

func (r *rangedDownloadSupport) supported(ctx context.Context, node task.Node, query func(context.Context, task.Node) (deliveryProtocols, error)) (deliverySupport, error) {
	now := time.Now()
	r.mu.Lock()
	ranged, rangedKnown := r.until[node.ID]
	refresh, refreshKnown := r.refresh[node.ID]
	r.mu.Unlock()
	if rangedKnown && now.Before(ranged) && refreshKnown && now.Before(refresh) {
		return deliverySupport{ranged: true, refresh: true}, nil
	}
	protocols, err := query(ctx, node)
	if err != nil {
		return deliverySupport{}, err
	}
	support := deliverySupport{ranged: protocols.RangedDownloadProtocol >= agentservice.RangedDownloadProtocol, refresh: protocols.SourceRefreshProtocol >= agentservice.SourceRefreshProtocol}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.until == nil {
		r.until, r.refresh = map[string]time.Time{}, map[string]time.Time{}
	}
	if support.ranged {
		r.until[node.ID] = now.Add(rangedSupportTTL)
	}
	if support.refresh {
		r.refresh[node.ID] = now.Add(rangedSupportTTL)
	}
	return support, nil
}

// rangedPass memoizes one answer per node, positive, negative or failed, for
// one delivery or observation pass, so an Agent that is still old is asked
// once per pass rather than before every child; the next pass asks again and
// so notices an upgrade. It is safe for concurrent use by node groups.
type rangedPass struct {
	t       *taskRuntime
	mu      sync.Mutex
	answers map[string]deliverySupport
	failed  map[string]error
}

func (t *taskRuntime) newRangedPass() *rangedPass {
	return &rangedPass{t: t, answers: map[string]deliverySupport{}, failed: map[string]error{}}
}

func (r *rangedPass) supported(ctx context.Context, node task.Node) (deliverySupport, error) {
	r.mu.Lock()
	answer, known := r.answers[node.ID]
	failure := r.failed[node.ID]
	r.mu.Unlock()
	if known || failure != nil {
		return answer, failure
	}
	query := r.t.deliveryProtocols
	if r.t.rangedQuery != nil {
		query = r.t.rangedQuery
	}
	answer, err := r.t.rangedSupport.supported(ctx, node, query)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.failed[node.ID] = err
		return deliverySupport{}, err
	}
	r.answers[node.ID] = answer
	return answer, nil
}

func (t *taskRuntime) deliveryProtocols(ctx context.Context, node task.Node) (deliveryProtocols, error) {
	var caps deliveryProtocols
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := t.server.agentCall(callCtx, nodeSession(node), "capabilities", false, nil, &caps)
	return caps, err
}

// acceptRequest is childAcceptance for the child's current node. Every
// delivery path (dispatch, observation, node add, takeover and rebalance)
// sends child_accept through it.
//
// For a LOCAL destination an Agent ranges only on an explicit "auto"; a job
// without the field streams. A current Agent therefore receives the task's
// choice spelled out ("auto" or "off"), while an Agent that predates the
// field, and would reject it, receives none and streams, which is all it
// can do anyway. Any child delivered without the field keeps streaming,
// including after that Agent is upgraded, so an "off" choice can never be
// lost to a rolling upgrade; an "auto" child ranges again after the task is
// stopped and resumed, which delivers the choice anew.
//
// An Agent that advertises source refresh is told that this Master accepts
// refreshed source pins. The Agent is asked only when that can matter, for a
// child with an object that failed because its source changed (or for a
// LOCAL destination, whose answer is needed anyway). Without it (an older
// Agent, a capability query that failed, or a failure the Master has not
// observed yet) the delivery still goes ahead, and that Agent retries a
// changed source the way older releases do.
func (t *taskRuntime) acceptRequest(ctx context.Context, pass *rangedPass, p task.Parent, child task.Child) (agentservice.ChildAcceptRequest, error) {
	request := childAcceptance(p, child)
	local := child.Job.Destination.Kind == "LOCAL"
	if !local && !sourceChanged(child.Record) {
		return request, nil
	}
	support, err := pass.supported(ctx, child.Node)
	request.SourceRefresh = err == nil && support.refresh
	if !local {
		return request, nil
	}
	if err != nil {
		return request, err
	}
	choice := ""
	if support.ranged {
		choice = execution.RangedDownloadAuto
		if child.Job.RangedDownload == execution.RangedDownloadOff {
			choice = execution.RangedDownloadOff
		}
	}
	return withRangedDownload(request, choice), nil
}

// withRangedDownload sets the choice on the job and on a takeover seed,
// which carries its own copy of the job.
func withRangedDownload(request agentservice.ChildAcceptRequest, choice string) agentservice.ChildAcceptRequest {
	request.Job.RangedDownload = choice
	if request.Seed != nil {
		seed := *request.Seed
		seed.Job.RangedDownload = choice
		request.Seed = &seed
	}
	return request
}

// sourceChanged reports an object of the record whose source changed after
// discovery, which a retry reads again only where the Master allows it.
func sourceChanged(record *execution.Record) bool {
	if record == nil {
		return false
	}
	for _, result := range record.Results {
		if result.Reconciliation == execution.ReasonSourceChanged {
			return true
		}
	}
	return false
}
