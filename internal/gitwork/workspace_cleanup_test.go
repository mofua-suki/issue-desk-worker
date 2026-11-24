package gitwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cleanupTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

func cleanupTestWorkspace(t *testing.T) (Config, string, string) {
	t.Helper()
	root := t.TempDir()
	mirror := filepath.Join(root, "control", "repos", "owner__project.git")
	tree := filepath.Join(root, "workspaces", "owner__project", "job-1")
	if err := os.MkdirAll(filepath.Dir(mirror), 0700); err != nil {
		t.Fatal(err)
	}
	cleanupTestGit(t, root, "init", "--bare", mirror)
	// A real initial commit and registered worktree, in exactly the deployed layout.
	head := cleanupTestGit(t, root, "--git-dir", mirror, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit-tree", "-m", "initial", cleanupTestEmptyTree(t, root, mirror))
	cleanupTestGit(t, root, "--git-dir", mirror, "update-ref", "refs/heads/codex/job-1", strings.TrimSpace(head))
	cleanupTestGit(t, root, "--git-dir", mirror, "worktree", "add", tree, "codex/job-1")
	return Config{Root: root, Git: "git"}, mirror, tree
}

func cleanupTestEmptyTree(t *testing.T, root, mirror string) string {
	t.Helper()
	return strings.TrimSpace(cleanupTestGit(t, root, "--git-dir", mirror, "mktree"))
}

func TestCleanupRemovesRegisteredTreeAndPreservesOtherAssets(t *testing.T) {
	c, mirror, tree := cleanupTestWorkspace(t)
	manual := filepath.Join(filepath.Dir(tree), "request-123-aabbccdd-merge-saved")
	cleanupTestGit(t, c.Root, "--git-dir", mirror, "worktree", "add", "--detach", manual, "refs/heads/codex/job-1")
	refs := cleanupTestGit(t, c.Root, "--git-dir", mirror, "show-ref")
	if err := CleanupWorkspace(context.Background(), c, "owner/project", 1, tree, func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("tree retained", err)
	}
	registered := cleanupTestGit(t, c.Root, "--git-dir", mirror, "worktree", "list", "--porcelain", "-z")
	if strings.Contains(registered, "worktree "+tree+"\x00") || !strings.Contains(registered, "worktree "+manual+"\x00") {
		t.Fatal("incorrect worktree registrations", registered)
	}
	if got := cleanupTestGit(t, c.Root, "--git-dir", mirror, "show-ref"); got != refs {
		t.Fatal("refs changed")
	}
}

func TestCleanupKeepsDirtyLockedOrUnverifiedTrees(t *testing.T) {
	for _, kind := range []string{"dirty", "locked", "recheck", "reload-error", "wrong-tree", "wrong-common", "unregistered"} {
		t.Run(kind, func(t *testing.T) {
			c, mirror, tree := cleanupTestWorkspace(t)
			target := tree
			switch kind {
			case "dirty":
				if err := os.WriteFile(filepath.Join(tree, "note"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "locked":
				cleanupTestGit(t, c.Root, "--git-dir", mirror, "worktree", "lock", tree)
			case "wrong-tree":
				target = filepath.Join(filepath.Dir(tree), "request-123-aabbccdd")
			case "wrong-common":
				other := filepath.Join(c.Root, "other.git")
				cleanupTestGit(t, c.Root, "init", "--bare", other)
				gitdir := strings.TrimSpace(cleanupTestGit(t, tree, "rev-parse", "--absolute-git-dir"))
				if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte(other), 0600); err != nil {
					t.Fatal(err)
				}
			case "unregistered":
				if err := os.Remove(filepath.Join(tree, ".git")); err != nil {
					t.Fatal(err)
				}
				cleanupTestGit(t, c.Root, "init", tree)
			}
			err := CleanupWorkspace(context.Background(), c, "owner/project", 1, target, func() (bool, error) {
				if kind == "reload-error" {
					return false, errors.New("reload failed")
				}
				return kind != "recheck", nil
			})
			if err == nil && kind != "recheck" {
				t.Fatal("failure not reported")
			}
			if _, err := os.Lstat(tree); err != nil {
				t.Fatal("tree removed", err)
			}
		})
	}
}

func TestCleanupSerializesWithOpen(t *testing.T) {
	c, _, tree := cleanupTestWorkspace(t)
	entered, release, removed, opened := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		removed <- CleanupWorkspace(ctx, c, "owner/project", 1, tree, func() (bool, error) {
			close(entered)
			select {
			case <-release:
				return true, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("cleanup did not enter lock")
	}
	go func() {
		cancelled, stop := context.WithCancel(ctx)
		stop()
		_, err := Open(cancelled, c, "owner/project", "main", "codex/job-1", 1)
		opened <- err
	}()
	select {
	case <-opened:
		t.Fatal("open bypassed repositoryLock")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	if err := <-opened; err == nil {
		t.Fatal("cancelled open succeeded")
	}
}

func TestCleanupWaitAndPathChecks(t *testing.T) {
	c := Config{Root: t.TempDir()}
	tree := filepath.Join(c.Root, "workspaces", "owner__project", "job-1")
	if exists, err := CleanupWorkspaceExists(c, "owner/project", 1, tree); err != nil || exists {
		t.Fatal(exists, err)
	}
	if err := os.WriteFile(filepath.Join(c.Root, "workspaces"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CleanupWorkspaceExists(c, "owner/project", 1, tree); err == nil {
		t.Fatal("invalid parent treated as absent")
	}
	if err := os.Remove(filepath.Join(c.Root, "workspaces")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(c.Root, "workspaces")); err != nil {
		t.Fatal(err)
	}
	if _, err := CleanupWorkspaceExists(c, "owner/project", 1, tree); err == nil {
		t.Fatal("symlink accepted")
	}
	lock := repositoryLock(c.Root + "/owner/project")
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := CleanupWorkspace(ctx, c, "owner/project", 1, tree, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestCleanupFinishesSlowRemovalAfterCallerDeadline(t *testing.T) {
	c, mirror, tree := cleanupTestWorkspace(t)
	// Use the real deployed layout and Git's ignored dependency-directory case.
	if err := os.WriteFile(filepath.Join(tree, ".gitignore"), []byte("node_modules/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupTestGit(t, tree, "add", ".gitignore")
	cleanupTestGit(t, tree, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "ignore dependencies")
	deps := filepath.Join(tree, "node_modules", "package")
	if err := os.MkdirAll(deps, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		if err := os.WriteFile(filepath.Join(deps, fmt.Sprintf("file-%d", i)), []byte("dependency"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	pruned := filepath.Join(c.Root, "pruned")
	c.Git = filepath.Join(c.Root, "slow-git")
	// Six seconds exceeds both former five-second limits. Only the remove is
	// delayed; all validation and the eventual remove/prune use real Git.
	script := "#!/bin/sh\nfor arg do\n case \"$arg\" in\n remove) sleep 6;;\n prune) : > " + shellQuote(pruned) + ";;\n esac\ndone\nexec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(c.Git, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := CleanupWorkspace(ctx, c, "owner/project", 1, tree, func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("caller did not expire during removal")
	}
	if _, err := os.Lstat(tree); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("slow removal left a partial tree", err)
	}
	if _, err := os.Stat(pruned); err != nil {
		t.Fatal("prune was skipped after caller cancellation", err)
	}
	// A later cycle must remain a harmless no-op, with no stale registration.
	if err := CleanupWorkspace(context.Background(), c, "owner/project", 1, tree, nil); err != nil {
		t.Fatal(err)
	}
	if registered := cleanupTestGit(t, c.Root, "--git-dir", mirror, "worktree", "list", "--porcelain", "-z"); strings.Contains(registered, "worktree "+tree+"\x00") {
		t.Fatal("removed tree remains registered", registered)
	}
}

func TestCleanupCancellationBeforeRemovalKeepsRegisteredTree(t *testing.T) {
	c, mirror, tree := cleanupTestWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := CleanupWorkspace(ctx, c, "owner/project", 1, tree, func() (bool, error) {
		cancel()
		return true, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation before remove not honored", err)
	}
	if _, err := os.Stat(filepath.Join(tree, ".git")); err != nil {
		t.Fatal("cancelled cleanup changed tree", err)
	}
	if err := CleanupWorkspace(context.Background(), c, "owner/project", 1, tree, func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("later cleanup retained tree", err)
	}
	if registered := cleanupTestGit(t, c.Root, "--git-dir", mirror, "worktree", "list", "--porcelain", "-z"); strings.Contains(registered, "worktree "+tree+"\x00") {
		t.Fatal("later cleanup retained registration", registered)
	}
}
