package execution

import "context"

func (e *Engine) jobContext(job Job) (context.Context, error) {
	if e.opts.JobContext != nil {
		return e.opts.JobContext(job)
	}
	if e.opts.ParentContext != nil {
		return e.opts.ParentContext(job.ParentID, job.Generation)
	}
	return e.ctx, nil
}
