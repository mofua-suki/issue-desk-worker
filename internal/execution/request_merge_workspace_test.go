package execution

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestManualMergeRetriesKeepDirtyTreesAndIgnoreOldLocalBranch(t *testing.T) {
	for _, detachOld := range []bool{false, true} {
		name := "旧枝が別worktreeで占有"
		if detachOld {
			name = "占有解除後も古いlocal枝が残る"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newPipeline(t, "approve")
			source := filepath.Join(f.root, "source")
			f.Branch = "chat/retry-direct"
			f.git(source, "checkout", "-b", f.Branch)
			write := func(dir, name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(source, "feature.txt", "accepted feature\n")
			f.git(source, "add", "feature.txt")
			f.git(source, "commit", "-m", "feature")
			f.git(source, "push", f.bare, f.Branch)
			originalHead := f.git(source, "rev-parse", "HEAD")
			f.head = originalHead
			f.pull = true
			old, err := f.Executor.requestWorkspace(ctx, monitorRequest{ID: "123-aabbccdd", Repo: "owner/project"}, "main", f.Branch)
			if err != nil {
				t.Fatal(err)
			}
			detached, err := f.Executor.requestMergeWorkspace(ctx, monitorRequest{ID: "124-aabbccdd", Repo: "owner/project"}, "main", f.Branch)
			if err != nil {
				t.Fatal(err)
			}
			if branch := f.git(detached.Tree, "branch", "--show-current"); branch != "" {
				t.Fatal("detachedでない", branch)
			}
			f.git(source, "checkout", "main")
			write(source, "advanced.txt", "new base\n")
			f.git(source, "add", "advanced.txt")
			f.git(source, "commit", "-m", "advance base")
			f.git(source, "push", f.bare, "main")
			base := f.git(source, "rev-parse", "HEAD")
			for _, w := range []struct {
				name   string
				follow func(context.Context) (bool, error)
			}{{"old", old.Follow}, {"detached", detached.Follow}} {
				changed, err := w.follow(ctx)
				if err != nil || !changed {
					t.Fatal(w.name, changed, err)
				}
			}
			if got, err := detached.Base(ctx); err != nil || got != base {
				t.Fatal("detached Base", got, err)
			}
			if snap, err := detached.Snapshot(ctx); err != nil || snap.Base != base || snap.Head == originalHead {
				t.Fatal("detached Follow/Snapshot", snap, err)
			}
			stale := f.git(old.Tree, "rev-parse", "HEAD")
			if stale == originalHead {
				t.Fatal("旧Follow済headを作れていません")
			}
			write(old.Tree, "failed-test-note.txt", "keep old dirty note\n")
			write(old.Tree, "feature.txt", "keep old dirty tracked content\n")
			write(detached.Tree, "detached-test-note.txt", "keep detached failed attempt\n")
			if detachOld {
				f.git(old.Tree, "checkout", "--detach")
			}
			f.git(source, "checkout", f.Branch)
			f.git(source, "merge", "--no-edit", "main")
			write(source, "current.txt", "approved current head\n")
			f.git(source, "add", "current.txt")
			f.git(source, "commit", "-m", "current PR head")
			f.git(source, "push", f.bare, f.Branch)
			f.head = f.git(source, "rev-parse", "HEAD")
			r := monitorRequest{ID: "125-aabbccdd", Kind: "merge_manual", Repo: "owner/project", Payload: encode(map[string]any{"number": 1, "head": f.head})}
			failedAttempt, err := f.Executor.requestMergeWorkspace(ctx, r, "main", f.Branch)
			if err != nil {
				t.Fatal(err)
			}
			write(failedAttempt.Tree, "same-request-note.txt", "keep same request dirty attempt\n")
			result, err := f.Executor.mergeRequestedPull(ctx, &requestProgress{Request: r, Stage: "claimed"})
			if err != nil || result["status"] != "merged" || !f.merged {
				t.Fatal(result, err)
			}
			if got := f.git(f.root, "--git-dir", old.Mirror, "rev-parse", "refs/heads/"+f.Branch); got != stale {
				t.Fatal("保存local枝を更新しました", got, stale)
			}
			if got := f.git(f.root, "--git-dir", f.bare, "rev-parse", f.Branch); got != f.head {
				t.Fatal("remote PR枝を変更しました", got, f.head)
			}
			if got := f.git(f.root, "--git-dir", f.bare, "rev-parse", "main"); got != f.head {
				t.Fatal("指定headのFFではありません", got, f.head)
			}
			for _, proof := range []struct{ dir, name, want string }{{old.Tree, "feature.txt", "keep old dirty tracked content\n"}, {old.Tree, "failed-test-note.txt", "keep old dirty note\n"}, {detached.Tree, "detached-test-note.txt", "keep detached failed attempt\n"}, {failedAttempt.Tree, "same-request-note.txt", "keep same request dirty attempt\n"}} {
				got, err := os.ReadFile(filepath.Join(proof.dir, proof.name))
				if err != nil || string(got) != proof.want {
					t.Fatal("旧作業を失いました", proof, err)
				}
			}
			attempts, err := filepath.Glob(filepath.Join(filepath.Dir(old.Tree), "request-"+r.ID+"-merge-*"))
			if err != nil || len(attempts) != 2 {
				t.Fatal("同IDの再試行が独立していません", attempts, err)
			}
		})
	}
}

