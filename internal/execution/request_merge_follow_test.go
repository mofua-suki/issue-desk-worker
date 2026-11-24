package execution

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
)

func delayedMergeFixture(t *testing.T, conflict bool) (*pipelineFixture, *requestProgress, string) {
	t.Helper()
	f := newPipeline(t, "approve")
	source := filepath.Join(f.root, "source")
	f.Branch = "chat/delayed"
	f.git(source, "checkout", "-b", f.Branch)
	if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "feature.txt")
	f.git(source, "commit", "-m", "feature")
	f.git(source, "push", f.bare, f.Branch)
	original := f.git(source, "rev-parse", "HEAD")
	f.head, f.pull = original, true
	f.git(source, "checkout", "main")
	name, content := "base.txt", "base\n"
	if conflict {
		name, content = "feature.txt", "conflicting\n"
	}
	if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", name)
	f.git(source, "commit", "-m", "advance")
	f.git(source, "push", f.bare, "main")
	base := f.git(source, "rev-parse", "HEAD")
	p := &requestProgress{Request: monitorRequest{ID: "124-aabbccdd", Kind: "merge_manual", Repo: "owner/project", Payload: encode(map[string]any{"number": 1, "head": original})}, Stage: "claimed"}
	return f, p, base
}

func TestDelayedManualMergePushesTestedCommit(t *testing.T) {
	f, p, base := delayedMergeFixture(t, false)
	original := f.head
	f.PullOverride = func(pull *gitea.Pull) {
		if pull.Head.SHA != original && f.PullReads < 5 {
			pull.Mergeable = false
		}
	}
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil || result["status"] != "merged" {
		t.Fatal(result, err)
	}
	head := f.git(f.root, "--git-dir", f.bare, "rev-parse", f.Branch)
	parents := strings.Fields(f.git(f.root, "--git-dir", f.bare, "rev-list", "--parents", "-n", "1", head))
	if len(parents) != 3 || parents[1] != original || parents[2] != base || p.Head != head || result["mergeCommit"] != head || f.MergePosts != 1 {
		t.Fatal(parents, p.Head, result, f.MergePosts)
	}
	saved, err := f.Executor.loadRequest(p.Request)
	if err != nil {
		t.Fatal(err)
	}
	result, err = f.Executor.mergeRequestedPull(context.Background(), saved)
	if err != nil || result["status"] != "merged" || f.MergePosts != 1 {
		t.Fatal(result, err)
	}
}

func TestDelayedManualMergeRefusesChangesRequestedAfterPush(t *testing.T) {
	f, p, _ := delayedMergeFixture(t, false)
	original := f.head
	f.PullOverride = func(pull *gitea.Pull) {
		if pull.Head.SHA != original {
			f.Reviews = []map[string]any{{"state": "REQUEST_CHANGES"}}
		}
	}
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil || result["status"] != "refused" || result["message"] != "変更要求のレビューが残っています" || f.MergePosts != 0 {
		t.Fatal(result, err, f.MergePosts)
	}
}

func TestDelayedManualMergeConflict(t *testing.T) {
	f, p, _ := delayedMergeFixture(t, true)
	original := f.head
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil || result["status"] != "refused" || !strings.Contains(result["message"].(string), "競合") || f.MergePosts != 0 || f.git(f.root, "--git-dir", f.bare, "rev-parse", f.Branch) != original {
		t.Fatal(result, err)
	}
}

func TestManualMergeResumeBeforeAndAfterPush(t *testing.T) {
	for _, pushed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "after"}[pushed], func(t *testing.T) {
			f, p, base := delayedMergeFixture(t, false)
			original := f.head
			source := filepath.Join(f.root, "source")
			f.git(source, "checkout", f.Branch)
			f.git(source, "merge", "--no-edit", "main")
			savedHead := f.git(source, "rev-parse", "HEAD")
			p.Head, p.Branch, p.Base = savedHead, f.Branch, "main"
			if pushed {
				f.git(source, "push", f.bare, f.Branch)
			}
			if err := f.Executor.saveRequest(p); err != nil {
				t.Fatal(err)
			}
			restored, err := f.Executor.loadRequest(p.Request)
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.Executor.mergeRequestedPull(context.Background(), restored)
			if err != nil || result["status"] != "merged" {
				t.Fatal(result, err)
			}
			got := result["mergeCommit"].(string)
			parents := strings.Fields(f.git(f.root, "--git-dir", f.bare, "rev-list", "--parents", "-n", "1", got))
			if len(parents) != 3 || parents[1] != original || parents[2] != base || (pushed && got != savedHead) {
				t.Fatal(parents, savedHead)
			}
		})
	}
}

