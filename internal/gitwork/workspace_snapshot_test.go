package gitwork

import (
	"context"
	"errors"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSnapshotUsesMergeBase(t *testing.T) {
	c, mirror, tree := cleanupTestWorkspace(t)
	c.User = "Fixture"
	w := &Workspace{Config: c, Tree: tree, Mirror: mirror, Default: "main"}
	ancestor, err := w.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	head := sameChangesCommit(t, w, "T", "test\n", "head")
	cleanupTestGit(t, tree, "checkout", "--detach", ancestor)
	base := sameChangesCommit(t, w, "P", "protected\n", "base")
	cleanupTestGit(t, tree, "update-ref", "refs/remotes/origin/main", base)
	cleanupTestGit(t, tree, "checkout", "--detach", head)
	s, err := w.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Head != head || s.Base != base || s.Base == ancestor || !reflect.DeepEqual(s.Paths, []string{"T"}) || !reflect.DeepEqual(s.Statuses, map[string]string{"T": "A"}) {
		t.Fatalf("snapshot: %+v", s)
	}
	expected := cleanupTestGit(t, tree, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", ancestor, head)
	if s.Diff != expected {
		t.Fatalf("diff mismatch: %s", s.Diff)
	}
	var headers []string
	for _, line := range strings.Split(s.Diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			headers = append(headers, line)
		}
	}
	if !reflect.DeepEqual(headers, []string{"diff --git a/T b/T"}) {
		t.Fatal(headers)
	}
}

func TestSnapshotWithoutMergeBase(t *testing.T) {
	c, mirror, tree := cleanupTestWorkspace(t)
	w := &Workspace{Config: c, Tree: tree, Mirror: mirror, Default: "main"}
	orphan := strings.TrimSpace(cleanupTestGit(t, tree, "-c", "user.name=Fixture", "-c", "user.email=f@example.invalid", "commit-tree", "-m", "orphan", cleanupTestEmptyTree(t, c.Root, mirror)))
	cleanupTestGit(t, tree, "update-ref", "refs/remotes/origin/main", orphan)
	// Exercise Follow with a real fetch over HTTP.
	remote := t.TempDir()
	cleanupTestGit(t, remote, "init", "--bare", ".")
	cleanupTestGit(t, tree, "push", remote, orphan+":refs/heads/main")
	backend := strings.TrimSpace(cleanupTestGit(t, tree, "--exec-path"))
	server := httptest.NewServer(&cgi.Handler{Path: filepath.Join(backend, "git-http-backend"), Env: []string{"GIT_PROJECT_ROOT=" + remote, "GIT_HTTP_EXPORT_ALL=1"}})
	defer server.Close()
	cleanupTestGit(t, tree, "remote", "add", "origin", server.URL+"/")
	_, err := w.Snapshot(context.Background())
	if !errors.Is(err, ErrMergeBase) || errors.Is(err, ErrConflict) || err.Error() != "マージベースを確認できません" {
		t.Fatal(err)
	}
	_, err = w.Follow(context.Background())
	if !errors.Is(err, ErrMergeBase) || errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestRevisionStateChecksRevisionsWithoutGeneratingDiff(t *testing.T) {
	c, mirror, tree := cleanupTestWorkspace(t)
	c.User = "Fixture"
	w := &Workspace{Config: c, Tree: tree, Mirror: mirror, Default: "main"}
	ctx := context.Background()
	ancestor, err := w.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head := sameChangesCommit(t, w, "feature", "feature\n", "feature")
	cleanupTestGit(t, tree, "checkout", "--detach", ancestor)
	base := sameChangesCommit(t, w, "base", "base\n", "base")
	cleanupTestGit(t, tree, "update-ref", "refs/remotes/origin/main", base)
	cleanupTestGit(t, tree, "checkout", "--detach", head)
	git, err := exec.LookPath(c.Git)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nfor arg do [ \"$arg\" = diff ] && exit 97; done\nexec " + shellQuote(git) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	w.Config.Git = wrapper
	gotHead, gotBase, err := w.RevisionState(ctx)
	if err != nil || gotHead != head || gotBase != base {
		t.Fatalf("head=%s base=%s err=%v", gotHead, gotBase, err)
	}
	orphan := strings.TrimSpace(cleanupTestGit(t, tree, "-c", "user.name=Fixture", "-c", "user.email=f@example.invalid", "commit-tree", "-m", "orphan", cleanupTestEmptyTree(t, c.Root, mirror)))
	cleanupTestGit(t, tree, "update-ref", "refs/remotes/origin/main", orphan)
	if _, _, err := w.RevisionState(ctx); !errors.Is(err, ErrMergeBase) {
		t.Fatalf("expected missing common ancestor: %v", err)
	}
}
