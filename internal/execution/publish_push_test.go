package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

func TestPublishPushClassificationAndSecrecy(t *testing.T) {
	cases := []struct{ output, reason string }{
		{"[rejected] (non-fast-forward)", gitwork.PushBranchReason},
		{"[rejected] (fetch first)", gitwork.PushBranchReason},
		{"fatal: Authentication failed", gitwork.PushAuthReason},
		{"remote: pre-receive hook declined", gitwork.PushHookReason},
		{"fatal: Could not resolve host", gitwork.PushConnectionReason},
		{"[rejected] denied", gitwork.PushOtherReason},
		{"unexpected", gitwork.PushOtherReason},
	}
	for _, tc := range cases {
		t.Run(tc.output, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "git")
			content := "#!/bin/sh\nprintf '%s\\n' '" + tc.output + " https://secret.invalid/path token-secret'\nprintf '%s\\n' 'stderr-secret' >&2\nexit 1\n"
			if err := os.WriteFile(script, []byte(content), 0700); err != nil {
				t.Fatal(err)
			}
			w := &gitwork.Workspace{Config: gitwork.Config{Git: script}, Tree: t.TempDir(), Branch: "codex/test"}
			err := (&Executor{}).publish(context.Background(), Descriptor{}, nil, w, "")
			var push *gitwork.PushError
			if !errors.As(err, &push) || err.Error() != tc.reason {
				t.Fatalf("push error: %v", err)
			}
			execution := &ExecutionError{Err: err, Stage: "publish"}
			if execution.SafeExecutionReason() != tc.reason || !errors.As(execution, &push) || !errors.Is(execution, err) {
				t.Fatal("reason or chain")
			}
			wrapped := &ExecutionError{Err: fmt.Errorf("%w: token-secret", err)}
			if wrapped.SafeExecutionReason() != "" {
				t.Fatal("accepted a suffix")
			}
		})
	}
}

