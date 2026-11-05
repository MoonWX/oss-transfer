package master

import (
	"context"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// rangedSupportTTL bounds how long a positive capability answer is reused.
// A negative answer is never reused: an Agent upgraded since then must
// receive the task's choice on its next delivery.
const rangedSupportTTL = time.Minute

// rangedDownloadSupport remembers Agents that advertised
// agentservice.RangedDownloadProtocol. Agents released before it reject an
// unknown job field, and they already stream every object, which is what a
// task's "off" asks for; such Agents receive the job without the field.
type rangedDownloadSupport struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (r *rangedDownloadSupport) supported(ctx context.Context, node task.Node, query func(context.Context, task.Node) (int, error)) (bool, error) {
	now := time.Now()
	r.mu.Lock()
	if until, ok := r.until[node.ID]; ok && now.Before(until) {
		r.mu.Unlock()
		return true, nil
	}
	r.mu.Unlock()
	protocol, err := query(ctx, node)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if protocol < agentservice.RangedDownloadProtocol {
		delete(r.until, node.ID)
		return false, nil
	}
	if r.until == nil {
		r.until = map[string]time.Time{}
	}
	r.until[node.ID] = now.Add(rangedSupportTTL)
	return true, nil
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
// sends child_accept through it, so an older Agent never sees the field.
func (t *taskRuntime) acceptRequest(ctx context.Context, p task.Parent, child task.Child) (agentservice.ChildAcceptRequest, error) {
	request := childAcceptance(p, child)
	if child.Job.RangedDownload == "" {
		return request, nil
	}
	supported, err := t.rangedSupport.supported(ctx, child.Node, t.rangedDownloadProtocol)
	if err != nil || supported {
		return request, err
	}
	return withoutRangedDownload(request), nil
}

// withoutRangedDownload removes the choice from the job and from a takeover
// seed, which carries its own copy of the job.
func withoutRangedDownload(request agentservice.ChildAcceptRequest) agentservice.ChildAcceptRequest {
	request.Job.RangedDownload = ""
	if request.Seed != nil {
		seed := *request.Seed
		seed.Job.RangedDownload = ""
		request.Seed = &seed
	}
	return request
}
