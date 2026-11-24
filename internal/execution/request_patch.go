package execution

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/gitwork"
	"strings"
)

func (e *Executor) requestWorkspace(ctx context.Context, r monitorRequest, base, branch string) (*gitwork.Workspace, error) {
	c := e.gitConfig()
	return gitwork.OpenRequest(ctx, c, r.Repo, base, branch, r.ID)
}
func (e *Executor) prepareRequestedPatch(ctx context.Context, r monitorRequest, p prPayload, base string) error {
	w, err := e.requestWorkspace(ctx, r, base, p.Branch)
	if err != nil {
		return err
	}
	checked := func(args ...string) (string, error) {
		out, err := w.Config.Checked(ctx, w.Tree, args...)
		if err != nil {
			return "", fmt.Errorf("依頼パッチのGit検証が失敗しました: %w", err)
		}
		return out, nil
	}
	if _, err := checked("cat-file", "-e", p.Base+"^{commit}"); err != nil {
		return err
	}
	if _, err := checked("merge-base", "--is-ancestor", p.Base, "refs/remotes/origin/"+base); err != nil {
		return cause.New("baseが既定ブランチの祖先ではありません", err)
	}
	record := fmt.Sprintf("チャットのAIからの依頼\n依頼ID: %s\nパッチSHA256: %s", r.ID, p.PatchHash)
	remote, err := w.Config.Run(ctx, w.Tree, "rev-parse", "--verify", "refs/remotes/origin/"+p.Branch)
	if err != nil {
		return err
	}
	published := remote.Code == 0
	if published {
		// The PR is made from the published branch, so check what is on the
		// remote, not the local tree a previous attempt left.
		ref := "refs/remotes/origin/" + p.Branch
		message, err := checked("log", "-1", "--format=%B", ref)
		if err != nil {
			return err
		}
		parent, err := checked("rev-parse", ref+"^")
		if err != nil {
			return err
		}
		if !strings.HasSuffix(message, record) || parent != p.Base {
			return errors.New("公開済みの依頼ブランチと記録が一致しません")
		}
		// 公開済みブランチは上書きせず、indexで同じパッチのtreeを再現して比較する。
		tree, err := checked("rev-parse", ref+"^{tree}")
		if err != nil {
			return err
		}
		if _, err := w.Config.Checked(ctx, w.Tree, "read-tree", p.Base); err != nil {
			return fmt.Errorf("パッチのbaseを読めません: %w", err)
		}
		if err := w.Config.InputChecked(ctx, w.Tree, p.Patch, "apply", "--cached", "--whitespace=nowarn", "-"); err != nil {
			return fmt.Errorf("公開済みパッチを再現できません: %w", err)
		}
		actual, err := checked("write-tree")
		if err != nil {
			return err
		}
		if _, err := checked("read-tree", "HEAD"); err != nil {
			return err
		}
		if tree != actual {
			return errors.New("公開済みの内容と依頼パッチが一致しません")
		}
		return nil
	}
	if _, err := checked("reset", "--hard", p.Base); err != nil {
		return err
	}
	// request-ID専用の作業ツリーだけを初期化する。
	if _, err := checked("clean", "-fd"); err != nil {
		return err
	}
	if err := w.Config.InputChecked(ctx, w.Tree, p.Patch, "apply", "--index", "--whitespace=nowarn", "-"); err != nil {
		return fmt.Errorf("パッチを安全に適用できません: %w", err)
	}
	if err := e.validatePending(ctx, w); err != nil {
		return err
	}
	head, err := w.Commit(ctx, e.redact(p.Title)+"\n\n"+record)
	if err != nil {
		return err
	}
	if head == p.Base {
		return errors.New("パッチの変更が空です")
	}
	return w.Push(ctx)
}
