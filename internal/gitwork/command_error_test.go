package gitwork

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 標準入力を渡すGit操作の失敗も、サブコマンドと標準エラーを示す。
func TestInputCheckedNamesSubcommandAndStderr(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("gitがありません")
	}
	t.Setenv("LC_ALL", "C")
	dir := t.TempDir()
	c := Config{Git: git}
	if out, err := c.Run(context.Background(), dir, "init", "-q"); err != nil || out.Code != 0 {
		t.Fatal(out, err)
	}
	err = c.InputChecked(context.Background(), dir, "not a patch\n", "apply", "--index", "-")
	if err == nil || !strings.Contains(err.Error(), "apply") || !strings.Contains(err.Error(), "終了コード") || !strings.Contains(err.Error(), "patch") {
		t.Fatalf("失敗の理由がありません: %v", err)
	}
}

func testRepo(t *testing.T) (Config, string, func(args ...string) string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("gitがありません")
	}
	dir := t.TempDir()
	c := Config{Git: git}
	run := func(args ...string) string {
		t.Helper()
		out, err := c.Run(context.Background(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@invalid"}, args...)...)
		if err != nil || out.Code != 0 {
			t.Fatalf("git %v: %v %s", args, err, out.Err)
		}
		return strings.TrimSpace(out.Out)
	}
	run("init", "-q", "-b", "main")
	return c, dir, run
}

// MergeTreeは、競合しないマージのtreeを返し、競合はErrConflictにする。
func TestMergeTreeTreeAndConflict(t *testing.T) {
	c, dir, run := testRepo(t)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "base\n")
	run("add", "a.txt")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")
	write("a.txt", "left\n")
	run("commit", "-q", "-am", "left")
	left := run("rev-parse", "HEAD")
	run("checkout", "-q", "-b", "right", base)
	write("b.txt", "right\n")
	run("add", "b.txt")
	run("commit", "-q", "-m", "right")
	right := run("rev-parse", "HEAD")
	if tree, err := c.MergeTree(context.Background(), dir, left, right); err != nil || !ValidSHA(tree) {
		t.Fatalf("競合しないマージ: %q %v", tree, err)
	}
	write("a.txt", "other\n")
	run("commit", "-q", "-am", "conflict")
	if _, err := c.MergeTree(context.Background(), dir, left, run("rev-parse", "HEAD")); !errors.Is(err, ErrConflict) {
		t.Fatalf("競合を示していません: %v", err)
	}
}

// Snapshotの差分は、既定ブランチの側だけで入った変更を含めない。
func TestSnapshotExcludesBaseOnlyChanges(t *testing.T) {
	c, dir, run := testRepo(t)
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		run("add", name)
		run("commit", "-q", "-m", name)
	}
	write("start.txt")
	fork := run("rev-parse", "HEAD")
	write("base-only.txt")
	run("update-ref", "refs/remotes/origin/main", run("rev-parse", "HEAD"))
	run("checkout", "-q", "-b", "work", fork)
	write("work.txt")
	s, err := (&Workspace{Config: c, Tree: dir, Default: "main"}).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Paths, ",") != "work.txt" || strings.Contains(s.Diff, "base-only") {
		t.Fatalf("既定ブランチだけの変更が混ざりました: %v", s.Paths)
	}
}
