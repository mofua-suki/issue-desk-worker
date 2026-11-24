package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/journal"
)

// pnpmの導入と依存の追加が失敗したら、終了コードとpnpmの出力の末尾を残し、何が失敗したか調べられるようにする。
func TestPNPMFailuresKeepOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "workspaces"), 0700); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(dir, "tree")
	if err := os.Mkdir(tree, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "package.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	sandbox := filepath.Join(dir, "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	pnpm := filepath.Join(dir, "pnpm")
	if err := os.WriteFile(pnpm, []byte("#!/bin/sh\necho \"ERR_PNPM_$1_FAILED\" >&2\nexit 3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Config: Configuration{Host: "pc-one", Fleet: Fleet{RuntimeDir: dir, Codex: sandbox, PNPM: pnpm, TestTimeoutSeconds: 10}}}
	r := &journal.Record{Tree: tree, Result: json.RawMessage(`{"addDependencies":[{"name":"left-pad","version":"1.3.0"}]}`)}
	err := e.setup(context.Background(), Repository{}, r)
	if err == nil || !strings.Contains(err.Error(), "固定された依存の導入が失敗しました (終了コード 3)") || !strings.Contains(err.Error(), "ERR_PNPM_install_FAILED") {
		t.Fatalf("導入の失敗の中身がありません: %v", err)
	}
	err = e.applyImplementationRequests(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "依存left-padの追加が拒否または失敗しました (終了コード 3)") || !strings.Contains(err.Error(), "ERR_PNPM_add_FAILED") {
		t.Fatalf("追加の失敗の中身がありません: %v", err)
	}
}

// 依存設定の誤りは、どのファイルかを作業ツリーからの相対パスで示す。
func TestDependencyPolicyNamesFile(t *testing.T) {
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "packages", "a"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "packages", "a", ".npmrc"), []byte("minimum-release-age-exclude=x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := dependencyPolicy(tree)
	if err == nil || !strings.Contains(err.Error(), filepath.Join("packages", "a", ".npmrc")) || strings.Contains(err.Error(), tree) {
		t.Fatalf("ファイルが分かりません: %v", err)
	}
}
