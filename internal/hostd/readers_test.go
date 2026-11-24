package hostd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"issue-desk-worker/internal/process"
)

// readerFixture は、originと読む側のcloneを作り、originへ新しいコミットを1つ足す。
func readerFixture(t *testing.T) (clone, previous, next string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("gitがありません")
	}
	for _, tool := range []string{"/bin/true", "/bin/false"} {
		if _, err := os.Stat(tool); err != nil {
			t.Skip(tool + "がありません")
		}
	}
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@invalid"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	origin := filepath.Join(root, "owner", "app.git")
	work := filepath.Join(root, "work")
	clone = filepath.Join(root, "reader")
	run(root, "init", "-q", "--bare", "-b", "main", origin)
	run(root, "clone", "-q", origin, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "a.txt")
	run(work, "commit", "-q", "-m", "first")
	run(work, "push", "-q", "origin", "HEAD:main")
	previous = run(work, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run(work, "commit", "-q", "-am", "second")
	run(work, "push", "-q", "origin", "HEAD:main")
	next = run(work, "rev-parse", "HEAD")
	// 読む側は前のコミットにいて、origin/mainは取得済みにしておく（hostdはfile://の取得を許さないため）。
	run(root, "clone", "-q", origin, clone)
	run(clone, "reset", "-q", "--hard", previous)
	return clone, previous, next
}

func readerRuntime(t *testing.T, path, commit string, check []string) (*Runtime, func() []readerResult) {
	t.Helper()
	return readerRuntimeJobs(t, path, []readerJob{{Repo: "owner/app", Commit: commit}}, check)
}

// readerRuntimeJobs は、受付が jobs を配る読む側の Runtime を作る。読む側の設定は owner/app だけ。
func readerRuntimeJobs(t *testing.T, path string, jobs []readerJob, check []string) (*Runtime, func() []readerResult) {
	t.Helper()
	var mu sync.Mutex
	var sent []readerResult
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/reader-next"):
			json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
		case strings.HasSuffix(r.URL.Path, "/reader-result"):
			var result readerResult
			_ = json.NewDecoder(r.Body).Decode(&result)
			mu.Lock()
			sent = append(sent, result)
			mu.Unlock()
			w.Write([]byte(`{}`))
		default:
			t.Errorf("予期しない経路: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	r := &Runtime{Config: Configuration{URL: server.URL, Host: "pc", Home: t.TempDir(), Readers: []Reader{{Repo: "owner/app", Path: path, CheckCommand: check}}}, HTTP: server.Client(), Run: func(ctx context.Context, name string, args []string, options process.Options) (process.Result, error) {
		// 取得済みのoriginを使い、取得だけは成功したことにする。
		if name == "git" && slices.Contains(args, "fetch") {
			return process.Result{Started: true}, nil
		}
		return process.Run(ctx, name, args, options)
	}}
	return r, func() []readerResult {
		mu.Lock()
		defer mu.Unlock()
		return append([]readerResult(nil), sent...)
	}
}

// 読む側は新しいコミットへ早送りし、確認コマンドが通れば synced、通らなければ前のコミットへ戻す。
// 戻した後は確認コマンドを流し直さず、作業ツリーが前のコミットどおりなら rollback_healthy とする。
func TestRunReadersSyncsOrRollsBack(t *testing.T) {
	for name, tc := range map[string]struct {
		check          []string
		status, reason string
		head           func(previous, next string) string
	}{
		"synced":      {[]string{"/bin/true"}, "synced", "healthy", func(_, next string) string { return next }},
		"rolled_back": {[]string{"/bin/false"}, "rolled_back", "rollback_healthy", func(previous, _ string) string { return previous }},
	} {
		t.Run(name, func(t *testing.T) {
			clone, previous, next := readerFixture(t)
			r, sent := readerRuntime(t, clone, next, tc.check)
			if err := r.runReaders(context.Background()); err != nil {
				t.Fatal(err)
			}
			results := sent()
			if len(results) != 1 || results[0].Status != tc.status || results[0].Reason != tc.reason {
				t.Fatalf("結果: %+v", results)
			}
			head, err := r.git(context.Background(), clone, "rev-parse", "HEAD")
			if err != nil || head != tc.head(previous, next) {
				t.Fatalf("HEAD: %s %v", head, err)
			}
			want := "2\n"
			if tc.status == "rolled_back" {
				want = "1\n"
			}
			if data, err := os.ReadFile(filepath.Join(clone, "a.txt")); err != nil || string(data) != want {
				t.Fatalf("作業ツリーの中身: %q %v", data, err)
			}
			if dirty, err := r.git(context.Background(), clone, "status", "--porcelain"); err != nil || dirty != "" {
				t.Fatalf("変更が残りました: %q %v", dirty, err)
			}
		})
	}
}

// 変更が残る読む側は触らず、dirtyとして送る。
func TestRunReadersSkipsDirtyCheckout(t *testing.T) {
	clone, previous, next := readerFixture(t)
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, sent := readerRuntime(t, clone, next, nil)
	if err := r.runReaders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if results := sent(); len(results) != 1 || results[0].Status != "skipped" || results[0].Reason != "dirty" {
		t.Fatalf("結果: %+v", results)
	}
	if head, _ := r.git(context.Background(), clone, "rev-parse", "HEAD"); head != previous {
		t.Fatalf("変更が残る読む側を動かしました: %s", head)
	}
	if data, err := os.ReadFile(filepath.Join(clone, "a.txt")); err != nil || string(data) != "local\n" {
		t.Fatalf("手元の変更を消しました: %q %v", data, err)
	}
}

