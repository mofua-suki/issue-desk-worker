package gitwork

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
)

// TargetIncluded checks the fetched target branch against the actual worktree HEAD.
// A missing remote branch is a valid first publication.
func (w *Workspace) TargetRelation(ctx context.Context, required bool) (string, error) {
	ref := "refs/remotes/origin/" + w.Branch
	out, err := w.Config.Run(ctx, w.Tree, "show-ref", "--verify", "--quiet", ref)
	if err != nil {
		return "", err
	}
	if out.Code == 1 {
		if required {
			return "", errors.New("対象枝を確認できません")
		}
		return "included", nil
	}
	if out.Code != 0 {
		return "", errors.New("対象枝を確認できません")
	}
	out, err = w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", ref, "HEAD")
	if err != nil {
		return "", err
	}
	switch out.Code {
	case 0:
		return "included", nil
	case 1:
		out, err = w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", "HEAD", ref)
		if err != nil {
			return "", err
		}
		if out.Code == 0 {
			return "behind", nil
		}
		if out.Code == 1 {
			return "diverged", nil
		}
		return "", errors.New("対象枝と作業ツリーの関係を確認できません")
	default:
		return "", errors.New("対象枝と作業ツリーの関係を確認できません")
	}
}

// ErrWorkspaceUncommitted indicates that the worktree has uncommitted changes.
var ErrWorkspaceUncommitted = errors.New("作業ツリーに未コミットの変更があります")

// CheckTargetClean distinguishes uncommitted changes from a failed status check.
func (w *Workspace) CheckTargetClean(ctx context.Context) error {
	out, err := w.Config.Run(ctx, w.Tree, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil || out.Code != 0 {
		return cause.New("作業ツリーの状態を確認できません", err)
	}
	if out.Out != "" {
		return ErrWorkspaceUncommitted
	}
	return nil
}

// FastForwardTarget updates only a clean worktree and refuses non-fast-forward updates.
func (w *Workspace) FastForwardTarget(ctx context.Context) error {
	if err := w.CheckTargetClean(ctx); err != nil {
		return err
	}
	args := []string{"merge", "--ff-only", "refs/remotes/origin/" + w.Branch}
	out, err := w.Config.Run(ctx, w.Tree, args...)
	if err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("対象枝へ追従できません: %w", CommandError(args, out))
	}
	return nil
}