func TestPublishPushCancellation(t *testing.T) {
	script := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'non-fast-forward token-secret' >&2\nsleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	w := &gitwork.Workspace{Config: gitwork.Config{Git: script}, Tree: t.TempDir(), Branch: "codex/test"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&Executor{}).publish(ctx, Descriptor{}, nil, w, "")
	if !errors.Is(err, context.Canceled) || (&ExecutionError{Err: err}).SafeExecutionReason() != "" {
		t.Fatalf("cancellation: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = (&Executor{}).publish(ctx, Descriptor{}, nil, w, "")
	if !errors.Is(err, context.DeadlineExceeded) || (&ExecutionError{Err: err}).SafeExecutionReason() != "" {
		t.Fatalf("deadline: %v", err)
	}
}

func TestPublishPushRuntimeResume(t *testing.T) {
	f := newPipeline(t, "approve")
	real := f.Executor.Config.Git
	script := filepath.Join(f.root, "push-wrapper")
	marker := filepath.Join(f.root, "push-failed")
	text := "#!/bin/sh\ncase \" $* \" in *' push '*) if [ -f '" + marker + "' ]; then touch '" + marker + "'; echo 'failed to connect https://secret.invalid/token-secret' >&2; exit 1; fi; touch '" + marker + "';; esac\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(script, []byte(text), 0700); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Git = script
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	a := runErrorResume(t, f)
	saved, err := f.Executor.Store.Load(f.Job.ID)
	if err != nil || saved == nil || saved.Stage != "publish" || len(saved.Result) == 0 || a.errorDetail["reason"] != gitwork.PushConnectionReason {
		t.Fatalf("saved=%+v detail=%v err=%v", saved, a.errorDetail, err)
	}
	detail, _ := json.Marshal(a.errorDetail)
	if len(a.errorDetail) != 4 || a.errorDetail["kind"] != "execution_error" || a.errorDetail["stage"] != "publish" || a.errorDetail["at"] == "" || strings.Contains(string(detail), "secret") {
		t.Fatalf("unsafe detail: %s", detail)
	}
	if !strings.Contains(logs.String(), gitwork.PushConnectionReason) || strings.Contains(logs.String(), "token-secret") || strings.Contains(logs.String(), "secret.invalid") {
		t.Fatalf("unsafe logs %s", logs.String())
	}
	result := append([]byte(nil), saved.Result...)
	calls := len(f.Calls)
	f.Executor.Config.Git = real
	a = runErrorResume(t, f)
	saved, err = f.Executor.Store.Load(f.Job.ID)
	if err != nil || saved.Stage != "done" || !bytes.Equal(result, saved.Result) || len(f.Calls) != calls || a.errorReports != 0 {
		t.Fatalf("resume saved=%+v calls=%d/%d err=%v", saved, len(f.Calls), calls, err)
	}
}

func TestResumeTargetBranchBeforeDirtyCommit(t *testing.T) {
	for _, recreate := range []bool{false} {
		t.Run(fmt.Sprint("recreate=", recreate), func(t *testing.T) {
			f := newPipeline(t, "approve")
			ctx := context.Background()
			c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
			w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
			if err != nil {
				t.Fatal(err)
			}
			head := f.git(w.Tree, "rev-parse", "HEAD")
			base, err := w.Base(ctx)
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(f.root, "source")
			f.git(source, "checkout", "-b", f.Branch)
			if err := os.WriteFile(filepath.Join(source, "remote.txt"), []byte("remote"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(source, "add", "remote.txt")
			f.git(source, "commit", "-m", "remote change")
			f.git(source, "push", f.bare, f.Branch)
			remote := f.git(source, "rev-parse", "HEAD")
			if recreate {
				f.git(f.root, "--git-dir", w.Mirror, "worktree", "remove", w.Tree)
			} else if err := os.WriteFile(filepath.Join(w.Tree, "dirty.txt"), []byte("unsaved"), 0600); err != nil {
				t.Fatal(err)
			}
			record := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "review", Branch: f.Branch, Provider: "codex", Head: head, Base: base}
			if err := f.Executor.Store.Save(record); err != nil {
				t.Fatal(err)
			}
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "held" {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Head != head || f.git(w.Tree, "rev-parse", "HEAD") != head || remote == head || len(f.Calls) != 0 || !strings.Contains(string(saved.FinalState), errTargetBranchDirty.Error()) {
				t.Fatalf("saved=%+v state=%+v", saved, state)
			}
			if !recreate {
				if b, err := os.ReadFile(filepath.Join(w.Tree, "dirty.txt")); err != nil || string(b) != "unsaved" {
					t.Fatal("dirty content changed")
				}
				// Reproduce the former post-open dirty-commit path after verifying the hold.
				if err := w.Stage(ctx); err != nil {
					t.Fatal(err)
				}
				generated, err := w.Commit(ctx, "resume")
				if err != nil || generated == head {
					t.Fatalf("generated=%s err=%v", generated, err)
				}
				out, err := w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", remote, generated)
				if err != nil || out.Code != 1 {
					t.Fatalf("generated relation=%+v err=%v", out, err)
				}
			}
		})
	}
}

func TestTargetBranchRelations(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = checkTargetBranch(ctx, w, false); err != nil {
		t.Fatal(err)
	}
	if err = checkTargetBranch(ctx, w, true); err == nil {
		t.Fatal("missing branch accepted")
	}
	source := filepath.Join(f.root, "source")
	f.git(source, "checkout", "-b", f.Branch)
	f.git(source, "push", f.bare, f.Branch)
	if err = w.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if err = checkTargetBranch(ctx, w, true); err != nil {
		t.Fatal(err)
	}
	// The remote can advance while the worktree has no local commits (behind).
	if err = os.WriteFile(filepath.Join(source, "behind"), []byte("remote"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "behind")
	f.git(source, "commit", "-m", "advance remote")
	f.git(source, "push", f.bare, f.Branch)
	if err = w.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if err = checkTargetBranch(ctx, w, true); !errors.Is(err, errTargetBranchDiverged) {
		t.Fatalf("behind: %v", err)
	}
	// Bring the worktree to the remote, then verify an unpublished descendant.
	f.git(w.Tree, "merge", "--ff-only", "refs/remotes/origin/"+f.Branch)
	if err = os.WriteFile(filepath.Join(w.Tree, "local"), []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(w.Tree, "add", "local")
	f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "local")
	if err = checkTargetBranch(ctx, w, true); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "remote"), []byte("remote"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "remote")
	f.git(source, "commit", "-m", "remote")
	f.git(source, "push", f.bare, f.Branch)
	if err = w.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if err = checkTargetBranch(ctx, w, true); !errors.Is(err, errTargetBranchDiverged) {
		t.Fatalf("diverged: %v", err)
	}
}

func TestPushRaceHoldsAndChecksOwnership(t *testing.T) {
	for _, mode := range []string{"publish", "held", "lost"} {
		t.Run(mode, func(t *testing.T) {
			f := newPipeline(t, "approve")
			ctx := context.Background()
			c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
			w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(f.root, "source")
			f.git(source, "checkout", "-b", f.Branch)
			f.git(source, "push", f.bare, f.Branch)
			if err = w.Fetch(ctx); err != nil {
				t.Fatal(err)
			}
			if err = checkTargetBranch(ctx, w, true); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(w.Tree, "local"), []byte("local"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(w.Tree, "add", "local")
			f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "local")
			if err = os.WriteFile(filepath.Join(source, "remote"), []byte("remote"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(source, "add", "remote")
			f.git(source, "commit", "-m", "remote")
			f.git(source, "push", f.bare, f.Branch)
			r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "publish", Branch: f.Branch, Head: f.git(w.Tree, "rev-parse", "HEAD")}
			r.Result = json.RawMessage(`{"acceptanceTransfers":null}`)
			fence := func(context.Context) error {
				if mode == "lost" {
					return errors.New("lease lost")
				}
				return nil
			}
			var state intake.State
			if mode == "publish" {
				err = f.Executor.publish(ctx, Descriptor{}, r, w, "")
				if !errors.Is(err, errTargetBranchDiverged) {
					t.Fatalf("publish: %v", err)
				}
				state, err = f.Executor.holdTargetBranch(ctx, r, fence, err)
			} else if mode == "lost" {
				err = f.Executor.publish(ctx, Descriptor{}, r, w, "")
				if !errors.Is(err, errTargetBranchDiverged) {
					t.Fatal(err)
				}
				state, err = f.Executor.holdTargetBranch(ctx, r, fence, err)
			} else {
				state, err = f.Executor.publishHeld(ctx, Descriptor{}, r, w, fence, "review_held", "other")
			}
			if mode == "lost" {
				if err == nil || len(r.FinalState) != 0 {
					t.Fatalf("lost: %+v %v", state, err)
				}
				return
			}
			if err != nil || state.Status != "held" || !strings.Contains(string(r.FinalState), errTargetBranchDiverged.Error()) {
				t.Fatalf("state=%+v err=%v record=%s", state, err, r.FinalState)
			}
		})
	}
}

func TestTargetBranchComparisonFailureIsNotDivergence(t *testing.T) {
	for _, code := range []int{2, 128} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "git")
			content := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *' show-ref '*) exit 0;; *' merge-base '*) exit %d;; esac\nexit 1\n", code)
			if err := os.WriteFile(script, []byte(content), 0700); err != nil {
				t.Fatal(err)
			}
			w := &gitwork.Workspace{Config: gitwork.Config{Git: script}, Tree: t.TempDir(), Branch: "codex/test"}
			err := checkTargetBranch(context.Background(), w, true)
			if err == nil || errors.Is(err, errTargetBranchDiverged) {
				t.Fatalf("comparison error: %v", err)
			}
		})
	}
}

