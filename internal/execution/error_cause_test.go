package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"issue-desk-worker/internal/jobspec"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

func captureStderr(t *testing.T, run func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = write
	func() { defer func() { os.Stderr = stderr }(); run() }()
	_ = write.Close()
	log, _ := io.ReadAll(read)
	_ = read.Close()
	return string(log)
}

// 変更が残る作業ツリーは意図して残すので、毎周期ではなく1回だけ記録する。
func TestWorkspaceCleanupLogsKeptDirtyTreeOnce(t *testing.T) {
	f := newPipeline(t, "approve")
	e := f.Executor
	w, err := gitwork.Open(context.Background(), gitwork.Config{Git: e.Config.Git, Root: e.Config.RuntimeDir, BaseURL: e.Config.GiteaURL, TokenFile: e.Config.WorkerTokenFile, Executable: e.Executable}, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Tree, "README.md"), []byte("dirty"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.Save(&journal.Record{ID: 1, Repo: w.Repo, Issue: 7, Tree: w.Tree, Stage: "done", FinalReported: true}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		io.WriteString(out, `{"number":7,"state":"closed"}`)
	}))
	defer server.Close()
	if e.Gitea, err = gitea.New(server.URL, "fixture-token"); err != nil {
		t.Fatal(err)
	}
	c := &WorkspaceCleaner{Executor: e}
	log := captureStderr(t, func() {
		for i := 0; i < 3; i++ {
			c.Cleanup(context.Background(), []string{w.Repo})
		}
	})
	if strings.Count(log, "変更が残っているため残します") != 1 || strings.Contains(log, "片付けに失敗しました（作業ツリーの削除") {
		t.Fatalf("残したツリーの記録が1回ではありません: %q", log)
	}
	if _, err := os.Stat(w.Tree); err != nil {
		t.Fatal("変更が残るツリーを消しました", err)
	}
}

func TestCleanupCauseKeepsKindWithoutPaths(t *testing.T) {
	for err, want := range map[error]string{
		&gitea.Error{Status: 503, Message: "secret", Call: "GET /repos/owner/project"}: "Gitea APIがHTTP 503を返しました",
		&gitea.UnreachableError{Call: "GET /repos/owner/project", Kind: "時間切れ"}:        "Giteaへ接続できません（時間切れ）",
		fmt.Errorf("x: %w", context.DeadlineExceeded):                                  "時間切れ",
		&journal.CorruptError{ID: 1, Path: "/private/state/job-1.json", Cause: "x"}:    "保存した記録が壊れています",
		&fs.PathError{Op: "lstat", Path: "/private/tree", Err: fs.ErrPermission}:       "lstat: permission denied",
		errors.New("片付け対象のGit登録を確認できません"):                                              "片付け対象のGit登録を確認できません",
		errors.New("open /private/tree: x"):                                            "詳細はパスを含むため記録しません",
	} {
		if got := cleanupCause(err); got != want {
			t.Errorf("%v: %s（期待: %s）", err, got, want)
		}
	}
}

// 記述子の不正は、どの条件で断ったかを示す。
func TestDescriptorProblemNamesCheck(t *testing.T) {
	for want, job := range map[string]intake.Job{
		"JSON":    {ID: 1, LeaseEpoch: 1, Raw: json.RawMessage(`{`)},
		"仕事番号":    {ID: 1, LeaseEpoch: 1, Raw: json.RawMessage(`{"id":2,"issue":7}`)},
		"Issue番号": {ID: 1, LeaseEpoch: 1, Raw: json.RawMessage(`{"id":1}`)},
		"epoch":   {ID: 1, Raw: json.RawMessage(`{"id":1,"issue":7}`)},
	} {
		var d Descriptor
		if err := descriptorProblem(job, &d); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	var d Descriptor
	if err := descriptorProblem(intake.Job{ID: 1, LeaseEpoch: 1, Raw: json.RawMessage(`{"id":1,"issue":7}`)}, &d); err != nil || d.Issue != 7 {
		t.Fatal(err)
	}
}

// 競合ファイルを確かめられないときは、gitの終了コードと標準エラーを示す。
func TestConflictCheckNamesGitFailure(t *testing.T) {
	f := newPipeline(t, "approve")
	w := &gitwork.Workspace{Config: gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir}, Tree: t.TempDir()}
	t.Setenv("LC_ALL", "C")
	body := ""
	_, err := f.Executor.resumeMergeConflict(context.Background(), Descriptor{Descriptor: jobspec.Descriptor{ID: 1}}, &journal.Record{ID: 1}, w, &body)
	if err == nil || !strings.Contains(err.Error(), "diff") || !strings.Contains(err.Error(), "終了コード") {
		t.Fatalf("gitの失敗の理由がありません: %v", err)
	}
}
