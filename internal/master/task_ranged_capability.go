package master

import (
	"context"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// rangedSupportTTL bounds how long a capability answer is reused. A stale
// answer is safe either way: a job without the field streams (see
// execution.Job.RangedDownload), and a field sent to an Agent that no longer
// understands it only fails deliveries until the answer expires.
const rangedSupportTTL = time.Minute

type rangedAnswer struct {
	supported bool
	until     time.Time
}

// rangedDownloadSupport remembers which Agents advertised
// agentservice.RangedDownloadProtocol.
type rangedDownloadSupport struct {
	mu      sync.Mutex
	answers map[string]rangedAnswer
}

func (r *rangedDownloadSupport) supported(ctx context.Context, node task.Node, query func(context.Context, task.Node) (int, error)) (bool, error) {
	now := time.Now()
	r.mu.Lock()
	if answer, ok := r.answers[node.ID]; ok && now.Before(answer.until) {
		r.mu.Unlock()
		return answer.supported, nil
	}
	r.mu.Unlock()
	protocol, err := query(ctx, node)
	if err != nil {
		return false, err
	}
	supported := protocol >= agentservice.RangedDownloadProtocol
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.answers == nil {
		r.answers = map[string]rangedAnswer{}
	}
	r.answers[node.ID] = rangedAnswer{supported: supported, until: now.Add(rangedSupportTTL)}
	return supported, nil
}

func (t *taskRuntime) rangedDownloadProtocol(ctx context.Context, node task.Node) (int, error) {
	var caps struct {
		RangedDownloadProtocol int `json:"rangedDownloadProtocol"`
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := t.server.agentCall(callCtx, nodeSession(node), "capabilities", false, nil, &caps)
	return caps.RangedDownloadProtocol, err
}

// acceptRequest is childAcceptance for the child's current node. Every
// delivery path (dispatch, observation, node add, takeover and rebalance)
// sends child_accept through it.
//
// For a LOCAL destination an Agent ranges only on an explicit "auto"; a job
// without the field streams. A current Agent therefore receives the task's
// choice spelled out ("auto" or "off"), while an Agent that predates the
// field, and would reject it, receives none and streams, which is all it
// can do anyway. A child it accepted keeps streaming after that Agent is
// upgraded, so an "off" choice can never be lost to a rolling upgrade.
func (t *taskRuntime) acceptRequest(ctx context.Context, p task.Parent, child task.Child) (agentservice.ChildAcceptRequest, error) {
	request := childAcceptance(p, child)
	if child.Job.Destination.Kind != "LOCAL" {
		return request, nil
	}
	supported, err := t.rangedSupport.supported(ctx, child.Node, t.rangedDownloadProtocol)
	if err != nil {
		return request, err
	}
	choice := ""
	if supported {
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