func dirtyDiagnosticRun(t *testing.T, missing bool, create func(string), configure func(*pipelineFixture)) string {
	t.Helper()
	f, p, _ := delayedMergeFixture(t, false)
	if missing {
		staged := t.TempDir()
		create(staged)
		f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "cp -a "+strconv.Quote(staged+"/.")+" .")
	}
	if configure != nil {
		configure(f)
	}
	observed := false
	result, err := f.Executor.mergeRequestedPullObserved(context.Background(), p, func(test TestResult) {
		observed = true
		if missing != test.evidenceMissing {
			t.Fatalf("unexpected evidence state: %+v", test)
		}
		matches, err := filepath.Glob(filepath.Join(f.Executor.Config.RuntimeDir, "workspaces", "owner__project", "request-"+p.Request.ID+"-merge-*"))
		if err != nil || len(matches) != 1 {
			t.Fatalf("workspace: %v %v", matches, err)
		}
		if !missing {
			create(matches[0])
		}
	})
	if err != nil || !observed || result["status"] != "refused" || f.MergePosts != 0 {
		t.Fatalf("result=%v error=%v observed=%v merges=%d", result, err, observed, f.MergePosts)
	}
	return result["message"].(string)
}

func dirtyWrite(t *testing.T, tree, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(tree, name), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestManualMergeDirtyTreeDiagnostic(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			msg := dirtyDiagnosticRun(t, missing, func(tree string) { dirtyWrite(t, tree, "feature.txt"); dirtyWrite(t, tree, "new.txt") }, nil)
			for _, want := range []string{` M "feature.txt"`, `?? "new.txt"`} {
				if !strings.Contains(msg, want) {
					t.Fatalf("missing %q: %s", want, msg)
				}
			}
		})
	}
}

func TestManualMergeDirtyTreeDiagnosticLimits(t *testing.T) {
	for _, tc := range []struct{ count, width int }{{25, 10}, {15, 200}} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			msg := dirtyDiagnosticRun(t, false, func(tree string) {
				for i := 0; i < tc.count; i++ {
					dirtyWrite(t, tree, fmt.Sprintf("%03d%s", i, strings.Repeat("a", tc.width)))
				}
			}, nil)
			lines := strings.Split(msg, "\n")
			shown := 0
			for _, line := range lines {
				if strings.HasPrefix(line, "?? ") {
					shown++
				}
			}
			if shown > 20 || utf8.RuneCountInString(msg) > 2000 || !strings.Contains(msg, fmt.Sprintf("ほか%d件", tc.count-shown)) || shown == tc.count {
				t.Fatal(shown, msg)
			}
		})
	}
}

func TestManualMergeDirtyTreeDiagnosticRedaction(t *testing.T) {
	secret := "private-fixture-value"
	token := "idk" + "_" + strings.Repeat("a", 43)
	msg := dirtyDiagnosticRun(t, false, func(tree string) { dirtyWrite(t, tree, secret+"-"+token) }, func(f *pipelineFixture) { f.Executor.secret = secret })
	if strings.Contains(msg, secret) || strings.Contains(msg, token) || !strings.Contains(msg, `?? "[REDACTED]-[REDACTED]"`) {
		t.Fatal(msg)
	}
}

func TestManualMergeDirtyTreeDiagnosticPaths(t *testing.T) {
	msg := dirtyDiagnosticRun(t, false, func(tree string) {
		cmd := exec.Command("git", "mv", "feature.txt", "renamed name")
		cmd.Dir = tree
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("rename: %v %s", err, output)
		}
		dirtyWrite(t, tree, "space name")
		dirtyWrite(t, tree, "newline\nname")
		dirtyWrite(t, tree, "quote\"name")
		dirtyWrite(t, tree, ".gitignore")
		if err := os.WriteFile(filepath.Join(tree, ".gitignore"), []byte("ignored-output\n"), 0600); err != nil {
			t.Fatal(err)
		}
		dirtyWrite(t, tree, "ignored-output")
	}, nil)
	for _, want := range []string{`R  "feature.txt" -> "renamed name"`, `?? "space name"`, `?? "newline\nname"`, `?? "quote\"name"`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "ignored-output") {
		t.Fatal(msg)
	}
}

func TestManualMergeDirtyTreeDiagnosticUnavailable(t *testing.T) {
	msg := dirtyDiagnosticRun(t, false, func(tree string) { dirtyWrite(t, tree, "feature.txt") }, func(f *pipelineFixture) { fakeMergeGit(t, f, "--porcelain=v1", "echo private-stderr >&2; exit 1") })
	if !strings.Contains(msg, "取得できません") || strings.Contains(msg, "private-stderr") {
		t.Fatal(msg)
	}
}

func TestManualMergeDirtyTreeDisappearsBeforeDiagnostic(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			msg := dirtyDiagnosticRun(t, missing, func(tree string) {
				dirtyWrite(t, tree, "transient.txt")
			}, func(f *pipelineFixture) {
				fakeMergeGit(t, f, "--porcelain=v1", "test -f transient.txt && rm transient.txt || exit 1")
			})
			if msg != "固定テスト・整形の後に作業ツリーが変わりました" {
				t.Fatalf("unexpected refusal: %q", msg)
			}
		})
	}
}
