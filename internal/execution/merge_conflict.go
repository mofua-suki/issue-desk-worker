package execution

import (
	"context"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

// resumeMergeConflict is called only after merge has returned its turn, or
// while resuming a persisted merge. Keep the checkpoint before implementation.
func (e *Executor) resumeMergeConflict(ctx context.Context, d Descriptor, r *journal.Record, w *gitwork.Workspace, body *string) (bool, error) {
	paths, err := w.Unmerged(ctx)
	if err != nil {
		return false, err
	}
	if len(paths) == 0 {
		return false, nil
	}
	active := e.controls.get(d.ID)
	if active != nil {
		active.mu.Lock()
		active.mergeStarted = false
		active.mu.Unlock()
	}
	r.ControllerAllowed = paths
	clearFollowedResults(r)
	*body = withConflictInstruction(*body)
	if err := e.checkpoint(ctx, d, r, "implement"); err != nil {
		return false, err
	}
	return true, nil
}