func TestManualMergeRejectsUnexpectedSavedHead(t *testing.T) {
	f, p, _ := delayedMergeFixture(t, false)
	source := filepath.Join(f.root, "source")
	f.git(source, "checkout", f.Branch)
	f.git(source, "merge", "--no-edit", "main")
	if err := os.WriteFile(filepath.Join(source, "extra.txt"), []byte("extra\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "extra.txt")
	f.git(source, "commit", "--amend", "--no-edit")
	f.git(source, "push", f.bare, f.Branch)
	p.Head = f.git(source, "rev-parse", "HEAD")
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil || result["status"] != "refused" || f.MergePosts != 0 {
		t.Fatal(result, err)
	}
}

func TestManualMergeRejectsFastForwardFollow(t *testing.T) {
	f := newPipeline(t, "approve")
	source := filepath.Join(f.root, "source")
	f.Branch = "chat/behind"
	f.git(source, "checkout", "-b", f.Branch)
	f.git(source, "push", f.bare, f.Branch)
	original := f.git(source, "rev-parse", "HEAD")
	f.head, f.pull = original, true
	f.git(source, "checkout", "main")
	if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "feature.txt")
	f.git(source, "commit", "-m", "base")
	f.git(source, "push", f.bare, "main")
	p := &requestProgress{Request: monitorRequest{ID: "124-aabbccdd", Kind: "merge_manual", Repo: "owner/project", Payload: encode(map[string]any{"number": 1, "head": original})}, Stage: "claimed"}
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil || result["status"] != "refused" || f.MergePosts != 0 || f.git(f.root, "--git-dir", f.bare, "rev-parse", f.Branch) != original {
		t.Fatal(result, err)
	}
}

func TestMergeSnapshotGuardsBeforeAndAfterFollow(t *testing.T) {
	for _, kind := range []string{"permission", "sql", "secret"} {
		t.Run(kind, func(t *testing.T) {
			f, p, _ := delayedMergeFixture(t, false)
			source := filepath.Join(f.root, "source")
			f.git(source, "checkout", f.Branch)
			name, content := "change.txt", "normal\n"
			switch kind {
			case "permission":
				name, content = "protected.json", "{}\n"
				f.Executor.Config.ReleasePolicy.PermissionFiles = []string{name}
			case "sql":
				name, content = "change.sql", "DROP TABLE production_data;\n"
			case "secret":
				content = "private-value-for-guard\n"
				f.Executor.secret = "private-value-for-guard"
			}
			if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(source, "add", name)
			f.git(source, "commit", "-m", "guarded change")
			f.git(source, "push", f.bare, f.Branch)
			w, err := f.Executor.requestMergeWorkspace(context.Background(), p.Request, "main", f.Branch)
			if err != nil {
				t.Fatal(err)
			}
			before, err := f.Executor.checkMergeSnapshot(context.Background(), w, "owner/project", &mergeGate{})
			if err != nil || before["status"] != "refused" {
				t.Fatal(before, err)
			}
			if changed, err := w.Follow(context.Background()); err != nil || !changed {
				t.Fatal(changed, err)
			}
			after, err := f.Executor.checkMergeSnapshot(context.Background(), w, "owner/project", &mergeGate{})
			if err != nil || after["status"] != "refused" || before["message"] != after["message"] {
				t.Fatal(before, after, err)
			}
		})
	}
}

func TestManualMergeResumesAfterPushAndBaseAdvance(t *testing.T) {
	f, p, _ := delayedMergeFixture(t, false)
	source := filepath.Join(f.root, "source")
	f.git(source, "checkout", f.Branch)
	f.git(source, "merge", "--no-edit", "main")
	first := f.git(source, "rev-parse", "HEAD")
	f.git(source, "push", f.bare, f.Branch)
	p.Head, p.Branch, p.Base = first, f.Branch, "main"
	f.git(source, "checkout", "main")
	if err := os.WriteFile(filepath.Join(source, "next.txt"), []byte("next\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "next.txt")
	f.git(source, "commit", "-m", "advance again")
	f.git(source, "push", f.bare, "main")
	base := f.git(source, "rev-parse", "HEAD")
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil || result["status"] != "merged" {
		t.Fatal(result, err)
	}
	parents := strings.Fields(f.git(f.root, "--git-dir", f.bare, "rev-list", "--parents", "-n", "1", result["mergeCommit"].(string)))
	if len(parents) != 3 || parents[1] != first || parents[2] != base {
		t.Fatal(parents)
	}
}

func TestManualMergeBaseOnlyProtectedChange(t *testing.T) {
	f, p, _ := delayedMergeFixture(t, false)
	source := filepath.Join(f.root, "source")
	f.Executor.Config.ReleasePolicy.PermissionFiles = []string{"protected.json"}
	if err := os.WriteFile(filepath.Join(source, "protected.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "protected.json")
	f.git(source, "commit", "-m", "base protection")
	f.git(source, "push", f.bare, "main")
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil || result["status"] != "merged" {
		t.Fatal(result, err)
	}
}

func TestPushDiagnosticRedactsCredentials(t *testing.T) {
	e := &Executor{secret: "executor-secret"}
	got := pushDiagnostic(e, "fatal: https://user:password@example.test/repo: executor-secret rejected")
	if strings.Contains(got, "password") || strings.Contains(got, "executor-secret") || !strings.Contains(got, "rejected") {
		t.Fatal(got)
	}
}

func TestMergeChainLimitAndInjectedTree(t *testing.T) {
	for _, inject := range []bool{false, true} {
		t.Run(map[bool]string{false: "limit", true: "injected"}[inject], func(t *testing.T) {
			f, p, _ := delayedMergeFixture(t, false)
			source := filepath.Join(f.root, "source")
			f.git(source, "checkout", f.Branch)
			count := 9
			if inject {
				count = 1
			}
			for i := 0; i < count; i++ {
				f.git(source, "checkout", "main")
				if err := os.WriteFile(filepath.Join(source, "advance.txt"), []byte(strings.Repeat("x", i+1)), 0600); err != nil {
					t.Fatal(err)
				}
				f.git(source, "add", "advance.txt")
				f.git(source, "commit", "-m", "advance")
				f.git(source, "checkout", f.Branch)
				f.git(source, "merge", "--no-edit", "main")
			}
			if inject {
				if err := os.WriteFile(filepath.Join(source, "injected.txt"), []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
				f.git(source, "add", "injected.txt")
				f.git(source, "commit", "--amend", "--no-edit")
			}
			f.git(source, "push", f.bare, "main")
			f.git(source, "push", f.bare, f.Branch)
			p.Head = f.git(source, "rev-parse", "HEAD")
			result, err := f.Executor.mergeRequestedPull(context.Background(), p)
			if err != nil || result["status"] != "refused" || f.MergePosts != 0 {
				t.Fatal(result, err)
			}
		})
	}
}

func TestManualMergeRejectsPROwnProtectedChange(t *testing.T) {
	f, p, _ := delayedMergeFixture(t, false)
	source := filepath.Join(f.root, "source")
	f.Executor.Config.ReleasePolicy.PermissionFiles = []string{"protected.json"}
	f.git(source, "checkout", f.Branch)
	if err := os.WriteFile(filepath.Join(source, "protected.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "protected.json")
	f.git(source, "commit", "-m", "protected change")
	f.git(source, "push", f.bare, f.Branch)
	head := f.git(source, "rev-parse", "HEAD")
	p.Request.Payload = encode(map[string]any{"number": 1, "head": head})
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil || result["status"] != "refused" || f.MergePosts != 0 {
		t.Fatal(result, err)
	}
}

// A base tip that is a merge commit must not pass as a follow: the check cannot rest
// on walking first parents.
func TestManualMergeRefusesFastForwardWhenBaseTipIsMergeCommit(t *testing.T) {
	f := newPipeline(t, "approve")
	source := filepath.Join(f.root, "source")
	f.Branch = "chat/behind-merge"
	f.git(source, "checkout", "-b", f.Branch)
	f.git(source, "push", f.bare, f.Branch)
	original := f.git(source, "rev-parse", "HEAD")
	f.head, f.pull = original, true
	f.git(source, "checkout", "-b", "side")
	if err := os.WriteFile(filepath.Join(source, "side.txt"), []byte("side\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "side.txt")
	f.git(source, "commit", "-m", "side")
	f.git(source, "checkout", "main")
	if err := os.WriteFile(filepath.Join(source, "main.txt"), []byte("main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "main.txt")
	f.git(source, "commit", "-m", "main")
	f.git(source, "merge", "--no-ff", "--no-edit", "side")
	f.git(source, "push", f.bare, "main")
	p := &requestProgress{Request: monitorRequest{ID: "124-aabbccdd", Kind: "merge_manual", Repo: "owner/project", Payload: encode(map[string]any{"number": 1, "head": original})}}
	result, err := f.Executor.mergeRequestedPull(context.Background(), p)
	if err != nil || result["status"] != "refused" || !strings.Contains(fmt.Sprint(result["message"]), "早送り") {
		t.Fatal(result, err)
	}
	if f.MergePosts != 0 || f.git(f.root, "--git-dir", f.bare, "rev-parse", f.Branch) != original {
		t.Fatal("早送りの先頭を送りました", f.MergePosts)
	}
}

// When merge-tree cannot run (Git older than 2.38), the comparison must refuse with a
// reason rather than be skipped or reported as an injected change.
func TestMergeChainRefusesWhenMergeTreeCannotRun(t *testing.T) {
	f, p, base := delayedMergeFixture(t, false)
	source := filepath.Join(f.root, "source")
	f.git(source, "checkout", f.Branch)
	f.git(source, "merge", "--no-edit", base)
	candidate := f.git(source, "rev-parse", "HEAD")
	original := f.head
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "git")
	body := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = merge-tree ]; then echo 'unknown option write-tree' >&2; exit 129; fi; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	w := &gitwork.Workspace{Config: gitwork.Config{Git: script}, Tree: source}
	result, err := validateMergeChain(context.Background(), w, original, candidate, base)
	if err != nil || result == nil || result["status"] != "refused" {
		t.Fatal(result, err, p.Request.ID)
	}
	message := fmt.Sprint(result["message"])
	if !strings.Contains(message, "merge-tree") || !strings.Contains(message, "unknown option") || strings.Contains(message, "別の変更") {
		t.Fatal(message)
	}
}

// An interrupted merge-tree says nothing about the PR. It must come back as an error so
// the request is resumed, because a refusal would be saved as the request's final result.
func TestMergeChainReturnsErrorWhenMergeTreeIsInterrupted(t *testing.T) {
	f, _, base := delayedMergeFixture(t, false)
	source := filepath.Join(f.root, "source")
	f.git(source, "checkout", f.Branch)
	f.git(source, "merge", "--no-edit", base)
	candidate := f.git(source, "rev-parse", "HEAD")
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	script := filepath.Join(dir, "git")
	body := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = merge-tree ]; then : > " + started + " || exit 1; exec sleep 60; fi; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	w := &gitwork.Workspace{Config: gitwork.Config{Git: script}, Tree: source}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	result, err := validateMergeChain(ctx, w, f.head, candidate, base)
	if !errors.Is(err, context.Canceled) || result != nil {
		t.Fatal(result, err)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatal("merge-tree の実行中に中断していません", err)
	}
}

func TestMergedTreeSeparatesInterruptionFromRunFailure(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	w := &gitwork.Workspace{Config: gitwork.Config{Git: "git"}, Tree: t.TempDir()}
	if tree, refused, err := mergedTree(cancelled, w, "a", "b"); !errors.Is(err, context.Canceled) || refused != nil || tree != "" {
		t.Fatal(tree, refused, err)
	}
	expired, stop := context.WithTimeout(context.Background(), 0)
	defer stop()
	if _, refused, err := mergedTree(expired, w, "a", "b"); !errors.Is(err, context.DeadlineExceeded) || refused != nil {
		t.Fatal(refused, err)
	}
	// Git を起動できないときは中断ではないので、理由付きの拒否のまま。
	w = &gitwork.Workspace{Config: gitwork.Config{Git: filepath.Join(t.TempDir(), "missing-git")}, Tree: t.TempDir()}
	tree, refused, err := mergedTree(context.Background(), w, "a", "b")
	if err != nil || tree != "" || refused == nil || refused["status"] != "refused" || !strings.Contains(fmt.Sprint(refused["message"]), "merge-tree") {
		t.Fatal(tree, refused, err)
	}
}

// A merge-tree killed from outside (exit code -1) did not fail for the Git version.
func TestMergedTreeReportsKilledCommand(t *testing.T) {
	script := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nkill -9 $$\n"), 0700); err != nil {
		t.Fatal(err)
	}
	w := &gitwork.Workspace{Config: gitwork.Config{Git: script}, Tree: t.TempDir()}
	_, refused, err := mergedTree(context.Background(), w, "a", "b")
	if err != nil || refused == nil {
		t.Fatal(refused, err)
	}
	message := fmt.Sprint(refused["message"])
	if !strings.Contains(message, "外から停止") || strings.Contains(message, "2.38") {
		t.Fatal(message)
	}
}

// fakeMergeGit wraps Git and runs body in place of it when an argument equals trigger.
func fakeMergeGit(t *testing.T, f *pipelineFixture, trigger, body string) {
	t.Helper()
	real := f.Executor.Config.Git
	script := filepath.Join(t.TempDir(), "git")
	text := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = " + trigger + " ]; then " + body + "; fi; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(text), 0700); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Git = script
	t.Cleanup(func() { f.Executor.Config.Git = real })
}

// cancelWhenStarted cancels the returned context once the started file appears.
func cancelWhenStarted(t *testing.T, started string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return ctx
}

// An interruption during setup, the tree check or the push says nothing about the PR.
// It must return an error, and the same request must then finish on resume.
func TestManualMergeInterruptionIsResumedNotSavedAsResult(t *testing.T) {
	for _, stage := range []string{"setup", "clean", "push"} {
		t.Run(stage, func(t *testing.T) {
			f, p, _ := delayedMergeFixture(t, false)
			real := f.Executor.Config.Git
			started := filepath.Join(t.TempDir(), "started")
			wait := ": > " + started + " || exit 1; exec sleep 60"
			switch stage {
			case "setup":
				f.Executor.Config.Repositories[0].SetupCommand = []string{"/bin/sh", "-c", wait}
			case "clean":
				fakeMergeGit(t, f, "status", wait)
			case "push":
				fakeMergeGit(t, f, "push", wait)
			}
			result, err := f.Executor.mergeRequestedPull(cancelWhenStarted(t, started), p)
			if !errors.Is(err, context.Canceled) || result != nil || f.MergePosts != 0 {
				t.Fatal(result, err, f.MergePosts)
			}
			if _, err := os.Stat(started); err != nil {
				t.Fatal("対象の段の実行中に中断していません", err)
			}
			f.Executor.Config.Git = real
			f.Executor.Config.Repositories[0].SetupCommand = nil
			result, err = f.Executor.mergeRequestedPull(context.Background(), p)
			if err != nil || result["status"] != "merged" {
				t.Fatal(result, err)
			}
		})
	}
}

// The same stages failing with the context alive keep their refusal or failure.
func TestManualMergeRealFailuresStayFinal(t *testing.T) {
	for _, tc := range []struct{ stage, status, message string }{
		{"setup", "refused", "固定テストの環境を準備できません"},
		{"clean", "refused", "作業ツリーが変わりました"},
		{"push", "failed", "pushに失敗しました"},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			f, p, _ := delayedMergeFixture(t, false)
			switch tc.stage {
			case "setup":
				f.Executor.Config.Repositories[0].SetupCommand = []string{"/bin/sh", "-c", "exit 3"}
			case "clean":
				fakeMergeGit(t, f, "status", "echo ' M changed.txt'; exit 0")
			case "push":
				fakeMergeGit(t, f, "push", "echo denied >&2; exit 1")
			}
			result, err := f.Executor.mergeRequestedPull(context.Background(), p)
			if err != nil || result["status"] != tc.status || !strings.Contains(fmt.Sprint(result["message"]), tc.message) || f.MergePosts != 0 {
				t.Fatal(result, err, f.MergePosts)
			}
		})
	}
}

func TestDirectMergeBrowserTools(t *testing.T) {
	for _, tc := range []struct {
		name, repo         string
		probeOK, wantTools bool
	}{
		{"intake", "mofua-suki/issue-desk-intake", true, true},
		{"other", "owner/project", true, false},
		{"probe-failure", "mofua-suki/issue-desk-intake", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p, _ := delayedMergeFixture(t, false)
			if tc.repo != "owner/project" {
				p.Request.Repo = tc.repo
				f.Executor.Config.Repositories[0].Name = tc.repo
				f.GiteaOverride = func(_ http.ResponseWriter, r *http.Request) bool {
					r.URL.Path = strings.Replace(r.URL.Path, "/api/v1/repos/mofua-suki/issue-desk-intake", "/api/v1/repos/owner/project", 1)
					return false
				}
				f.PullOverride = func(pull *gitea.Pull) { pull.Head.Repo.FullName = tc.repo; pull.Base.Repo.FullName = tc.repo }
				repoDir := filepath.Join(f.root, "repositories", "mofua-suki")
				if err := os.MkdirAll(repoDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.bare, filepath.Join(repoDir, "issue-desk-intake.git")); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			caddy, chromium, codex := filepath.Join(dir, "caddy"), filepath.Join(dir, "chromium"), filepath.Join(dir, "codex")
			for _, path := range []string{caddy, chromium} {
				if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			probeCode := "1"
			if tc.probeOK {
				probeCode = "0"
			}
			script := "#!/bin/sh\ncase $* in *--dump-dom*) exit " + probeCode + ";; esac\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -- ]; then shift; exec \"$@\"; fi; shift; done\nexit 8\n"
			if err := os.WriteFile(codex, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			f.Executor.Config.Codex = codex
			f.Executor.browserTools = func() (string, string) { return caddy, chromium }
			wantCaddy, wantChromium := "unset", "unset"
			if tc.wantTools {
				wantCaddy, wantChromium = caddy, chromium
			}
			check := fmt.Sprintf(`test "${ISSUE_DESK_TEST_CADDY-unset}" = %q && test "${ISSUE_DESK_TEST_CHROMIUM-unset}" = %q`, wantCaddy, wantChromium)
			f.Executor.Config.Repositories[0].TestCommand = []string{f.Executor.Config.Node, "fixture-command.test.js", check}
			var observed TestResult
			result, err := f.Executor.mergeRequestedPullObserved(context.Background(), p, func(test TestResult) { observed = test })
			if err != nil || result["status"] != "merged" || !observed.Passed {
				t.Fatalf("result=%v test=%+v err=%v", result, observed, err)
			}
			if !tc.probeOK && !strings.Contains(observed.Detail, chromiumProbeWarning) {
				t.Fatalf("missing probe warning: %+v", observed)
			}
		})
	}
}

func TestManualMergeReviewPagination(t *testing.T) {
	for _, mode := range []string{"requested", "dismissed", "failure", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			f, p, _ := delayedMergeFixture(t, false)
			f.GiteaOverride = pagedReviewResponse(mode)
			tests := 0
			result, err := f.Executor.mergeRequestedPullObserved(context.Background(), p, func(TestResult) { tests++ })
			switch mode {
			case "dismissed":
				if err != nil || result["status"] != "merged" || f.MergePosts != 1 {
					t.Fatalf("result=%v err=%v merges=%d", result, err, f.MergePosts)
				}
			case "requested":
				if err != nil || result["status"] != "refused" || len(f.TurnCalls) != 0 || tests != 0 || f.MergePosts != 0 {
					t.Fatalf("result=%v err=%v turns=%v tests=%d merges=%d", result, err, f.TurnCalls, tests, f.MergePosts)
				}
			default:
				if err == nil || f.MergePosts != 0 {
					t.Fatalf("result=%v err=%v merges=%d", result, err, f.MergePosts)
				}
			}
		})
	}
}
