package gitwork

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sameChangesCommit(t *testing.T, w *Workspace, file, text, title string) string {
	t.Helper()
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(w.Tree, file), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Stage(ctx); err != nil {
		t.Fatal(err)
	}
	head, err := w.Commit(ctx, title)
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// The default branch moves on a side branch, and the PR branch follows it by merging.
func sameChangesWorkspace(t *testing.T) (*Workspace, string) {
	t.Helper()
	c, mirror, tree := cleanupTestWorkspace(t)
	c.User = "Fixture"
	w := &Workspace{Config: c, Tree: tree, Mirror: mirror, Default: "trunk"}
	base := sameChangesCommit(t, w, "guarded", "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n", "base")
	cleanupTestGit(t, tree, "update-ref", "refs/remotes/origin/trunk", base)
	cleanupTestGit(t, tree, "branch", "base-line", base)
	approved := sameChangesCommit(t, w, "guarded", "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nadded\n", "change")
	return w, approved
}

func sameChangesFollow(t *testing.T, w *Workspace, file, text string) {
	t.Helper()
	cleanupTestGit(t, w.Tree, "checkout", "base-line")
	moved := sameChangesCommit(t, w, file, text, "trunk moves")
	cleanupTestGit(t, w.Tree, "checkout", "-")
	cleanupTestGit(t, w.Tree, "update-ref", "refs/remotes/origin/trunk", moved)
	cleanupTestGit(t, w.Tree, "-c", "user.name=Fixture", "-c", "user.email=f@example.invalid", "merge", "--no-edit", "refs/remotes/origin/trunk")
}

func TestSameFileContentsApprovalBoundaries(t *testing.T) {
	ctx := context.Background()
	t.Run("unchanged", func(t *testing.T) {
		w, a := sameChangesWorkspace(t)
		h := sameChangesCommit(t, w, "other", "x\n", "other")
		ok, err := w.SameFileContents(ctx, a, h, []string{"guarded"})
		if !ok || err != nil {
			t.Fatalf("%v %v", ok, err)
		}
	})
	t.Run("same lines different blob", func(t *testing.T) {
		w, a := sameChangesWorkspace(t)
		sameChangesFollow(t, w, "guarded", "ONE\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n")
		h, _ := w.Head(ctx)
		ok, err := w.SameFileContents(ctx, a, h, []string{"guarded"})
		if ok || err != nil {
			t.Fatalf("%v %v", ok, err)
		}
	})
	t.Run("changed and missing", func(t *testing.T) {
		w, a := sameChangesWorkspace(t)
		h := sameChangesCommit(t, w, "guarded", "changed\n", "edit")
		for _, p := range []string{"guarded", "missing"} {
			ok, err := w.SameFileContents(ctx, a, h, []string{p})
			if ok || err != nil {
				t.Fatalf("%s: %v %v", p, ok, err)
			}
		}
	})
	t.Run("one of several files changed", func(t *testing.T) {
		w, _ := sameChangesWorkspace(t)
		a := sameChangesCommit(t, w, "second", "kept\n", "second")
		h := sameChangesCommit(t, w, "second", "edited\n", "edit second")
		for _, files := range [][]string{{"guarded", "second"}, {"second", "guarded"}} {
			ok, err := w.SameFileContents(ctx, a, h, files)
			if ok || err != nil {
				t.Fatalf("%v: %v %v", files, ok, err)
			}
		}
	})
	t.Run("same content different mode", func(t *testing.T) {
		w, a := sameChangesWorkspace(t)
		if err := os.Chmod(filepath.Join(w.Tree, "guarded"), 0700); err != nil {
			t.Fatal(err)
		}
		h := sameChangesCommit(t, w, "other", "x\n", "mode")
		ok, err := w.SameFileContents(ctx, a, h, []string{"guarded"})
		if ok || err != nil {
			t.Fatalf("%v %v", ok, err)
		}
	})
	t.Run("file replaced by symlink with same bytes", func(t *testing.T) {
		w, _ := sameChangesWorkspace(t)
		a := sameChangesCommit(t, w, "link", "guarded", "file")
		link := filepath.Join(w.Tree, "link")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("guarded", link); err != nil {
			t.Fatal(err)
		}
		h := sameChangesCommit(t, w, "other", "x\n", "swap")
		for _, pair := range [][2]string{{a, h}, {h, h}} {
			ok, err := w.SameFileContents(ctx, pair[0], pair[1], []string{"link"})
			if ok || err != nil {
				t.Fatalf("%v %v", ok, err)
			}
		}
	})
	t.Run("unusable approved head", func(t *testing.T) {
		w, a := sameChangesWorkspace(t)
		h := sameChangesCommit(t, w, "other", "x\n", "other")
		for _, approved := range []string{a[:12], "base-line", strings.Repeat("0", 40), ""} {
			ok, err := w.SameFileContents(ctx, approved, h, []string{"guarded"})
			if ok || err == nil {
				t.Fatalf("%q: %v %v", approved, ok, err)
			}
		}
	})
	t.Run("nonancestor", func(t *testing.T) {
		w, a := sameChangesWorkspace(t)
		cleanupTestGit(t, w.Tree, "checkout", "base-line")
		h := sameChangesCommit(t, w, "other", "x", "other")
		ok, err := w.SameFileContents(ctx, a, h, []string{"guarded"})
		if ok || err != nil {
			t.Fatalf("%v %v", ok, err)
		}
	})
	t.Run("failure", func(t *testing.T) {
		w, a := sameChangesWorkspace(t)
		w.Config.Git = "/nonexistent/git"
		ok, err := w.SameFileContents(ctx, a, a, []string{"guarded"})
		if ok || err == nil {
			t.Fatalf("%v %v", ok, err)
		}
	})
}

type fakeDeletionEvidence struct {
	value DeletionApprovalEvidence
	err   error
}

func (f fakeDeletionEvidence) ReadDeletionApproval(context.Context, string) (DeletionApprovalEvidence, error) {
	return f.value, f.err
}

func TestDeletionApprovalEvidence(t *testing.T) {
	ctx := context.Background()
	w, _ := sameChangesWorkspace(t)
	a := sameChangesCommit(t, w, "query.sql", `DELETE FROM
 users;
DELETE FROM
 users;
`, "sql")
	base := strings.TrimSpace(cleanupTestGit(t, w.Tree, "rev-parse", a+"^"))
	// Use the actual diff output format produced by the worker.
	out, err := w.Config.Run(ctx, w.Tree, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", base, a, "--")
	if err != nil || out.Code != 0 {
		t.Fatal(err)
	}
	e := DeletionApprovalEvidence{PR: 7, Head: a, Base: base, Paths: []string{"query.sql"}, Diff: out.Out}
	got, err := w.ReadDeletionApprovalSnapshot(ctx, fakeDeletionEvidence{value: e}, a, 7)
	if err != nil || got.Diff != e.Diff || strings.Count(got.Diff, "+DELETE FROM") != 2 || !strings.Contains(got.Diff, "+ users;") || len(got.Paths) != 1 || got.Paths[0] != "query.sql" {
		t.Fatalf("%+v %v", got, err)
	}
	cases := []struct {
		name string
		e    DeletionApprovalEvidence
		err  error
	}{
		{"missing", DeletionApprovalEvidence{}, nil}, {"wrong pr", func() DeletionApprovalEvidence { x := e; x.PR = 8; return x }(), nil},
		{"wrong head", func() DeletionApprovalEvidence { x := e; x.Head = strings.Repeat("1", 40); return x }(), nil},
		{"wrong base", func() DeletionApprovalEvidence { x := e; x.Base = strings.Repeat("1", 40); return x }(), nil},
		{"missing path", func() DeletionApprovalEvidence { x := e; x.Paths = []string{"other"}; return x }(), nil},
		{"changed diff", func() DeletionApprovalEvidence { x := e; x.Diff += "+DELETE FROM x\n"; return x }(), nil},
		{"read error", e, errors.New("read")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.ReadDeletionApprovalSnapshot(ctx, fakeDeletionEvidence{tc.e, tc.err}, a, 7); err == nil {
				t.Fatal("unverifiable evidence accepted")
			}
		})
	}
}

func TestDeletionApprovalBinaryAttributeEvidence(t *testing.T) {
	ctx := context.Background()
	w, _ := sameChangesWorkspace(t)
	base := sameChangesCommit(t, w, ".gitattributes", "*.sql binary\n", "attributes")
	head := sameChangesCommit(t, w, "query.sql", "DELETE FROM\n users;\nDELETE FROM\n users;\n", "sql")
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", base, head, "--"}
	abbreviated, err := w.Config.Run(ctx, w.Tree, args...)
	if err != nil || abbreviated.Code != 0 || !strings.Contains(abbreviated.Out, "Binary files") {
		t.Fatalf("expected binary diff: %+v %v", abbreviated, err)
	}
	e := DeletionApprovalEvidence{PR: 7, Head: head, Base: base, Paths: []string{"query.sql"}, Diff: abbreviated.Out}
	if _, err := w.ReadDeletionApprovalSnapshot(ctx, fakeDeletionEvidence{value: e}, head, 7); err == nil {
		t.Fatal("abbreviated binary evidence accepted")
	}
	full, err := w.Config.Run(ctx, w.Tree, "diff", "--text", "--no-ext-diff", "--no-textconv", "--no-renames", base, head, "--")
	if err != nil || full.Code != 0 {
		t.Fatalf("full diff: %+v %v", full, err)
	}
	e.Diff = full.Out
	got, err := w.ReadDeletionApprovalSnapshot(ctx, fakeDeletionEvidence{value: e}, head, 7)
	if err != nil || got.Diff != full.Out || strings.Count(got.Diff, "+DELETE FROM") != 2 || strings.Count(got.Diff, "+ users;") != 2 || len(got.Paths) != 1 || got.Paths[0] != "query.sql" {
		t.Fatalf("full evidence: %+v %v", got, err)
	}
}
