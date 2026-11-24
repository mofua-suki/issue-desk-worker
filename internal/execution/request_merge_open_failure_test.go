package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 開けなかったマージ依頼の作業ツリー（request-<id>-merge-*）は、登録ごと消してから返す。
func TestManualMergeOpenFailureRemovesFreshTree(t *testing.T) {
	ctx := context.Background()
	f := newPipeline(t, "approve")
	source := filepath.Join(f.root, "source")
	f.Branch = "chat/submodule"
	f.git(source, "checkout", "-b", f.Branch)
	if err := os.WriteFile(filepath.Join(source, ".gitmodules"), []byte("[submodule \"x\"]\n\tpath = x\n\turl = https://example.invalid/x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", ".gitmodules")
	f.git(source, "commit", "-m", "submodule")
	f.git(source, "push", f.bare, f.Branch)
	if _, err := f.Executor.requestMergeWorkspace(ctx, monitorRequest{ID: "125-aabbccdd", Repo: "owner/project"}, "main", f.Branch); err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("submoduleで断っていません: %v", err)
	}
	trees, err := filepath.Glob(filepath.Join(f.Executor.Config.RuntimeDir, "workspaces", "owner__project", "request-125-aabbccdd-merge-*"))
	if err != nil || len(trees) != 0 {
		t.Fatalf("作業ツリーが残っています: %v %v", trees, err)
	}
	mirror := filepath.Join(f.Executor.Config.RuntimeDir, "control", "repos", "owner__project.git")
	if registered := f.git(f.root, "--git-dir", mirror, "worktree", "list", "--porcelain"); strings.Contains(registered, "request-125-aabbccdd-merge-") {
		t.Fatalf("作業ツリーの登録が残っています: %s", registered)
	}
}
