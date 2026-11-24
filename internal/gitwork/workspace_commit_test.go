package gitwork

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitNormalEmptyAndResolvedMerge(t *testing.T) {
	c, mirror, tree := cleanupTestWorkspace(t)
	c.User = "Fixture"
	w := &Workspace{Config: c, Tree: tree, Mirror: mirror}
	ctx := context.Background()
	initial, err := w.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head, err := w.Commit(ctx, "empty")
	if err != nil || head != initial {
		t.Fatalf("empty commit: %s %v", head, err)
	}
	if err := os.WriteFile(filepath.Join(tree, "shared"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Stage(ctx); err != nil {
		t.Fatal(err)
	}
	base, err := w.Commit(ctx, "base")
	if err != nil || base == initial {
		t.Fatalf("normal commit: %s %v", base, err)
	}
	cleanupTestGit(t, tree, "branch", "other")
	if err := os.WriteFile(filepath.Join(tree, "shared"), []byte("ours\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Stage(ctx); err != nil {
		t.Fatal(err)
	}
	ours, err := w.Commit(ctx, "ours")
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestGit(t, tree, "checkout", "other")
	if err := os.WriteFile(filepath.Join(tree, "shared"), []byte("theirs\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Stage(ctx); err != nil {
		t.Fatal(err)
	}
	theirs, err := w.Commit(ctx, "theirs")
	if err != nil {
		t.Fatal(err)
	}
	cleanupTestGit(t, tree, "checkout", "codex/job-1")
	result, err := c.Run(ctx, tree, "merge", "--no-edit", "other")
	if err != nil || result.Code == 0 {
		t.Fatalf("expected conflict: %+v %v", result, err)
	}
	if err := os.WriteFile(filepath.Join(tree, "shared"), []byte("ours\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Stage(ctx); err != nil {
		t.Fatal(err)
	}
	merged, err := w.Commit(ctx, "resolve")
	if err != nil || merged == ours {
		t.Fatalf("empty resolution: %s %v", merged, err)
	}
	parents := strings.Fields(cleanupTestGit(t, tree, "show", "-s", "--format=%P", merged))
	if len(parents) != 2 || parents[0] != ours || parents[1] != theirs {
		t.Fatalf("parents %v", parents)
	}
	head, err = w.Commit(ctx, "empty after merge")
	if err != nil || head != merged {
		t.Fatalf("post merge no-op: %s %v", head, err)
	}
}
