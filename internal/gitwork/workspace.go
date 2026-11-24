package gitwork

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/process"
)

var lfsFilterPattern = regexp.MustCompile(`filter\s*=\s*lfs`)

type Config struct{ Git, Root, BaseURL, TokenFile, Executable, User string }
type Workspace struct {
	Config                              Config
	Repo, Default, Branch, Mirror, Tree string
}
type Snapshot struct {
	Base, Head, Diff string
	Paths            []string
	Statuses         map[string]string
}

var repositoryLocks sync.Map

func repositoryLock(key string) *sync.Mutex {
	value, _ := repositoryLocks.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
}

var ErrMergeBase = errors.New("マージベースを確認できません")

var ErrConflict = errors.New("既定ブランチとの競合があります")
var sha = regexp.MustCompile(`^[0-9a-f]{40}$`)
var requestIDPattern = regexp.MustCompile(`^\d{1,16}-[a-f0-9]{8}$`)
var branchShapePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func (c Config) args(args ...string) []string {
	helper := "!" + shellQuote(c.Executable) + " credential --token-file " + shellQuote(c.TokenFile) + " --origin " + shellQuote(c.BaseURL)
	// Like Local, the per-user ignore and attributes files under ~/.config/git
	// are not read even though GIT_CONFIG_GLOBAL is /dev/null: a host's own
	// ignore list must not keep a changed file out of the job's commit.
	prefix := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.excludesFile=/dev/null", "-c", "core.attributesFile=/dev/null", "-c", "http.followRedirects=false", "-c", "protocol.file.allow=never", "-c", "credential.helper=", "-c", "credential.helper=" + helper, "-c", "user.name=" + c.User, "-c", "user.email=issue-desk@localhost"}
	return append(prefix, args...)
}
func (c Config) Run(ctx context.Context, dir string, args ...string) (process.Result, error) {
	return process.Run(ctx, c.Git, append(c.args(), localExcludeArgs(dir, args...)...), process.Options{Dir: dir, Timeout: 5 * time.Minute, Env: map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"}})
}

// Checked runs git like Run and returns its trimmed output. A non-zero exit
// becomes an error naming the subcommand, the exit code and the end of
// stderr (without credentials in URLs).
func (c Config) Checked(ctx context.Context, dir string, args ...string) (string, error) {
	return c.checked(ctx, dir, args...)
}

func (c Config) checked(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := c.Run(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", CommandError(args, out)
	}
	return strings.TrimSpace(out.Out), nil
}

// CommandError describes a git command that exited non-zero by its
// subcommand, exit code and the end of its stderr (without credentials in
// URLs). Use it where the output must be kept exactly and Checked, which
// trims it, does not fit.
func CommandError(args []string, out process.Result) error {
	if detail := gitStderrTail(out.Err); detail != "" {
		return fmt.Errorf("実行したGit操作（%s）が失敗しました（終了コード%d）: %s", gitSubcommand(args), out.Code, detail)
	}
	return fmt.Errorf("実行したGit操作（%s）が失敗しました（終了コード%d）", gitSubcommand(args), out.Code)
}

// gitSubcommand names the git subcommand in args, skipping global options.
func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-c" || a == "-C" || a == "--git-dir" || a == "--work-tree":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	return "git"
}

// gitStderrTailLimit keeps the end of git's stderr, where it puts the reason.
const gitStderrTailLimit = 300

// gitStderrTail keeps the end of git's stderr on one line, without credentials
// in remote URLs.
func gitStderrTail(stderr string) string {
	text := strings.Join(strings.Fields(process.HideURLCredentials(stderr)), " ")
	if len(text) > gitStderrTailLimit {
		text = "…" + strings.ToValidUTF8(text[len(text)-gitStderrTailLimit:], "")
	}
	return text
}
func Open(ctx context.Context, c Config, repo, base, branch string, id int64) (*Workspace, error) {
	if !gitea.ValidRepo(repo) || id < 1 || base == "" || strings.HasPrefix(base, "-") || !strings.HasPrefix(branch, "codex/") {
		return nil, errors.New("作業リポジトリ・枝の指定が不正です")
	}
	return open(ctx, c, repo, base, branch, fmt.Sprintf("job-%d", id), false)
}
func OpenRequest(ctx context.Context, c Config, repo, base, branch, id string) (*Workspace, error) {
	if !gitea.ValidRepo(repo) || !ValidRequestID(id) || !validBranch(branch) || !validBranch(base) {
		return nil, errors.New("依頼の作業枝が不正です")
	}
	return open(ctx, c, repo, base, branch, "request-"+id, false)
}

// ValidSHA reports whether s is a full 40-hex Git object name.
func ValidSHA(s string) bool {
	return sha.MatchString(s)
}
func ValidRequestID(id string) bool {
	return requestIDPattern.MatchString(id)
}

func validBranch(branch string) bool { return ValidBranchShape(branch) }

func ValidBranchShape(branch string) bool {
	return branchShapePattern.MatchString(branch) && !strings.Contains(branch, "..") && !strings.Contains(branch, "//") && !strings.HasSuffix(branch, "/") && !strings.HasSuffix(branch, ".lock")
}
func open(ctx context.Context, c Config, repo, base, branch, treeID string, detached bool) (_ *Workspace, err error) {
	lock := repositoryLock(c.Root + "/" + repo)
	lock.Lock()
	defer lock.Unlock()
	w := &Workspace{Config: c, Repo: repo, Default: base, Branch: branch, Mirror: filepath.Join(c.Root, "control", "repos", strings.ReplaceAll(repo, "/", "__")+".git"), Tree: filepath.Join(c.Root, "workspaces", strings.ReplaceAll(repo, "/", "__"), treeID)}
	if err := os.MkdirAll(filepath.Dir(w.Mirror), 0700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(w.Mirror); errors.Is(err, os.ErrNotExist) {
		if _, err := c.checked(ctx, c.Root, "init", "--bare", w.Mirror); err != nil {
			return nil, err
		}
		if _, err := c.checked(ctx, c.Root, "--git-dir", w.Mirror, "remote", "add", "origin", strings.TrimRight(c.BaseURL, "/")+"/"+repo+".git"); err != nil {
			return nil, err
		}
		if _, err := c.checked(ctx, c.Root, "--git-dir", w.Mirror, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err := w.fetch(ctx); err != nil {
		return nil, err
	}
	if detached {
		if err := os.MkdirAll(filepath.Dir(w.Tree), 0700); err != nil {
			return nil, err
		}
		w.Tree, err = os.MkdirTemp(filepath.Dir(w.Tree), treeID+"-merge-")
		if err != nil {
			return nil, err
		}
		// A merge attempt that cannot open never runs, so its fresh tree is not kept.
		defer func() {
			if err != nil {
				discardMergeTree(ctx, c, w.Mirror, w.Tree)
			}
		}()
	}
	if _, err := os.Lstat(filepath.Join(w.Tree, ".git")); err == nil {
		common, err := c.checked(ctx, w.Tree, "rev-parse", "--git-common-dir")
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(common) {
			common = filepath.Join(w.Tree, common)
		}
		if filepath.Clean(common) != w.Mirror {
			return nil, errors.New("作業ツリーのGit管理領域が一致しません")
		}
		current, err := c.checked(ctx, w.Tree, "branch", "--show-current")
		if err != nil || current != branch {
			return nil, cause.New("保存済み作業ツリーの枝が一致しません", err)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(w.Tree), 0700); err != nil {
			return nil, err
		}
		start := "refs/remotes/origin/" + base
		// A failed run (timeout, launch failure) leaves Code 0; it must not read as "the ref exists".
		exists, err := c.Run(ctx, c.Root, "--git-dir", w.Mirror, "rev-parse", "--verify", "refs/remotes/origin/"+branch)
		if err != nil {
			return nil, err
		}
		if exists.Code == 0 {
			start = "refs/remotes/origin/" + branch
		}
		existing, err := c.Run(ctx, c.Root, "--git-dir", w.Mirror, "show-ref", "--verify", "refs/heads/"+branch)
		if err != nil {
			return nil, err
		}
		args := []string{"--git-dir", w.Mirror, "worktree", "add"}
		if detached {
			args = append(args, "--detach", w.Tree, "refs/remotes/origin/"+branch)
		} else if existing.Code != 0 {
			args = append(args, "-b", branch, w.Tree, start)
		} else {
			args = append(args, w.Tree, branch)
		}
		if _, err := c.checked(ctx, c.Root, args...); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(w.Tree, ".gitmodules")); err == nil {
		return nil, errors.New("submoduleを含むリポジトリは実行できません")
	}
	if out, err := c.Run(ctx, w.Tree, "ls-files", "--stage", "-z"); err != nil || out.Code != 0 {
		return nil, cause.New("Gitの管理ファイル種別を確認できません", err)
	} else {
		for _, entry := range strings.Split(out.Out, "\x00") {
			if strings.HasPrefix(entry, "120000 ") || strings.HasPrefix(entry, "160000 ") {
				return nil, errors.New("シンボリックリンク・submoduleを含むリポジトリは実行できません")
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(w.Tree, ".gitattributes")); err == nil && lfsFilterPattern.Match(data) {
		return nil, errors.New("対象がGit LFSを含むためリポジトリを実行できません")
	}
	return w, nil
}

// discardMergeTree removes a merge tree created by the failed open, and its
// registration. It runs under the repository lock, even after cancellation.
func discardMergeTree(ctx context.Context, c Config, mirror, tree string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceRemovalTimeout)
	defer cancel()
	_, _ = c.Run(ctx, c.Root, "--git-dir", mirror, "worktree", "remove", "--force", tree)
	_ = os.RemoveAll(tree)
	_, _ = c.Run(ctx, c.Root, "--git-dir", mirror, "worktree", "prune")
}
func (w *Workspace) Fetch(ctx context.Context) error {
	lock := repositoryLock(w.Config.Root + "/" + w.Repo)
	lock.Lock()
	defer lock.Unlock()
	return w.fetch(ctx)
}
func (w *Workspace) fetch(ctx context.Context) error {
	_, err := w.Config.checked(ctx, w.Config.Root, "--git-dir", w.Mirror, "fetch", "--prune", "origin")
	return err
}
func (w *Workspace) Head(ctx context.Context) (string, error) {
	return w.Config.checked(ctx, w.Tree, "rev-parse", "HEAD")
}
func (w *Workspace) Base(ctx context.Context) (string, error) {
	return w.Config.checked(ctx, w.Tree, "rev-parse", "refs/remotes/origin/"+w.Default)
}
func (w *Workspace) Follow(ctx context.Context) (bool, error) {
	if err := w.Fetch(ctx); err != nil {
		return false, err
	}
	head, err := w.Head(ctx)
	if err != nil {
		return false, err
	}
	base, err := w.Base(ctx)
	if err != nil {
		return false, err
	}
	ancestor, err := w.mergeBase(ctx, base, head)
	if err != nil {
		return false, err
	}
	if ancestor == base {
		return false, nil
	}
	out, err := w.Config.Run(ctx, w.Tree, "merge", "--no-edit", "refs/remotes/origin/"+w.Default)
	if err != nil {
		return false, err
	}
	if out.Code != 0 {
		unmerged, checkErr := w.Unmerged(ctx)
		if checkErr != nil {
			return false, checkErr
		}
		if len(unmerged) > 0 {
			return true, ErrConflict
		}
		return false, fmt.Errorf("既定ブランチへの追従に失敗しました: %w", CommandError([]string{"merge"}, out))
	}
	return true, nil
}

// Unmerged lists the paths with an unresolved merge conflict, or none.
func (w *Workspace) Unmerged(ctx context.Context) ([]string, error) {
	out, err := w.Config.checked(ctx, w.Tree, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("競合ファイルを確認できません: %w", err)
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

func (w *Workspace) Stage(ctx context.Context) error {
	_, err := w.Config.checked(ctx, w.Tree, "add", "--all")
	return err
}
func (w *Workspace) Commit(ctx context.Context, title string) (string, error) {
	out, err := w.Config.Run(ctx, w.Tree, "diff", "--cached", "--quiet")
	if err != nil {
		return "", err
	}
	if out.Code == 0 {
		pending, err := w.Config.Run(ctx, w.Tree, "rev-parse", "--verify", "MERGE_HEAD")
		if err != nil {
			return "", err
		}
		if pending.Code != 0 {
			return w.Head(ctx)
		}
	} else if out.Code != 1 {
		return "", fmt.Errorf("作業ツリーのGit差分を確認できません: %w", CommandError([]string{"diff"}, out))
	}
	if _, err := w.Config.checked(ctx, w.Tree, "commit", "-m", title); err != nil {
		return "", err
	}
	return w.Head(ctx)
}
func (w *Workspace) mergeBase(ctx context.Context, base, head string) (string, error) {
	out, err := w.Config.Run(ctx, w.Tree, "merge-base", base, head)
	if err != nil {
		return "", err
	}
	ancestor := strings.TrimSpace(out.Out)
	if out.Code != 0 || !sha.MatchString(ancestor) {
		return "", ErrMergeBase
	}
	return ancestor, nil
}

// RevisionState verifies the current revisions and their common ancestor.
func (w *Workspace) RevisionState(ctx context.Context) (head, base string, err error) {
	head, err = w.Head(ctx)
	if err != nil {
		return
	}
	base, err = w.Base(ctx)
	if err != nil {
		return
	}
	_, err = w.mergeBase(ctx, base, head)
	return
}

func (w *Workspace) Snapshot(ctx context.Context) (Snapshot, error) {
	return w.SnapshotReusing(ctx, Snapshot{})
}

// SnapshotReusing reads the current revisions and reuses a snapshot from this
// workspace only when both commits still match.
func (w *Workspace) SnapshotReusing(ctx context.Context, previous Snapshot) (Snapshot, error) {
	var s Snapshot
	var err error
	s.Head, err = w.Head(ctx)
	if err != nil {
		return s, err
	}
	s.Base, err = w.Base(ctx)
	if err != nil {
		return s, err
	}
	ancestor, err := w.mergeBase(ctx, s.Base, s.Head)
	if err != nil {
		return s, err
	}
	if s.Head == previous.Head && s.Base == previous.Base && previous.Statuses != nil {
		return previous, nil
	}
	raw, err := w.Config.checked(ctx, w.Tree, "diff", "--name-status", "-z", "--no-renames", ancestor, s.Head)
	if err != nil {
		return s, err
	}
	entries := strings.Split(raw, "\x00")
	s.Statuses = map[string]string{}
	for i := 0; i+1 < len(entries); i += 2 {
		if entries[i] == "" {
			break
		}
		s.Statuses[entries[i+1]] = entries[i]
		s.Paths = append(s.Paths, entries[i+1])
	}
	diffArgs := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", ancestor, s.Head}
	out, err := w.Config.Run(ctx, w.Tree, diffArgs...)
	if err != nil {
		return s, cause.New("レビュー差分を読めません", err)
	}
	if out.Code != 0 {
		return s, fmt.Errorf("レビュー差分を読めません: %w", CommandError(diffArgs, out))
	}
	s.Diff = out.Out
	return s, nil
}
func (w *Workspace) Clean(ctx context.Context) bool {
	out, err := w.Config.checked(ctx, w.Tree, "status", "--porcelain")
	return err == nil && out == ""
}

// WorktreeStatus is one porcelain status entry, including both names for a rename.
type WorktreeStatus struct {
	Status string
	Paths  []string
}

func (w *Workspace) WorktreeStatus(ctx context.Context) ([]WorktreeStatus, error) {
	out, err := w.Config.Run(ctx, w.Tree, "status", "--porcelain=v1", "-z")
	if err != nil || out.Code != 0 {
		return nil, cause.New("変更一覧を読めません", err)
	}
	var result []WorktreeStatus
	fields := strings.Split(out.Out, "\x00")
	for i := 0; i < len(fields) && fields[i] != ""; i++ {
		entry := fields[i]
		if len(entry) < 4 || entry[2] != ' ' {
			return nil, errors.New("変更一覧を読めません")
		}
		item := WorktreeStatus{Status: entry[:2], Paths: []string{entry[3:]}}
		if strings.ContainsAny(item.Status, "RC") {
			i++
			if i >= len(fields) || fields[i] == "" {
				return nil, errors.New("変更一覧を読めません")
			}
			item.Paths = append(item.Paths, fields[i])
		}
		result = append(result, item)
	}
	return result, nil
}
func (w *Workspace) ValidateFiles(paths []string) error {
	for _, name := range paths {
		if filepath.IsAbs(name) || strings.HasPrefix(filepath.Clean(name), "..") {
			return errors.New("変更パスが作業ツリー外です")
		}
		info, err := os.Lstat(filepath.Join(w.Tree, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("変更にシンボリックリンクがあります")
		}
	}
	return nil
}

func (w *Workspace) Pending(ctx context.Context) ([]string, error) {
	out, err := w.Config.Run(ctx, w.Tree, "ls-files", "--modified", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, cause.New("公開前の変更一覧を読めません", err)
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("公開前の変更一覧を読めません: %w", CommandError([]string{"ls-files"}, out))
	}
	staged, stagedErr := w.Config.Run(ctx, w.Tree, "diff", "--cached", "--name-only", "-z")
	if stagedErr != nil {
		return nil, cause.New("indexの変更一覧を読めません", stagedErr)
	}
	if staged.Code != 0 {
		return nil, fmt.Errorf("indexの変更一覧を読めません: %w", CommandError([]string{"diff"}, staged))
	}
	items := strings.Split(out.Out+staged.Out, "\x00")
	paths := []string{}
	for _, name := range items {
		if name != "" {
			paths = append(paths, name)
		}
	}
	return paths, nil
}

// ErrRetryBranchExists indicates that the retry branch is already published.
var ErrRetryBranchExists = errors.New("再試行の枝が遠隔に既に存在します")

// PrepareRetry removes only this job's registered tree before retrying.
func PrepareRetry(ctx context.Context, c Config, repo string, id int64, branch string) error {
	if !gitea.ValidRepo(repo) || id < 1 || !validBranch(branch) || !strings.HasPrefix(branch, "codex/") {
		return errors.New("再試行の指定が不正です")
	}
	lock := repositoryLock(c.Root + "/" + repo)
	lock.Lock()
	defer lock.Unlock()
	mirror := filepath.Join(c.Root, "control", "repos", strings.ReplaceAll(repo, "/", "__")+".git")
	tree := filepath.Join(c.Root, "workspaces", strings.ReplaceAll(repo, "/", "__"), fmt.Sprintf("job-%d", id))
	if _, err := os.Lstat(mirror); errors.Is(err, os.ErrNotExist) {
		if _, treeErr := os.Lstat(tree); !errors.Is(treeErr, os.ErrNotExist) {
			return errors.New("作業ツリーのGit管理領域がありません")
		}
		if err := os.MkdirAll(filepath.Dir(mirror), 0700); err != nil {
			return err
		}
		if _, err := c.checked(ctx, c.Root, "init", "--bare", mirror); err != nil {
			return err
		}
		if _, err := c.checked(ctx, c.Root, "--git-dir", mirror, "remote", "add", "origin", strings.TrimRight(c.BaseURL, "/")+"/"+repo+".git"); err != nil {
			return err
		}
		if _, err := c.checked(ctx, c.Root, "--git-dir", mirror, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := c.checked(ctx, c.Root, "--git-dir", mirror, "fetch", "--prune", "origin"); err != nil {
		return err
	}
	remoteExists := false
	localExists := false
	for i, ref := range []string{"refs/heads/" + branch, "refs/remotes/origin/" + branch} {
		out, err := c.Run(ctx, c.Root, "--git-dir", mirror, "show-ref", "--verify", "--quiet", ref)
		if err != nil {
			return err
		}
		if out.Code == 0 {
			if i == 0 {
				localExists = true
			} else {
				remoteExists = true
			}
		}
		if out.Code != 0 && out.Code != 1 {
			return errors.New("再試行の枝を確認できません")
		}
	}
	if localExists && !remoteExists {
		return errors.New("再試行の枝が既に存在します")
	}
	if _, err := os.Lstat(tree); errors.Is(err, os.ErrNotExist) {
		_, err = c.checked(ctx, c.Root, "--git-dir", mirror, "worktree", "prune")
		if err != nil {
			return err
		}
		if remoteExists {
			return ErrRetryBranchExists
		}
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(tree, ".git")); errors.Is(err, os.ErrNotExist) {
		// A removal interrupted partway leaves a directory that is no longer a worktree.
		if err := os.RemoveAll(tree); err != nil {
			return err
		}
		if _, err := c.checked(ctx, c.Root, "--git-dir", mirror, "worktree", "prune"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		common, err := c.checked(ctx, tree, "rev-parse", "--git-common-dir")
		if err != nil {
			return err
		}
		if !filepath.IsAbs(common) {
			common = filepath.Join(tree, common)
		}
		if filepath.Clean(common) != mirror {
			return errors.New("作業ツリーのGit管理領域が一致しません")
		}
		if _, err := c.checked(ctx, c.Root, "--git-dir", mirror, "worktree", "remove", "--force", tree); err != nil {
			return err
		}
	}
	if remoteExists {
		return ErrRetryBranchExists
	}
	return nil
}
