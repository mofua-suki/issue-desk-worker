package execution

import (
	"context"
	"issue-desk-worker/internal/journal"
	"os"
	"path/filepath"
	"testing"
)

func fileExecutor(t *testing.T) (*Executor, *journal.Record, string) {
	t.Helper()
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	_ = os.MkdirAll(tree, 0700)
	store, err := journal.Open(filepath.Join(root, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e := &Executor{Store: store}
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 1, Tree: tree, Provider: "claude", Result: encode(map[string]any{"status": "completed"})}
	return e, r, root
}
func writeFixture(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestDeleteRequestsValidateWholeSetAndRejectEscapes(t *testing.T) {
	for _, bad := range []string{"link/secret", "sub/../.git/config", "../private/secret", ".claude/worktrees/item"} {
		t.Run(bad, func(t *testing.T) {
			e, r, root := fileExecutor(t)
			writeFixture(t, filepath.Join(r.Tree, "good"), "keep")
			writeFixture(t, filepath.Join(root, "private", "secret"), "outside")
			writeFixture(t, filepath.Join(r.Tree, ".git", "config"), "metadata")
			_ = os.Symlink(filepath.Join(root, "private"), filepath.Join(r.Tree, "link"))
			if err := e.applyDeleteRequests(context.Background(), r, []string{"good", bad}); err == nil {
				t.Fatal("不正削除を許可しました")
			}
			for name, want := range map[string]string{filepath.Join(r.Tree, "good"): "keep", filepath.Join(root, "private", "secret"): "outside", filepath.Join(r.Tree, ".git", "config"): "metadata"} {
				raw, err := os.ReadFile(name)
				if err != nil || string(raw) != want {
					t.Fatalf("全件検証前に変更しました: %s", name)
				}
			}
		})
	}
}
func TestDeleteRequestIsAnchoredAgainstAncestorReplacement(t *testing.T) {
	_, r, root := fileExecutor(t)
	writeFixture(t, filepath.Join(r.Tree, "sub", "item"), "owned")
	writeFixture(t, filepath.Join(root, "outside", "item"), "keep")
	target, err := openSafeTarget(r.Tree, "sub/item", false)
	if err != nil {
		t.Fatal(err)
	}
	defer target.close()
	if err := os.Rename(filepath.Join(r.Tree, "sub"), filepath.Join(r.Tree, "original")); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(filepath.Join(root, "outside"), filepath.Join(r.Tree, "sub"))
	if err := target.remove(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "outside", "item"))
	if err != nil || string(raw) != "keep" {
		t.Fatal("祖先差替で作業ツリー外を削除しました")
	}
}
func TestDeleteRequestsPreserveScopeStopAndDurableHistory(t *testing.T) {
	e, r, _ := fileExecutor(t)
	writeFixture(t, filepath.Join(r.Tree, "ok"), "delete")
	r.ControllerAllowed = []string{"different"}
	if err := e.applyDeleteRequests(context.Background(), r, []string{"ok"}); err == nil {
		t.Fatal("許容範囲外を削除しました")
	}
	r.ControllerAllowed = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.applyDeleteRequests(ctx, r, []string{"ok"}); err == nil {
		t.Fatal("停止後に削除しました")
	}
	if err := e.applyDeleteRequests(context.Background(), r, []string{"ok"}); err != nil {
		t.Fatal(err)
	}
	saved, err := e.Store.Load(1)
	if err != nil || len(saved.ControllerChanges) != 2 {
		t.Fatal("削除履歴を保存していません")
	}
}
func TestClaudeStagedAppliesValidatedSetAndRecordsIt(t *testing.T) {
	e, r, _ := fileExecutor(t)
	writeFixture(t, filepath.Join(r.Tree, ".claude", "old.txt"), "old")
	writeFixture(t, filepath.Join(r.Tree, ".claude-staged", "hooks", "check.sh"), "new")
	writeFixture(t, filepath.Join(r.Tree, ".claude-staged", ".delete"), ".claude/old.txt\n")
	if err := e.applyRequests(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(r.Tree, ".claude", "hooks", "check.sh"))
	if err != nil || string(raw) != "new" {
		t.Fatal("完成したstageを反映しませんでした")
	}
	if _, err := os.Stat(filepath.Join(r.Tree, ".claude", "old.txt")); !os.IsNotExist(err) {
		t.Fatal("検証したstage削除を反映しませんでした")
	}
	if _, err := os.Stat(filepath.Join(r.Tree, ".claude-staged")); !os.IsNotExist(err) {
		t.Fatal("stageを残しました")
	}
	if len(r.ControllerChanges) != 4 {
		t.Fatal("stage履歴を保存しませんでした")
	}
}
func TestClaudeStagedRejectsWholeSetAndUncompletedChanges(t *testing.T) {
	for _, kind := range []string{"outside", "link", "duplicate", "scope", "question", "worktrees"} {
		t.Run(kind, func(t *testing.T) {
			e, r, root := fileExecutor(t)
			writeFixture(t, filepath.Join(r.Tree, ".claude-staged", "good.txt"), "new")
			switch kind {
			case "outside":
				writeFixture(t, filepath.Join(r.Tree, ".claude-staged", ".delete"), "../outside\n")
			case "link":
				writeFixture(t, filepath.Join(root, "outside"), "keep")
				_ = os.Symlink(filepath.Join(root, "outside"), filepath.Join(r.Tree, ".claude-staged", "bad"))
			case "duplicate":
				writeFixture(t, filepath.Join(r.Tree, ".claude-staged", ".delete"), ".claude/good.txt\n")
			case "scope":
				r.ControllerAllowed = []string{".claude/other"}
			case "question":
				r.Result = encode(map[string]any{"status": "question"})
			case "worktrees":
				writeFixture(t, filepath.Join(r.Tree, ".claude-staged", "worktrees", "item"), "bad")
			}
			if err := e.applyClaudeStaged(context.Background(), r); err == nil {
				t.Fatal("不正stageを反映しました")
			}
			if _, err := os.Stat(filepath.Join(r.Tree, ".claude", "good.txt")); !os.IsNotExist(err) {
				t.Fatal("全件検証前にstageを一部反映しました")
			}
		})
	}
}
