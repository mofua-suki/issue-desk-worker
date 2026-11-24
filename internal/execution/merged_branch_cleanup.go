package execution

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
)

// The merge itself keeps the head branch (delete_branch_after_merge=false) because
// recovery re-reads the PR and matches its head ref. The branch is removed only
// after the result is saved: a PR job just before its workspace is cleaned up, a
// manual merge request once its result is recorded.
const mergedBranchCleanupTimeout = 10 * time.Second
const openPullPages = 20

func branchRoute(repo, branch string) string {
	return "/repos/" + repo + "/branches/" + strings.ReplaceAll(url.PathEscape(branch), "%2F", "/")
}

// deleteMergedBranch is the manual merge request path: a kept branch costs only a
// ref, so every failure is logged and ignored and the saved result never changes.
// Issue branches are left to the job's workspace cleanup.
func (e *Executor) deleteMergedBranch(ctx context.Context, repo string, number int64, branch string) {
	if strings.HasPrefix(branch, "codex/issue-") {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, mergedBranchCleanupTimeout)
	defer cancel()
	if step, err := e.removeMergedBranch(ctx, repo, number, branch); err != nil {
		mergedBranchFailure(number, step)
	}
}

func mergedBranchFailure(number int64, step string) {
	// Neither remote bodies, repo names nor branch names enter this log.
	fmt.Fprintf(os.Stderr, "PR%dのマージ済みブランチを消せませんでした（%s）\n", number, step)
}

// removeMergedBranch deletes the work branch of a merged PR when it still points at
// the merged head and no other open PR uses it as head or base. The Git lease
// protects the ref if it changes after the Gitea checks.
func (e *Executor) removeMergedBranch(ctx context.Context, repo string, number int64, branch string) (string, error) {
	if number < 1 || branch == "" || !gitea.ValidRepo(repo) {
		return "", nil
	}
	pull, err := e.Gitea.Pull(ctx, repo, number)
	if err != nil {
		return "PRの確認", err
	}
	if pull.Number != number || !pull.Merged || pull.Head.Ref != branch || !strings.EqualFold(pull.Head.Repo.FullName, repo) || !strings.EqualFold(pull.Base.Repo.FullName, repo) || !gitwork.ValidSHA(pull.Head.SHA) {
		return "", nil
	}
	info, err := e.Gitea.Repo(ctx, repo)
	if err != nil {
		return "リポジトリの確認", err
	}
	if info.DefaultBranch == "" || strings.EqualFold(branch, info.DefaultBranch) || strings.EqualFold(branch, pull.Base.Ref) {
		return "", nil
	}
	inUse := false
	err = e.Gitea.EachPull(ctx, repo, "open", openPullPages, func(other gitea.Pull) (bool, error) {
		inUse = other.Number != number && (other.Head.Ref == branch && strings.EqualFold(other.Head.Repo.FullName, repo) || other.Base.Ref == branch)
		return inUse, nil
	})
	if errors.Is(err, gitea.ErrListIncomplete) {
		return "開いたPRの確認", errors.New("開いたPRが多すぎます")
	}
	if err != nil {
		return "開いたPRの確認", err
	}
	if inUse {
		return "", nil
	}
	route := branchRoute(repo, branch)
	var current struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := e.Gitea.Request(ctx, "GET", route, nil, &current); err != nil {
		if notFound(err) {
			return "", nil
		}
		return "ブランチの確認", err
	}
	// Commits pushed after the merge are not in the default branch; keep them.
	if current.Commit.ID != pull.Head.SHA {
		return "", nil
	}
	if err := gitwork.DeleteRemoteBranchAt(ctx, e.gitConfig(), repo, branch, pull.Head.SHA); err != nil {
		return "ブランチの削除", err
	}
	return "", nil
}

func notFound(err error) bool {
	var refusal *gitea.Error
	return errors.As(err, &refusal) && refusal.Status == 404
}

// transientGitea reports failures worth retrying on a later cycle. Refusals such
// as a protected branch, malformed responses and too many open PRs would never
// clear and must not hold the workspace forever.
func transientGitea(err error) bool {
	var refusal *gitea.Error
	if errors.As(err, &refusal) {
		return refusal.Status >= 500 || refusal.Status == 408 || refusal.Status == 429
	}
	return errors.Is(err, gitea.ErrUnreachable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
