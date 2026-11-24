package gitwork

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const emptyDiffFingerprint = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// A PR branch whose base is "trunk" in the deployed worktree layout.
func fingerprintWorkspace(t *testing.T) (*Workspace, string) {
	t.Helper()
	c, mirror, tree := cleanupTestWorkspace(t)
	c.User = "Fixture"
	w := &Workspace{Config: c, Tree: tree, Mirror: mirror, Default: "trunk"}
	base := sameChangesCommit(t, w, "keep.txt", "keep\n", "base")
	cleanupTestGit(t, tree, "update-ref", "refs/remotes/origin/trunk", base)
	cleanupTestGit(t, tree, "branch", "base-line", base)
	return w, base
}

func fingerprintOf(t *testing.T, w *Workspace) string {
	t.Helper()
	head, err := w.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := DiffFingerprint(context.Background(), w.Config, w.Tree, "refs/remotes/origin/trunk", head)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The fixed example in the intake's API description.
func TestDiffFingerprintMatchesIntakeExample(t *testing.T) {
	w, _ := fingerprintWorkspace(t)
	sameChangesCommit(t, w, "a.txt", "x\n", "add a")
	if got, want := fingerprintOf(t, w), "sha256:85b96e7304348fab90523950169bc034c4a66fa8e30896f65c093b1b1daab191"; got != want {
		t.Fatalf("受付の固定例と違う指紋です: %s", got)
	}
}

func TestDiffFingerprintOfEmptyDiff(t *testing.T) {
	w, _ := fingerprintWorkspace(t)
	if got := fingerprintOf(t, w); got != emptyDiffFingerprint {
		t.Fatalf("空差分の指紋が違います: %s", got)
	}
}

func TestDiffFingerprintTreatsRenameAsDeleteAndAdd(t *testing.T) {
	w, _ := fingerprintWorkspace(t)
	cleanupTestGit(t, w.Tree, "mv", "keep.txt", "moved.txt")
	head, err := w.Commit(context.Background(), "rename")
	if err != nil {
		t.Fatal(err)
	}
	got, err := DiffFingerprint(context.Background(), w.Config, w.Tree, "refs/remotes/origin/trunk", head)
	if err != nil {
		t.Fatal(err)
	}
	want, err := FingerprintEntries([]DiffFingerprintEntry{
		{Path: []byte("moved.txt"), OldMode: "000000", NewMode: "100644", NewContent: []byte("keep\n")},
		{Path: []byte("keep.txt"), OldMode: "100644", NewMode: "000000", OldContent: []byte("keep\n")},
	})
	if err != nil || got != want {
		t.Fatalf("renameを削除と追加として扱っていません: %s %s %v", got, want, err)
	}
}

// Taking in the default branch changes head but not the PR's own change.
func TestDiffFingerprintIgnoresMergedDefaultBranch(t *testing.T) {
	w, _ := fingerprintWorkspace(t)
	sameChangesCommit(t, w, "feature.txt", "feature\n", "feature")
	before := fingerprintOf(t, w)
	sameChangesFollow(t, w, "keep.txt", "trunk moved\n")
	if got := fingerprintOf(t, w); got != before {
		t.Fatalf("既定ブランチの取り込みだけで指紋が変わりました: %s %s", before, got)
	}
	sameChangesCommit(t, w, "feature.txt", "feature changed\n", "edit")
	if got := fingerprintOf(t, w); got == before {
		t.Fatal("PR自身の変更で指紋が変わりません")
	}
}

func TestDiffFingerprintRequiresMergeBase(t *testing.T) {
	w, _ := fingerprintWorkspace(t)
	cleanupTestGit(t, w.Tree, "checkout", "--orphan", "unrelated")
	if err := os.WriteFile(filepath.Join(w.Tree, "other.txt"), []byte("other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupTestGit(t, w.Tree, "rm", "-q", "--cached", "keep.txt")
	cleanupTestGit(t, w.Tree, "add", "other.txt")
	cleanupTestGit(t, w.Tree, "-c", "user.name=F", "-c", "user.email=f@example.invalid", "commit", "-q", "-m", "unrelated")
	head := strings.TrimSpace(cleanupTestGit(t, w.Tree, "rev-parse", "HEAD"))
	if got, err := DiffFingerprint(context.Background(), w.Config, w.Tree, "refs/remotes/origin/trunk", head); err == nil {
		t.Fatalf("merge-baseが無いのに指紋を返しました: %s", got)
	}
}

// Criss-cross merges leave two merge bases; the change is then ambiguous.
func TestDiffFingerprintRejectsAmbiguousMergeBase(t *testing.T) {
	w, _ := fingerprintWorkspace(t)
	tree := w.Tree
	git := func(args ...string) string {
		return strings.TrimSpace(cleanupTestGit(t, tree, append([]string{"-c", "user.name=F", "-c", "user.email=f@example.invalid"}, args...)...))
	}
	branch := git("branch", "--show-current")
	sameChangesCommit(t, w, "left.txt", "left\n", "left")
	left := git("rev-parse", "HEAD")
	git("checkout", "-q", "base-line")
	sameChangesCommit(t, w, "right.txt", "right\n", "right")
	right := git("rev-parse", "HEAD")
	git("merge", "-q", "--no-edit", left)
	trunk := git("rev-parse", "HEAD")
	git("checkout", "-q", branch)
	git("merge", "-q", "--no-edit", right)
	head := git("rev-parse", "HEAD")
	git("update-ref", "refs/remotes/origin/trunk", trunk)
	if got, err := DiffFingerprint(context.Background(), w.Config, tree, "refs/remotes/origin/trunk", head); err == nil {
		t.Fatalf("merge-baseが複数あるのに指紋を返しました: %s", got)
	}
}

// Deletion, mode-only change and a nested addition, against a hash computed
// outside this implementation.
func TestDiffFingerprintOfSeveralChanges(t *testing.T) {
	w, _ := fingerprintWorkspace(t)
	sameChangesCommit(t, w, "a.txt", "x\n", "a")
	start := sameChangesCommit(t, w, "b.txt", "b\n", "b")
	cleanupTestGit(t, w.Tree, "update-ref", "refs/remotes/origin/trunk", start)
	cleanupTestGit(t, w.Tree, "rm", "-q", "a.txt")
	cleanupTestGit(t, w.Tree, "update-index", "--chmod=+x", "b.txt")
	if err := os.MkdirAll(filepath.Join(w.Tree, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Tree, "dir", "z.txt"), []byte("z\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupTestGit(t, w.Tree, "add", "dir/z.txt")
	cleanupTestGit(t, w.Tree, "-c", "user.name=F", "-c", "user.email=f@example.invalid", "commit", "-q", "-m", "several")
	if got, want := fingerprintOf(t, w), "sha256:9dd2444f129f121678ca0cfd9004ea20d0eb2fe87e49737134d14fbd473a4112"; got != want {
		t.Fatalf("複数の変更の指紋が違います: %s", got)
	}
}

const sharedLines = "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"

// A PR branch whose default branch already holds a file of several lines.
func fingerprintSharedFile(t *testing.T) *Workspace {
	t.Helper()
	w, _ := fingerprintWorkspace(t)
	start := sameChangesCommit(t, w, "shared.txt", sharedLines, "shared")
	cleanupTestGit(t, w.Tree, "update-ref", "refs/remotes/origin/trunk", start)
	cleanupTestGit(t, w.Tree, "branch", "-f", "base-line", start)
	return w
}

// The default branch edits another line of the file the PR changes. Taking
// it in rewrites the whole file, but the PR's own change is the same.
func TestDiffFingerprintKeepsChangeWhenDefaultBranchEditsSameFile(t *testing.T) {
	w := fingerprintSharedFile(t)
	sameChangesCommit(t, w, "shared.txt", strings.Replace(sharedLines, "ten", "TEN", 1), "own")
	before := fingerprintOf(t, w)
	sameChangesFollow(t, w, "shared.txt", strings.Replace(sharedLines, "one", "ONE", 1))
	sameChangesFollow(t, w, "shared.txt", strings.Replace(sharedLines, "one", "First", 1))
	if got := fingerprintOf(t, w); got != before {
		t.Fatalf("既定ブランチが同じファイルを変えただけで指紋が変わりました: %s %s", before, got)
	}
	sameChangesCommit(t, w, "shared.txt", strings.Replace(strings.Replace(sharedLines, "one", "First", 1), "ten", "Ten", 1), "edit")
	if got := fingerprintOf(t, w); got == before {
		t.Fatal("取り込みの後のPR自身の変更で指紋が変わりません")
	}
}

// Taking in the default branch and then moving the PR's edit to another
// place of the file is a different change, though its lines read the same.
func TestDiffFingerprintChangesWhenSameEditMovesAfterFollow(t *testing.T) {
	const base = "[test]\nskip_auth = false\nname = t\n[prod]\nskip_auth = false\nname = p\n"
	w, _ := fingerprintWorkspace(t)
	start := sameChangesCommit(t, w, "shared.txt", base, "shared")
	cleanupTestGit(t, w.Tree, "update-ref", "refs/remotes/origin/trunk", start)
	cleanupTestGit(t, w.Tree, "branch", "-f", "base-line", start)
	sameChangesCommit(t, w, "shared.txt", strings.Replace(base, "skip_auth = false", "skip_auth = true", 1), "own")
	before := fingerprintOf(t, w)
	sameChangesFollow(t, w, "keep.txt", "trunk moved\n")
	sameChangesCommit(t, w, "shared.txt", "[test]\nskip_auth = false\nname = t\n[prod]\nskip_auth = true\nname = p\n", "moved")
	sameChangesFollow(t, w, "keep.txt", "trunk moved again\n")
	if got := fingerprintOf(t, w); got == before {
		t.Fatal("同じ文面の変更を別の場所へ移しても指紋が変わりません")
	}
}

// A merge of the default branch that also carries a hand edit is not just
// the default branch taken in.
func TestDiffFingerprintChangesWhenFollowMergeIsEdited(t *testing.T) {
	w := fingerprintSharedFile(t)
	sameChangesCommit(t, w, "shared.txt", strings.Replace(sharedLines, "ten", "TEN", 1), "own")
	before := fingerprintOf(t, w)
	sameChangesFollow(t, w, "shared.txt", strings.Replace(sharedLines, "one", "ONE", 1))
	if err := os.WriteFile(filepath.Join(w.Tree, "extra.txt"), []byte("extra\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupTestGit(t, w.Tree, "add", "extra.txt")
	cleanupTestGit(t, w.Tree, "-c", "user.name=F", "-c", "user.email=f@example.invalid", "commit", "-q", "--amend", "--no-edit")
	if got := fingerprintOf(t, w); got == before {
		t.Fatal("取り込みのマージに手で足した変更を見落としました")
	}
}

// A conflict resolved while taking in the default branch is a new decision.
func TestDiffFingerprintChangesWhenFollowNeededConflictResolved(t *testing.T) {
	w := fingerprintSharedFile(t)
	sameChangesCommit(t, w, "shared.txt", strings.Replace(sharedLines, "ten", "TEN", 1), "own")
	before := fingerprintOf(t, w)
	git := func(args ...string) {
		t.Helper()
		cleanupTestGit(t, w.Tree, append([]string{"-c", "user.name=F", "-c", "user.email=f@example.invalid"}, args...)...)
	}
	git("checkout", "-q", "base-line")
	moved := sameChangesCommit(t, w, "shared.txt", strings.Replace(sharedLines, "ten", "Tenth", 1), "trunk moves")
	git("checkout", "-q", "-")
	git("update-ref", "refs/remotes/origin/trunk", moved)
	git("merge", "-q", "--no-edit", "-X", "ours", "refs/remotes/origin/trunk")
	if got := fingerprintOf(t, w); got == before {
		t.Fatal("競合を解いた取り込みを、取り込みだけと扱いました")
	}
}

// Merging some other branch into the PR is not taking in the default branch.
func TestDiffFingerprintChangesWhenOtherBranchIsMerged(t *testing.T) {
	w, _ := fingerprintWorkspace(t)
	sameChangesCommit(t, w, "feature.txt", "feature\n", "feature")
	before := fingerprintOf(t, w)
	git := func(args ...string) {
		t.Helper()
		cleanupTestGit(t, w.Tree, append([]string{"-c", "user.name=F", "-c", "user.email=f@example.invalid"}, args...)...)
	}
	git("checkout", "-q", "-b", "other", "base-line")
	sameChangesCommit(t, w, "other.txt", "other\n", "other")
	git("checkout", "-q", "-")
	git("merge", "-q", "--no-edit", "other")
	if got := fingerprintOf(t, w); got == before {
		t.Fatal("既定ブランチに無い枝のマージを、取り込みだけと扱いました")
	}
}

// A conflicted merge committed with its conflict markers is not the default
// branch taken in.
func TestDiffFingerprintChangesWhenConflictMarkersAreCommitted(t *testing.T) {
	w := fingerprintSharedFile(t)
	sameChangesCommit(t, w, "shared.txt", strings.Replace(sharedLines, "ten", "TEN", 1), "own")
	before := fingerprintOf(t, w)
	git := func(args ...string) {
		t.Helper()
		cleanupTestGit(t, w.Tree, append([]string{"-c", "user.name=F", "-c", "user.email=f@example.invalid"}, args...)...)
	}
	git("checkout", "-q", "base-line")
	moved := sameChangesCommit(t, w, "shared.txt", strings.Replace(sharedLines, "ten", "Tenth", 1), "trunk moves")
	git("checkout", "-q", "-")
	git("update-ref", "refs/remotes/origin/trunk", moved)
	merge := exec.Command("git", "-c", "user.name=F", "-c", "user.email=f@example.invalid", "merge", "--no-edit", "refs/remotes/origin/trunk")
	merge.Dir = w.Tree
	if err := merge.Run(); err == nil {
		t.Fatal("競合するはずの取り込みが成功しました")
	}
	git("add", "shared.txt")
	git("commit", "-q", "--no-edit")
	if got := fingerprintOf(t, w); got == before {
		t.Fatal("競合の印を残したマージを、取り込みだけと扱いました")
	}
}

// A clean follow on top of an edited merge does not hide the edit.
func TestDiffFingerprintChangesWhenFollowComesAfterEditedMerge(t *testing.T) {
	w := fingerprintSharedFile(t)
	sameChangesCommit(t, w, "shared.txt", strings.Replace(sharedLines, "ten", "TEN", 1), "own")
	before := fingerprintOf(t, w)
	sameChangesFollow(t, w, "shared.txt", strings.Replace(sharedLines, "one", "ONE", 1))
	if err := os.WriteFile(filepath.Join(w.Tree, "extra.txt"), []byte("extra\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupTestGit(t, w.Tree, "add", "extra.txt")
	cleanupTestGit(t, w.Tree, "-c", "user.name=F", "-c", "user.email=f@example.invalid", "commit", "-q", "--amend", "--no-edit")
	sameChangesFollow(t, w, "shared.txt", strings.Replace(sharedLines, "one", "First", 1))
	if got := fingerprintOf(t, w); got == before {
		t.Fatal("手で編集したマージの後の取り込みで、編集を見落としました")
	}
}