// 確認コマンドの設定が不正なときは、取り込む前に断り、読む側を動かさない。
func TestRunReadersRefusesBadCheckBeforeMoving(t *testing.T) {
	clone, previous, next := readerFixture(t)
	r, sent := readerRuntime(t, clone, next, []string{""})
	if err := r.runReaders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if results := sent(); len(results) != 1 || results[0].Status != "failed" || results[0].Reason != "preflight_failed" {
		t.Fatalf("結果: %+v", results)
	}
	if head, _ := r.git(context.Background(), clone, "rev-parse", "HEAD"); head != previous {
		t.Fatalf("不正な設定のまま取り込みました: %s", head)
	}
}

// 取得の後もコミットが無いときは、分岐（diverged）ではなく失敗として送る。
func TestRunReadersReportsMissingCommitAsFailure(t *testing.T) {
	clone, previous, _ := readerFixture(t)
	r, sent := readerRuntime(t, clone, strings.Repeat("c", 40), nil)
	if err := r.runReaders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if results := sent(); len(results) != 1 || results[0].Status != "failed" || results[0].Reason != "preflight_failed" {
		t.Fatalf("結果: %+v", results)
	}
	if head, _ := r.git(context.Background(), clone, "rev-parse", "HEAD"); head != previous {
		t.Fatalf("読む側を動かしました: %s", head)
	}
}

// 相対パスの読む側は、何もせずに設定の誤りとして送る。
func TestRunReadersRefusesRelativePath(t *testing.T) {
	_, _, next := readerFixture(t)
	r, sent := readerRuntime(t, "relative/reader", next, nil)
	if err := r.runReaders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if results := sent(); len(results) != 1 || results[0].Status != "failed" || results[0].Reason != "preflight_failed" {
		t.Fatalf("結果: %+v", results)
	}
}

// 重複した設定のジョブは失敗として送り、後のジョブを止めない。
func TestRunReadersReportsDuplicateSettingWithoutStopping(t *testing.T) {
	clone, previous, next := readerFixture(t)
	r, sent := readerRuntimeJobs(t, clone, []readerJob{{Repo: "owner/app", Commit: next}, {Repo: "owner/other", Commit: next}}, nil)
	r.Config.Readers = append(r.Config.Readers, r.Config.Readers[0])
	if err := r.runReaders(context.Background()); err != nil {
		t.Fatalf("ジョブ1件の誤りで周期を止めました: %v", err)
	}
	results := sent()
	if len(results) != 2 || results[0].Status != "failed" || results[0].Reason != "preflight_failed" || results[1].Repo != "owner/other" || results[1].Reason != "not_allowed" {
		t.Fatalf("結果: %+v", results)
	}
	if head, _ := r.git(context.Background(), clone, "rev-parse", "HEAD"); head != previous {
		t.Fatalf("読む側を動かしました: %s", head)
	}
}

// 不正なコミットのジョブは保存も送信もせずに飛ばし、後のジョブは同期する。
func TestRunReadersSkipsInvalidCommitAndSyncsOthers(t *testing.T) {
	clone, _, next := readerFixture(t)
	r, sent := readerRuntimeJobs(t, clone, []readerJob{{Repo: "owner/app", Commit: "bad"}, {Repo: "owner/app", Commit: next}}, nil)
	if err := r.runReaders(context.Background()); err == nil || !strings.Contains(err.Error(), "コミットが不正") {
		t.Fatalf("不正なコミットを知らせません: %v", err)
	}
	if results := sent(); len(results) != 1 || results[0].Commit != next || results[0].Status != "synced" {
		t.Fatalf("結果: %+v", results)
	}
	var pending *readerResult
	if err := r.load("reader-state.json", &pending); err != nil || pending != nil {
		t.Fatalf("結果が保存に残りました: %+v %v", pending, err)
	}
}

// 中断の状態を保存した後に早送りが失敗したとき、読む側が動いていればinterruptedと前のコミットを残し、
// 動いていなければ取り込み前の失敗として送る。
func TestRunReadersMergeFailureReportsWhetherMoved(t *testing.T) {
	for name, moved := range map[string]bool{"動いた": true, "動いていない": false} {
		t.Run(name, func(t *testing.T) {
			clone, previous, next := readerFixture(t)
			r, sent := readerRuntime(t, clone, next, nil)
			run := r.Run
			r.Run = func(ctx context.Context, tool string, args []string, options process.Options) (process.Result, error) {
				if tool == "git" && slices.Contains(args, "--ff-only") {
					if moved {
						if _, err := run(ctx, tool, args, options); err != nil {
							t.Fatalf("早送りの準備に失敗しました: %v", err)
						}
					}
					return process.Result{Started: true, Code: 128}, fmt.Errorf("merge failed")
				}
				return run(ctx, tool, args, options)
			}
			if err := r.runReaders(context.Background()); err != nil {
				t.Fatal(err)
			}
			results := sent()
			want := readerResult{Repo: "owner/app", Commit: next, Status: "failed", Reason: "preflight_failed"}
			if moved {
				want.Reason, want.Previous = "interrupted", previous
			}
			if len(results) != 1 || results[0] != want {
				t.Fatalf("結果: %+v", results)
			}
		})
	}
}
