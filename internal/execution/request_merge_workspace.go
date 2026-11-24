package execution

import (
	"context"
	"issue-desk-worker/internal/gitwork"
)

func (e *Executor) requestMergeWorkspace(ctx context.Context, r monitorRequest, base, branch string) (*gitwork.Workspace, error) {
	c := e.gitConfig()
	return gitwork.OpenMergeRequest(ctx, c, r.Repo, base, branch, r.ID)
}
