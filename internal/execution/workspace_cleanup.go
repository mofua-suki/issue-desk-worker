package execution

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

const workspaceCleanupLimit = 5
const workspaceCleanupScanLimit = 100

// This bounds admission (including Gitea and lock waits), not a started Git
// removal. CleanupWorkspace gives remove/prune their own completion allowance;
// when it returns after this deadline, no further record is attempted.
const workspaceCleanupTimeout = 5 * time.Second

// WorkspaceCleaner lives for the controller's lifetime. Its cursor lets later
// records progress even when earlier worktrees repeatedly fail to be removed.
// Cleanup is called serially in the scheduler's idle repository slots.
type WorkspaceCleaner struct {
	Executor    *Executor
	next        int
	nextRequest int
	// kept are trees left on purpose (ErrWorkspaceDirty), logged once each.
	kept map[string]bool
}

// keep logs a tree kept on purpose the first time only, so it does not fill
// the log on every cycle.
func (c *WorkspaceCleaner) keep(target string) {
	if c.kept == nil {
		c.kept = map[string]bool{}
	}
	if c.kept[target] {
		return
	}
	c.kept[target] = true
	fmt.Fprintf(os.Stderr, "%sの作業ツリーは%s。手で確かめてください\n", target, gitwork.ErrWorkspaceDirty)
}

func (c *WorkspaceCleaner) Cleanup(ctx context.Context, eligible []string) {
	ctx, cancel := context.WithTimeout(ctx, workspaceCleanupTimeout)
	defer cancel()
	e := c.Executor
	if ctx.Err() != nil || len(eligible) == 0 {
		return
	}
	config := gitwork.Config{Git: e.Config.Git, Root: e.Config.RuntimeDir}
	ids, err := e.Store.IDs()
	if err != nil {
		cleanupFailure(0, "仕事一覧の読取", err)
		// 依頼の作業ツリーは仕事の一覧に依らないので、こちらは続ける。
		c.cleanupRequests(ctx, eligible, config)
		return
	}
	attempts := 0
	for scanned := 0; scanned < len(ids) && scanned < workspaceCleanupScanLimit && attempts < workspaceCleanupLimit; scanned++ {
		if ctx.Err() != nil {
			return
		}
		c.next %= len(ids)
		id := ids[c.next]
		c.next++
		record, err := e.Store.Load(id)
		if err != nil {
			cleanupFailure(id, "仕事の読取", err)
			continue
		}
		if record == nil {
			continue
		}
		r := *record
		if !cleanupComplete(&r) || !slices.ContainsFunc(eligible, func(repo string) bool { return strings.EqualFold(repo, r.Repo) }) {
			continue
		}
		exists, err := gitwork.CleanupWorkspaceExists(config, r.Repo, r.ID, r.Tree)
		if err == nil && !exists {
			continue
		}
		attempts++ // Invalid paths, HTTP failures and dirty trees all consume a slot.
		if err != nil {
			cleanupFailure(r.ID, "作業ツリーの存在・配置確認", err)
			continue
		}
		issue, err := e.Gitea.Issue(ctx, r.Repo, r.Issue)
		if err == nil && (issue.Number != r.Issue || (issue.State != "open" && issue.State != "closed")) {
			err = errors.New("応答のIssueが一致しません")
		}
		if err != nil {
			cleanupFailure(r.ID, "Issueの確認", err)
			continue
		}
		if issue.State != "closed" {
			continue
		}
		// Before the tree goes: a transient failure keeps the tree so a later cycle retries.
		// The job is finished and reported; another job of the same Issue that has not
		// finished may still continue from this branch or PR, and then removes it itself.
		// An unreadable journal elsewhere keeps only the branch, not every tree.
		shared, err := e.branchStillUsed(r)
		if err != nil {
			mergedBranchFailure(r.PR, "同じIssueの仕事の確認")
		}
		if shared || err != nil {
			// Keep the branch; the tree is still removed below.
		} else if step, err := e.removeMergedBranch(ctx, r.Repo, r.PR, r.Branch); err != nil {
			if transientGitea(err) || pushConnectionFailed(err) {
				cleanupFailure(r.ID, "マージ済みブランチ: "+step, err)
				continue
			}
			mergedBranchFailure(r.PR, step)
		}
		err = gitwork.CleanupWorkspace(ctx, config, r.Repo, r.ID, r.Tree, func() (bool, error) {
			fresh, err := e.Store.Load(r.ID)
			return cleanupComplete(fresh) && fresh.Generation == r.Generation && fresh.Repo == r.Repo && fresh.Issue == r.Issue && fresh.Tree == r.Tree, err
		})
		if errors.Is(err, gitwork.ErrWorkspaceDirty) {
			c.keep(fmt.Sprintf("仕事%d", r.ID))
		} else if err != nil {
			cleanupFailure(r.ID, "作業ツリーの削除", err)
		} else {
			delete(c.kept, fmt.Sprintf("仕事%d", r.ID))
		}
	}
	c.cleanupRequests(ctx, eligible, config)
}

func pushConnectionFailed(err error) bool {
	var failure *gitwork.PushError
	return errors.As(err, &failure) && failure.Error() == gitwork.PushConnectionReason
}