func TestScopeAndRequestedPatchPushFailureReasons(t *testing.T) {
	for _, mode := range []string{"scope", "patch"} {
		t.Run(mode, func(t *testing.T) {
			f := newPipeline(t, "scope-granted")
			real := f.Executor.Config.Git
			script := filepath.Join(f.root, "fail-push")
			content := "#!/bin/sh\ncase \" $* \" in *' push '*) echo 'permission denied token-secret' >&2; exit 1;; esac\nexec '" + real + "' \"$@\"\n"
			if err := os.WriteFile(script, []byte(content), 0700); err != nil {
				t.Fatal(err)
			}
			f.Executor.Config.Git = script
			var err error
			if mode == "scope" {
				_, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			} else {
				base := f.git(f.root, "--git-dir", f.bare, "rev-parse", "main")
				patch := "diff --git a/feature.txt b/feature.txt\nnew file mode 100644\nindex 0000000..0000000\n--- /dev/null\n+++ b/feature.txt\n@@ -0,0 +1 @@\n+accepted feature\n"
				r := monitorRequest{ID: "123-aabbccdd", Kind: "pr_create", Repo: "owner/project"}
				p := prPayload{Title: "feature", Branch: "chat/123-aabbccdd", Patch: patch, Base: base, PatchHash: "test"}
				err = f.Executor.prepareRequestedPatch(context.Background(), r, p, "main")
			}
			var push *gitwork.PushError
			if !errors.As(err, &push) || err.Error() != gitwork.PushAuthReason {
				t.Fatalf("push error = %v", err)
			}
			if strings.Contains(err.Error(), "token-secret") {
				t.Fatal("push error exposed token-secret")
			}
			if got := (&ExecutionError{Err: push}).SafeExecutionReason(); got != gitwork.PushAuthReason {
				t.Fatalf("reason = %q", got)
			}
		})
	}
}

