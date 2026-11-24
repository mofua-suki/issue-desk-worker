package gitwork

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"os"
	"path/filepath"
	"strings"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/process"
)

// A started removal must not inherit the short maintenance admission deadline:
// Git can already have removed .git when it is still deleting ignored files.
// Bound the entire remove/prune phase by the existing Git operation allowance.
const workspaceRemovalTimeout = 5 * time.Minute

// ErrWorkspaceDirty is a finished tree left with changes or untracked files.
// It is kept on purpose: nothing in it is removed without a person.
var ErrWorkspaceDirty = errors.New("変更が残っているため残します")

// CleanupWorkspaceExists checks the shipped job layout without following links.
// Only ENOENT means absent; inaccessible or malformed paths remain failures.
// Root and all its ancestors must also be real directories, without symlinks.
func CleanupWorkspaceExists(c Config, repo string, id int64, tree string) (bool, error) {
	if !filepath.IsAbs(c.Root) || !gitea.ValidRepo(repo) || id < 1 || tree != filepath.Join(c.Root, "workspaces", strings.ReplaceAll(repo, "/", "__"), fmt.Sprintf("job-%d", id)) {
		return false, errors.New("片付け対象の配置が一致しません")
	}
	if err := cleanupDirectory(tree); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func cleanupDirectory(path string) error {
	if parent := filepath.Dir(path); parent != path {
		if err := cleanupDirectory(parent); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("片付け対象の経路が通常のディレクトリではありません")
	}
	return nil
}

// CleanupWorkspace serializes with open/PrepareRetry. recheck runs under that
// same lock, after Git validation and immediately before the non-forced remove.
// The caller must also reserve the scheduler's idle repository slot.
func CleanupWorkspace(ctx context.Context, c Config, repo string, id int64, tree string, recheck func() (bool, error)) error {
	return cleanupTree(ctx, c, repo, tree, func() (bool, error) { return CleanupWorkspaceExists(c, repo, id, tree) }, recheck)
}

// RequestTrees lists the trees a request left in the shipped layout: the
// request's own tree (request-<id>, full test and patch PR creation) and its
// merge trees (request-<id>-merge-*, manual merge).
func RequestTrees(c Config, repo, id string) ([]string, error) {
	if !filepath.IsAbs(c.Root) || !gitea.ValidRepo(repo) || !ValidRequestID(id) {
		return nil, errors.New("片付け対象の配置が一致しません")
	}
	dir := filepath.Join(c.Root, "workspaces", strings.ReplaceAll(repo, "/", "__"))
	trees, err := filepath.Glob(filepath.Join(dir, "request-"+id+"-merge-*"))
	if err != nil {
		return nil, err
	}
	own := filepath.Join(dir, "request-"+id)
	if _, err := os.Lstat(own); err == nil {
		trees = append([]string{own}, trees...)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return trees, nil
}

// CleanupRequestWorkspace removes one tree of a finished request with the
// same checks and non-forced removal as a job's tree.
func CleanupRequestWorkspace(ctx context.Context, c Config, repo, id, tree string, recheck func() (bool, error)) error {
	exists := func() (bool, error) {
		name := filepath.Base(tree)
		if !filepath.IsAbs(c.Root) || !gitea.ValidRepo(repo) || !ValidRequestID(id) || filepath.Dir(tree) != filepath.Join(c.Root, "workspaces", strings.ReplaceAll(repo, "/", "__")) || (name != "request-"+id && !strings.HasPrefix(name, "request-"+id+"-merge-")) {
			return false, errors.New("片付け対象の配置が一致しません")
		}
		if err := cleanupDirectory(tree); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	return cleanupTree(ctx, c, repo, tree, exists, recheck)
}

func cleanupTree(ctx context.Context, c Config, repo, tree string, present func() (bool, error), recheck func() (bool, error)) error {
	lock := repositoryLock(c.Root + "/" + repo)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !lock.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	exists, err := present()
	if err != nil || !exists {
		return err
	}
	mirror := filepath.Join(c.Root, "control", "repos", strings.ReplaceAll(repo, "/", "__")+".git")
	if err := cleanupDirectory(mirror); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(tree, ".git"))
	if err != nil || !info.Mode().IsRegular() {
		return cause.New("片付け対象のGit管理ファイルを確認できません", err)
	}
	common, err := cleanupGit(ctx, c, tree, 5*time.Second, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || strings.TrimSpace(common) != mirror {
		return cause.New("片付け対象のGit管理領域が一致しません", err)
	}
	registered, err := cleanupGit(ctx, c, c.Root, 5*time.Second, "--git-dir", mirror, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return err
	}
	found := false
	for _, field := range strings.Split(registered, "\x00") {
		if field == "worktree "+tree {
			found = true
		}
	}
	if !found || recheck == nil {
		return errors.New("片付け対象のGit登録を確認できません")
	}
	ok, err := recheck()
	if err != nil || !ok {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Keep the repository lock and the scheduler's idle slot until both commands
	// finish. Cancellation before this point keeps the tree; after admission it
	// stops the next job, rather than interrupting this destructive operation.
	finishCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), workspaceRemovalTimeout)
	defer finish()
	// git refuses to remove a tree with changes or untracked files; say so
	// instead of a generic failure, so the caller can keep it quietly. A large
	// tree gets the removal allowance, as the removal itself would.
	status, err := cleanupGit(finishCtx, c, tree, workspaceRemovalTimeout, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return ErrWorkspaceDirty
	}
	if _, err := cleanupGit(finishCtx, c, c.Root, workspaceRemovalTimeout, "--git-dir", mirror, "worktree", "remove", tree); err != nil {
		return err
	}
	_, err = cleanupGit(finishCtx, c, c.Root, workspaceRemovalTimeout, "--git-dir", mirror, "worktree", "prune")
	return err
}

// Local commands carry no credential helper and cannot use any Git transport.
// Never expose Git output or path-bearing process errors in maintenance logs.
func cleanupGit(ctx context.Context, c Config, dir string, timeout time.Duration, args ...string) (string, error) {
	out, err := c.Local(ctx, dir, timeout, args...)
	if err != nil || out.Code != 0 {
		return "", cause.New("片付けのローカルGit操作に失敗しました", err)
	}
	return out.Out, nil
}

// Local runs a Git command that only reads or changes local repository data.
// It never reads the host's system or global configuration, nor the default
// per-user ignore and attributes files under ~/.config/git, runs no hooks,
// carries no credential helper and cannot use any transport.
func (c Config) Local(ctx context.Context, dir string, timeout time.Duration, args ...string) (process.Result, error) {
	prefix := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.excludesFile=/dev/null", "-c", "core.attributesFile=/dev/null", "-c", "credential.helper=", "-c", "protocol.allow=never"}
	return process.Run(ctx, c.Git, append(prefix, localExcludeArgs(dir, args...)...), process.Options{Dir: dir, Timeout: timeout, Env: map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_OPTIONAL_LOCKS": "0"}})
}
