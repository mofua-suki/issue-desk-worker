package gitwork

import (
	"context"
	"errors"
	"issue-desk-worker/internal/gitea"
)

// Merge validation never owns or resets the author's saved local branch.
// Every attempt keeps its own worktree, including failed tests and dirty files,
// until WorkspaceCleaner removes the clean trees of requests with a saved result.
func OpenMergeRequest(ctx context.Context, c Config, repo, base, branch, id string) (*Workspace, error) {
	if !gitea.ValidRepo(repo) || !ValidRequestID(id) || !validBranch(branch) || !validBranch(base) {
		return nil, errors.New("マージ依頼の作業枝が不正です")
	}
	return open(ctx, c, repo, base, branch, "request-"+id, true)
}