func TestExecutePublishRacePersistsBranchHold(t *testing.T) {
	f := newPipeline(t, "approve")
	real := f.Executor.Config.Git
	script := filepath.Join(f.root, "fail-first-push")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncase \" $* \" in *' push '*) echo 'failed to connect' >&2; exit 1;; esac\nexec '"+real+"' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Git = script
	runErrorResume(t, f)
	r, err := f.Executor.Store.Load(f.Job.ID)
	if err != nil || r.Stage != "publish" {
		t.Fatalf("record=%+v err=%v", r, err)
	}
	source := filepath.Join(f.root, "source")
	f.git(source, "checkout", "-b", f.Branch)
	if err := os.WriteFile(filepath.Join(source, "remote"), []byte("advance"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "remote")
	f.git(source, "commit", "-m", "advance")
	// Advance after Open has fetched, at the first push in Execute.
	race := filepath.Join(f.root, "race-push")
	marker := filepath.Join(f.root, "raced")
	scriptText := "#!/bin/sh\ncase \" $* \" in *' push '*) if [ ! -f '" + marker + "' ]; then touch '" + marker + "'; '" + real + "' -C '" + source + "' push '" + f.bare + "' '" + f.Branch + "' || exit 1; fi;; esac\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(race, []byte(scriptText), 0700); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Git = race
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || state.HoldReason != "review_held" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	saved, err := f.Executor.Store.Load(f.Job.ID)
	var final intake.State
	if err != nil || json.Unmarshal(saved.FinalState, &final) != nil {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	var result, view map[string]any
	_ = json.Unmarshal(final.Result, &result)
	_ = json.Unmarshal(final.View, &view)
	if result["summary"] != errTargetBranchDiverged.Error() || view["targetBranchRelation"] != "diverged" {
		t.Fatalf("final=%+v", final)
	}
}

func TestFastForwardCleanBehindAndHoldRelations(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(f.root, "source")
	f.git(source, "checkout", "-b", f.Branch)
	if err := os.WriteFile(filepath.Join(source, "remote"), []byte("remote"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "remote")
	f.git(source, "commit", "-m", "remote")
	f.git(source, "push", f.bare, f.Branch)
	if err := w.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	err = checkTargetBranch(ctx, w, true)
	r := &journal.Record{ID: 1, Result: json.RawMessage(`{"acceptanceTransfers":null}`)}
	state, err := f.Executor.holdTargetBranch(ctx, r, func(context.Context) error { return nil }, err)
	var view map[string]any
	_ = json.Unmarshal(state.View, &view)
	if err != nil || view["targetBranchRelation"] != "behind" || !strings.Contains(string(r.FinalState), errTargetBranchDiverged.Error()) {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if err := w.FastForwardTarget(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := f.git(w.Tree, "rev-parse", "HEAD"), f.git(source, "rev-parse", "HEAD"); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestExecuteFastForwardsRecreatedCleanWorktree(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	head := f.git(w.Tree, "rev-parse", "HEAD")
	base, err := w.Base(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(f.root, "source")
	f.git(source, "checkout", "-b", f.Branch)
	if err := os.WriteFile(filepath.Join(source, "remote"), []byte("remote"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "remote")
	f.git(source, "commit", "-m", "remote")
	f.git(source, "push", f.bare, f.Branch)
	remote := f.git(source, "rev-parse", "HEAD")
	f.git(f.root, "--git-dir", w.Mirror, "worktree", "remove", w.Tree)
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "publish", Branch: f.Branch, Provider: "codex", Head: head, Base: base, Result: json.RawMessage(`{"acceptanceTransfers":null}`), Test: json.RawMessage(`{"passed":true}`), Review: json.RawMessage(`{"approved":true}`)}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	real := f.Executor.Config.Git
	script := filepath.Join(f.root, "stop-after-ff")
	text := "#!/bin/sh\ncase \" $* \" in *' diff --name-only --diff-filter=U '*) exit 1;; esac\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(script, []byte(text), 0700); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Git = script
	_, err = f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("expected injected interruption")
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.git(w.Tree, "rev-parse", "HEAD"); got != remote || saved.Stage != "test" || hasJSON(saved.Test) || hasJSON(saved.Review) {
		t.Fatalf("head=%s remote=%s record=%+v", got, remote, saved)
	}
}

func TestBranchSummaryAloneDoesNotSuppressTransferError(t *testing.T) {
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: 1, Result: json.RawMessage(`{"acceptanceTransfers":null}`)}
	state, err := f.Executor.finish(r, "held", "review_held", errTargetBranchDiverged.Error(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(state.Result, &result); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result["summary"].(string), "受入条件の移送を報告できません:") {
		t.Fatalf("result=%v", result)
	}
}
