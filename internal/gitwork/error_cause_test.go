package gitwork

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// Gitの失敗は、サブコマンドと標準エラーの末尾（URLの資格情報を除く）を示す。
func TestCheckedNamesSubcommandAndStderr(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("gitがありません")
	}
	t.Setenv("LC_ALL", "C")
	_, err = Config{Git: git}.checked(context.Background(), t.TempDir(), "rev-parse", "--verify", "refs/heads/none")
	if err == nil || !strings.Contains(err.Error(), "rev-parse") || !strings.Contains(err.Error(), "終了コード") || !strings.Contains(err.Error(), "fatal") {
		t.Fatalf("失敗の理由がありません: %v", err)
	}
}

func TestGitStderrTail(t *testing.T) {
	if got := gitStderrTail("fatal: https://user:p@ss@example.invalid/r.git"); strings.Contains(got, "ss@") || !strings.Contains(got, "https://[REDACTED]@example.invalid") {
		t.Fatal(got)
	}
	got := gitStderrTail("fatal: unable to access 'https://user:secret@example.invalid/o/r.git/':\n  denied\n")
	if strings.Contains(got, "secret") || !strings.Contains(got, "https://[REDACTED]@example.invalid") || strings.Contains(got, "\n") {
		t.Fatal(got)
	}
	long := gitStderrTail(strings.Repeat("あ", 500) + " end")
	if !strings.HasSuffix(long, "end") || len(long) > gitStderrTailLimit+len("…") {
		t.Fatal(len(long))
	}
	if got := gitSubcommand([]string{"-C", "dir", "-c", "a=b", "--no-pager", "fetch", "origin"}); got != "fetch" {
		t.Fatal(got)
	}
}
