package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

type setupArtifactsFixture struct {
	tree      string
	executor  *Executor
	workspace *gitwork.Workspace
	record    *journal.Record
}

func newSetupArtifactsFixture(t *testing.T, trackedLock bool) *setupArtifactsFixture {
	t.Helper()
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(tree, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "workspaces"), 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tree
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	git("init")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tree, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", "{}\n")
	if trackedLock {
		write("pnpm-lock.yaml", "before\n")
	}
	git("add", "--all")
	git("commit", "-m", "base")
	sandbox := filepath.Join(root, "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: root, Codex: sandbox, Git: gitPath, GitUser: "Fixture", TestTimeoutSeconds: 30}}}
	return &setupArtifactsFixture{tree, e, &gitwork.Workspace{Tree: tree, Config: gitwork.Config{Git: gitPath, User: "Fixture"}}, &journal.Record{Tree: tree}}
}

func (f *setupArtifactsFixture) setup(t *testing.T) {
	t.Helper()
	command := []string{"/bin/sh", "-c", "mkdir -p node_modules/x && printf '{}\\n' > node_modules/x/y.json && if [ ! -e pnpm-lock.yaml ]; then printf 'generated\\n' > pnpm-lock.yaml; fi"}
	if err := f.executor.setup(context.Background(), Repository{SetupCommand: command}, f.record); err != nil {
		t.Fatal(err)
	}
}
func (f *setupArtifactsFixture) write(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.tree, name), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *setupArtifactsFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := f.workspace.Config.Checked(context.Background(), f.tree, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSetupArtifactsNotStaged(t *testing.T) {
	f := newSetupArtifactsFixture(t, false)
	f.setup(t)
	f.write(t, "feature.txt", "change\n")
	pending, err := f.workspace.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pending, ",") != "feature.txt" {
		t.Fatalf("pending: %v", pending)
	}
	if err := f.workspace.Stage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.workspace.Commit(context.Background(), "feature"); err != nil {
		t.Fatal(err)
	}
	if got := f.git(t, "diff", "--name-only", "HEAD^", "HEAD"); got != "feature.txt" {
		t.Fatalf("committed paths: %q", got)
	}
	pending, err = f.workspace.Pending(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after commit: %v, %v", pending, err)
	}
}

func TestSetupArtifactsKeepTestHeadClean(t *testing.T) {
	f := newSetupArtifactsFixture(t, false)
	f.setup(t)
	if _, err := f.executor.testHead(context.Background(), f.record); err != nil {
		t.Fatal(err)
	}
}

func TestSetupKeepsIntendedChanges(t *testing.T) {
	f := newSetupArtifactsFixture(t, true)
	f.setup(t)
	f.write(t, "package.json", "{\"name\":\"changed\"}\n")
	f.write(t, "pnpm-lock.yaml", "changed\n")
	if err := f.workspace.Stage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.git(t, "diff", "--cached", "--name-only"); got != "package.json\npnpm-lock.yaml" {
		t.Fatalf("staged paths: %q", got)
	}
}

func TestSetupArtifactsIsolatedBetweenWorktrees(t *testing.T) {
	f := newSetupArtifactsFixture(t, false)
	other := f.tree
	linked := filepath.Join(filepath.Dir(other), "linked")
	f.git(t, "worktree", "add", "--detach", linked, "HEAD")
	f.write(t, "pnpm-lock.yaml", "existing\n")
	f.tree, f.workspace.Tree, f.record.Tree = linked, linked, linked
	f.setup(t)
	pending, err := f.workspace.Pending(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("linked worktree pending: %v, %v", pending, err)
	}
	otherWorkspace := &gitwork.Workspace{Tree: other, Config: f.workspace.Config}
	pending, err = otherWorkspace.Pending(context.Background())
	if err != nil || strings.Join(pending, ",") != "pnpm-lock.yaml" {
		t.Fatalf("other worktree pending: %v, %v", pending, err)
	}
	if _, err := f.executor.testHead(context.Background(), f.record); err != nil {
		t.Fatal(err)
	}
}

func TestSetupKeepsPreexistingUntrackedLockfile(t *testing.T) {
	f := newSetupArtifactsFixture(t, false)
	f.write(t, "pnpm-lock.yaml", "user file\n")
	f.setup(t)
	pending, err := f.workspace.Pending(context.Background())
	if err != nil || strings.Join(pending, ",") != "pnpm-lock.yaml" {
		t.Fatalf("pending: %v, %v", pending, err)
	}
	if err := f.workspace.Stage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.git(t, "diff", "--cached", "--name-only"); got != "pnpm-lock.yaml" {
		t.Fatalf("staged paths: %q", got)
	}
}

func TestNoSetupKeepsExistingNodeModules(t *testing.T) {
	f := newSetupArtifactsFixture(t, false)
	if err := os.MkdirAll(filepath.Join(f.tree, "node_modules"), 0700); err != nil {
		t.Fatal(err)
	}
	f.write(t, "node_modules/manual.txt", "user file\n")
	if err := f.executor.setup(context.Background(), Repository{}, f.record); err != nil {
		t.Fatal(err)
	}
	pending, err := f.workspace.Pending(context.Background())
	if err != nil || strings.Join(pending, ",") != "node_modules/manual.txt" {
		t.Fatalf("pending: %v, %v", pending, err)
	}
}
