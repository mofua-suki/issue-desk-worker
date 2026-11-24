package execution

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 結果を保存した手動マージ依頼の作業ツリーは片付け、結果の無い依頼・変更が残るツリーは残す。
func TestWorkspaceCleanupRemovesFinishedMergeRequestTrees(t *testing.T) {
	c, _ := cleanupFixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) })
	e := c.Executor
	e.Config.Git = "git"
	root := e.Config.RuntimeDir
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mirror := filepath.Join(root, "control", "repos", "owner__project.git")
	if err := os.MkdirAll(filepath.Dir(mirror), 0700); err != nil {
		t.Fatal(err)
	}
	run(root, "init", "--bare", mirror)
	head := run(root, "--git-dir", mirror, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit-tree", "-m", "initial", run(root, "--git-dir", mirror, "mktree"))
	run(root, "--git-dir", mirror, "update-ref", "refs/remotes/origin/chat/direct", head)
	tree := func(id string, result bool) string {
		t.Helper()
		path := filepath.Join(root, "workspaces", "owner__project", "request-"+id+"-merge-1")
		run(root, "--git-dir", mirror, "worktree", "add", "--detach", path, "refs/remotes/origin/chat/direct")
		p := &requestProgress{Request: monitorRequest{ID: id, Kind: "merge_manual", Repo: "owner/project", Payload: json.RawMessage(`{}`)}, Stage: "claimed"}
		if result {
			p.Stage, p.Result = "result", json.RawMessage(`{"status":"refused"}`)
		}
		if err := e.saveRequest(p); err != nil {
			t.Fatal(err)
		}
		return path
	}
	finished := tree("201-aabbccdd", true)
	running := tree("202-aabbccdd", false)
	dirty := tree("203-aabbccdd", true)
	if err := os.WriteFile(filepath.Join(dirty, "failed-test-note.txt"), []byte("keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// 対象外のリポジトリとして呼んだときは、終わった依頼のツリーも残す。
	c.Cleanup(context.Background(), []string{"owner/other"})
	if _, err := os.Lstat(finished); err != nil {
		t.Fatalf("対象外のリポジトリのツリーを消しました: %v", err)
	}
	c.Cleanup(context.Background(), []string{"owner/project"})
	if _, err := os.Lstat(finished); !os.IsNotExist(err) {
		t.Fatalf("結果を保存した依頼のツリーが残っています: %v", err)
	}
	for _, kept := range []string{running, dirty} {
		if _, err := os.Lstat(kept); err != nil {
			t.Fatalf("残すツリーが消えました: %s %v", kept, err)
		}
	}
	if registered := run(root, "--git-dir", mirror, "worktree", "list", "--porcelain"); strings.Contains(registered, finished) {
		t.Fatalf("登録が残っています: %s", registered)
	}
}

// 全体テストとパッチのPR作成の依頼が使った作業ツリーも、結果を保存したら片付ける。
func TestWorkspaceCleanupRemovesFinishedRequestOwnTrees(t *testing.T) {
	c, _ := cleanupFixture(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) })
	e := c.Executor
	e.Config.Git = "git"
	root := e.Config.RuntimeDir
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mirror := filepath.Join(root, "control", "repos", "owner__project.git")
	if err := os.MkdirAll(filepath.Dir(mirror), 0700); err != nil {
		t.Fatal(err)
	}
	run(root, "init", "--bare", mirror)
	head := run(root, "--git-dir", mirror, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit-tree", "-m", "initial", run(root, "--git-dir", mirror, "mktree"))
	run(root, "--git-dir", mirror, "update-ref", "refs/remotes/origin/main", head)
	tree := func(id, kind string, result bool) string {
		t.Helper()
		path := filepath.Join(root, "workspaces", "owner__project", "request-"+id)
		run(root, "--git-dir", mirror, "worktree", "add", "--detach", path, "refs/remotes/origin/main")
		p := &requestProgress{Request: monitorRequest{ID: id, Kind: kind, Repo: "owner/project", Payload: json.RawMessage(`{}`)}, Stage: "claimed"}
		if result {
			p.Stage, p.Result = "result", json.RawMessage(`{"status":"passed"}`)
		}
		if err := e.saveRequest(p); err != nil {
			t.Fatal(err)
		}
		return path
	}
	fullTest := tree("301-aabbccdd", "full_test", true)
	patchPR := tree("302-aabbccdd", "pr_create", true)
	running := tree("303-aabbccdd", "full_test", false)
	other := tree("304-aabbccdd", "pr_close", true)
	// 同じ依頼の依頼自身のツリーとマージのツリーは、両方とも片付ける。
	mergeOfFullTest := filepath.Join(root, "workspaces", "owner__project", "request-301-aabbccdd-merge-1")
	run(root, "--git-dir", mirror, "worktree", "add", "--detach", mergeOfFullTest, "refs/remotes/origin/main")
	c.Cleanup(context.Background(), []string{"owner/other"})
	if _, err := os.Lstat(fullTest); err != nil {
		t.Fatalf("対象外のリポジトリのツリーを消しました: %v", err)
	}
	c.Cleanup(context.Background(), []string{"owner/project"})
	for _, gone := range []string{fullTest, patchPR, mergeOfFullTest} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Fatalf("結果を保存した依頼のツリーが残っています: %s %v", gone, err)
		}
	}
	if registered := run(root, "--git-dir", mirror, "worktree", "list", "--porcelain"); strings.Contains(registered, fullTest+"\n") || strings.Contains(registered, patchPR+"\n") {
		t.Fatalf("登録が残っています: %s", registered)
	}
	for _, kept := range []string{running, other} {
		if _, err := os.Lstat(kept); err != nil {
			t.Fatalf("残すツリーが消えました: %s %v", kept, err)
		}
	}
}