// cleanupRequests removes the trees of requests whose result is saved; a
// saved result is reported again without any tree.
func (c *WorkspaceCleaner) cleanupRequests(ctx context.Context, eligible []string, config gitwork.Config) {
	e := c.Executor
	files, err := filepath.Glob(filepath.Join(e.Config.RuntimeDir, "state", "requests", "*.json"))
	if err != nil {
		cleanupRequestFailure("", "依頼一覧の読取", err)
		return
	}
	attempts := 0
	for scanned := 0; scanned < len(files) && scanned < workspaceCleanupScanLimit && attempts < workspaceCleanupLimit; scanned++ {
		if ctx.Err() != nil {
			return
		}
		c.nextRequest %= len(files)
		id := strings.TrimSuffix(filepath.Base(files[c.nextRequest]), ".json")
		c.nextRequest++
		if !gitwork.ValidRequestID(id) {
			continue
		}
		p, err := e.savedRequest(id)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			cleanupRequestFailure(id, "依頼の読取", err)
			continue
		}
		if !requestFinished(p, id) || !slices.ContainsFunc(eligible, func(repo string) bool { return strings.EqualFold(repo, p.Request.Repo) }) {
			continue
		}
		trees, err := gitwork.RequestTrees(config, p.Request.Repo, id)
		if err != nil {
			cleanupRequestFailure(id, "作業ツリーの存在・配置確認", err)
			continue
		}
		for _, tree := range trees {
			if attempts >= workspaceCleanupLimit || ctx.Err() != nil {
				return
			}
			attempts++
			err := gitwork.CleanupRequestWorkspace(ctx, config, p.Request.Repo, id, tree, func() (bool, error) {
				fresh, err := e.savedRequest(id)
				return err == nil && requestFinished(fresh, id) && fresh.Request.Repo == p.Request.Repo, err
			})
			if errors.Is(err, gitwork.ErrWorkspaceDirty) {
				c.keep("依頼" + id)
			} else if err != nil {
				cleanupRequestFailure(id, "作業ツリーの削除", err)
			} else {
				delete(c.kept, "依頼"+id)
			}
		}
	}
}

// savedRequest reads a saved request progress without creating one.
func (e *Executor) savedRequest(id string) (*requestProgress, error) {
	return e.requests().Saved(id)
}

// requestFinished is a request whose result is saved; it is reported again
// from that result without any tree. Kinds that work in a tree of their own
// are listed: manual merge, full test and PR creation from a patch.
func requestFinished(p *requestProgress, id string) bool {
	if p == nil || p.Request.ID != id || p.Stage != "result" || len(p.Result) == 0 {
		return false
	}
	switch p.Request.Kind {
	case "merge_manual", "full_test", "pr_create":
		return true
	}
	return false
}

func cleanupRequestFailure(id, step string, err error) {
	fmt.Fprintf(os.Stderr, "依頼%sの片付けに失敗しました（%s: %s）。後続の周期で再確認します\n", id, step, cleanupCause(err))
}

// branchStillUsed reports whether another unfinished job of the same Issue carries
// this job's branch or PR.
func (e *Executor) branchStillUsed(r journal.Record) (bool, error) {
	others, err := e.Store.ListIssue(r.Repo, r.Issue)
	if err != nil {
		return false, err
	}
	for _, other := range others {
		if other.ID != r.ID && !cleanupComplete(&other) && (r.Branch != "" && other.Branch == r.Branch || r.PR > 0 && other.PR == r.PR) {
			return true, nil
		}
	}
	return false, nil
}

func cleanupComplete(r *journal.Record) bool {
	return r != nil && r.ID > 0 && r.Issue > 0 && r.Stage == "done" && r.FinalReported
}

func cleanupFailure(id int64, step string, err error) {
	fmt.Fprintf(os.Stderr, "仕事%dの片付けに失敗しました（%s: %s）。後続の周期で再確認します\n", id, step, cleanupCause(err))
}

// cleanupCause names why a cleanup step failed. Neither remote bodies, command
// output, repo names nor paths enter this log, so only the kind of failure is
// kept: an HTTP status, a transport failure kind, or a fixed message.
func cleanupCause(err error) string {
	var refusal *gitea.Error
	var unreachable *gitea.UnreachableError
	var corrupt *journal.CorruptError
	var path *fs.PathError
	switch {
	case err == nil:
		return "原因不明"
	case errors.As(err, &refusal):
		return fmt.Sprintf("Gitea APIがHTTP %dを返しました", refusal.Status)
	case errors.As(err, &unreachable):
		return "Giteaへ接続できません（" + unreachable.Kind + "）"
	case errors.Is(err, context.DeadlineExceeded):
		return "時間切れ"
	case errors.Is(err, context.Canceled):
		return "取り消されました"
	case errors.As(err, &corrupt):
		return "保存した記録が壊れています"
	case errors.As(err, &path):
		return path.Op + ": " + path.Err.Error()
	}
	if message := err.Error(); !strings.ContainsAny(message, "/\\") {
		return message
	}
	return "詳細はパスを含むため記録しません"
}
