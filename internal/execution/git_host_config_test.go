package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/journal"
)

// 作業ツリーの状態の確認は、ホストのGit設定（全体の除外設定など）を読まない。
func TestTreeStateIgnoresHostGitConfig(t *testing.T) {
	for _, place := range []string{"gitconfig", "xdg"} {
		t.Run(place, func(t *testing.T) { treeStateIgnoresHostGitConfig(t, place) })
	}
}

func treeStateIgnoresHostGitConfig(t *testing.T, place string) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("gitがありません")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if place == "gitconfig" {
		if err := os.WriteFile(filepath.Join(home, "excludes"), []byte("local-note.txt\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\texcludesFile = "+filepath.Join(home, "excludes")+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.MkdirAll(filepath.Join(home, ".config", "git"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".config", "git", "ignore"), []byte("local-note.txt\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tree := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@invalid", "commit", "-q", "--allow-empty", "-m", "first"}} {
		cmd := exec.Command(git, args...)
		cmd.Dir = tree
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, "local-note.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Config: Configuration{Fleet: Fleet{Git: git}}}
	r := &journal.Record{Tree: tree}
	if _, err := e.testHead(context.Background(), r); err == nil || !strings.Contains(err.Error(), "未確定") {
		t.Fatal("ホストの除外設定で隠れた未追跡ファイルを見落としました")
	}
	changed, err := e.antigravityChanged(context.Background(), r)
	if err != nil || !changed {
		t.Fatalf("ホストの除外設定で変更を見落としました: %t %v", changed, err)
	}
}
