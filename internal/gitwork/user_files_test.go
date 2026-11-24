package gitwork

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// 実行PCの利用者の ~/.config/git/ignore と attributes は、仕事のGit操作に効かない。
// 効くと、利用者が無視している名前の変更がコミットから黙って抜ける。
func TestRunIgnoresPerUserIgnoreFile(t *testing.T) {
	// 子のGitへ渡る環境はHOMEを残し、XDG_CONFIG_HOMEを落とす（process.CleanEnv）。
	home := t.TempDir()
	config := filepath.Join(home, ".config", "git")
	if err := os.MkdirAll(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "ignore"), []byte("*.txt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	tree := t.TempDir()
	cleanupTestGit(t, tree, "init", "-q")
	if err := os.WriteFile(filepath.Join(tree, "a.txt"), []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := Config{Git: "git"}.Checked(context.Background(), tree, "status", "--porcelain")
	if err != nil || out != "?? a.txt" {
		t.Fatalf("利用者の無視の一覧が効きました: %q %v", out, err)
	}
}

// 利用者の ~/.config/git/attributes も効かない。効くと、改行の扱いなどが実行PCごとに変わる。
func TestRunIgnoresPerUserAttributesFile(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, ".config", "git")
	if err := os.MkdirAll(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "attributes"), []byte("*.txt marked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	tree := t.TempDir()
	cleanupTestGit(t, tree, "init", "-q")
	out, err := Config{Git: "git"}.Checked(context.Background(), tree, "check-attr", "marked", "--", "a.txt")
	if err != nil || out != "a.txt: marked: unspecified" {
		t.Fatalf("利用者の属性の一覧が効きました: %q %v", out, err)
	}
}

// Unmerged は未解決の競合のファイルだけを返し、競合が無ければ空を返す。
func TestWorkspaceUnmergedListsConflicts(t *testing.T) {
	tree := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cleanupTestGit(t, tree, append([]string{"-c", "user.name=t", "-c", "user.email=t@invalid"}, args...)...)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tree, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("a.txt", "base\n")
	write("b.txt", "base\n")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	w := &Workspace{Config: Config{Git: "git"}, Tree: tree}
	if paths, err := w.Unmerged(context.Background()); err != nil || paths != nil {
		t.Fatalf("競合が無いのに返しました: %q %v", paths, err)
	}
	git("checkout", "-q", "-b", "other")
	write("a.txt", "other\n")
	git("commit", "-q", "-am", "other")
	git("checkout", "-q", "main")
	write("a.txt", "main\n")
	git("commit", "-q", "-am", "main")
	cmd := []string{"-c", "user.name=t", "-c", "user.email=t@invalid", "merge", "other"}
	if out, _ := (Config{Git: "git"}).Run(context.Background(), tree, cmd...); out.Code == 0 {
		t.Fatal("競合が起きませんでした")
	}
	paths, err := w.Unmerged(context.Background())
	if err != nil || !slices.Equal(paths, []string{"a.txt"}) {
		t.Fatalf("競合のファイル: %q %v", paths, err)
	}
}
