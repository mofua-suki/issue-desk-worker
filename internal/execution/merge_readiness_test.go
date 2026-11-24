package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
)

func assertTurnReleased(t *testing.T, f *pipelineFixture) {
	t.Helper()
	if len(f.TurnCalls) == 0 || !strings.HasSuffix(f.TurnCalls[len(f.TurnCalls)-1], "/release") {
		t.Fatalf("merge turn was not released: %v", f.TurnCalls)
	}
}

func heldPullChecks(t *testing.T, state intake.State) (string, []pullCheck, bool) {
	t.Helper()
	var view struct {
		Check struct {
			Phase   string      `json:"phase"`
			Checks  []pullCheck `json:"checks"`
			Waiting bool        `json:"waiting"`
		} `json:"mergePullCheck"`
	}
	if state.Status != "held" || state.HoldReason != "review_held" || json.Unmarshal(state.View, &view) != nil || len(view.Check.Checks) == 0 {
		t.Fatalf("missing held diagnostics: %+v", state)
	}
	return view.Check.Phase, view.Check.Checks, view.Check.Waiting
}

func TestMergeReadinessTransientFalse(t *testing.T) {
	for _, phase := range []string{"initial", "final"} {
		t.Run(phase, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.Executor.mergeReadWait = func(context.Context, time.Duration) error { return nil }
			read := map[string]int{"initial": 4, "final": 5}[phase]
			falseSeen := false
			f.PullOverride = func(p *gitea.Pull) {
				// Repository names retain the case-insensitive identity contract.
				p.Head.Repo.FullName, p.Base.Repo.FullName = "OWNER/Project", "Owner/PROJECT"
				if f.PullReads == read {
					p.Mergeable = false
					falseSeen = true
				}
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || !falseSeen || f.MergePosts != 1 || len(f.Reviews) != 1 {
				t.Fatalf("false did not recover in same execution: status=%s err=%v reads=%d", state.Status, err, f.PullReads)
			}
			assertTurnReleased(t, f)
		})
	}
}

func TestMergeReadinessRetryLimitWithoutRealWait(t *testing.T) {
	f := newPipeline(t, "approve")
	waits := 0
	f.Executor.mergeReadWait = func(ctx context.Context, delay time.Duration) error {
		if delay <= 0 || delay > mergeReadInterval {
			t.Fatalf("invalid retry delay: %v", delay)
		}
		waits++
		return ctx.Err()
	}
	f.PullOverride = func(p *gitea.Pull) {
		if f.PullReads >= 4 {
			p.Mergeable = false
		}
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || f.MergePosts != 0 || waits < 1 || waits > mergeReadRetries || f.PullReads > 4+waits {
		t.Fatalf("err=%v merge posts=%d waits=%d reads=%d", err, f.MergePosts, waits, f.PullReads)
	}
	phase, checks, waiting := heldPullChecks(t, state)
	if phase != "initial" || !waiting || !reflect.DeepEqual(checks, []pullCheck{{"mergeable", "unconfirmed"}}) {
		t.Fatalf("unexpected hold: phase=%s checks=%v waiting=%v", phase, checks, waiting)
	}
	assertTurnReleased(t, f)
}

func TestMergeReadinessRemainingBudgetLimitsWait(t *testing.T) {
	f := newPipeline(t, "approve")
	now := time.Unix(1, 0)
	var delays []time.Duration
	f.Executor.mergeReadNow = func() time.Time { return now }
	f.Executor.mergeReadWait = func(ctx context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		if len(delays) == 1 {
			now = now.Add(mergeReadLimit - 250*time.Millisecond)
		} else {
			now = now.Add(delay)
		}
		return ctx.Err()
	}
	f.PullOverride = func(p *gitea.Pull) {
		if f.PullReads >= 4 {
			p.Mergeable = false
		}
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	_, checks, waiting := heldPullChecks(t, state)
	if err != nil || f.MergePosts != 0 || f.PullReads != 5 || !waiting || !reflect.DeepEqual(checks, []pullCheck{{"mergeable", "unconfirmed"}}) || !reflect.DeepEqual(delays, []time.Duration{time.Second, 250 * time.Millisecond}) {
		t.Fatalf("unexpected budget behavior: err=%v reads=%d waits=%v checks=%v", err, f.PullReads, delays, checks)
	}
	assertTurnReleased(t, f)
}

func TestMergeReadinessMismatchFields(t *testing.T) {
	parallelCases := make(chan struct{}, 4)
	for _, tc := range []struct {
		name   string
		change func(*gitea.Pull)
		want   []pullCheck
	}{
		{"number", func(p *gitea.Pull) { p.Number = 0 }, []pullCheck{{"number", "mismatch"}}},
		{"state", func(p *gitea.Pull) { p.State = "closed" }, []pullCheck{{"state", "mismatch"}}},
		{"draft", func(p *gitea.Pull) { p.Draft = true }, []pullCheck{{"draft", "mismatch"}}},
		{"head.ref", func(p *gitea.Pull) { p.Head.Ref = "private-response-value" }, []pullCheck{{"head.ref", "mismatch"}}},
		{"base.ref", func(p *gitea.Pull) { p.Base.Ref = "private-response-value" }, []pullCheck{{"base.ref", "mismatch"}}},
		{"head.sha", func(p *gitea.Pull) { p.Head.SHA = "private-response-value" }, []pullCheck{{"head.sha", "mismatch"}}},
		{"head.repo.full_name", func(p *gitea.Pull) { p.Head.Repo.FullName = "private-response-value" }, []pullCheck{{"head.repo.full_name", "mismatch"}}},
		{"base.repo.full_name", func(p *gitea.Pull) { p.Base.Repo.FullName = "private-response-value" }, []pullCheck{{"base.repo.full_name", "mismatch"}}},
		{"multiple", func(p *gitea.Pull) {
			p.State = "closed"
			p.Draft = true
			p.Head.SHA = "private-response-value"
			p.Mergeable = false
		}, []pullCheck{{"state", "mismatch"}, {"draft", "mismatch"}, {"head.sha", "mismatch"}, {"mergeable", "unconfirmed"}}},
	} {
		for _, phase := range []string{"initial", "final"} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				parallelCases <- struct{}{}
				defer func() { <-parallelCases }()
				f := newPipeline(t, "approve")
				read := map[string]int{"initial": 4, "final": 5}[phase]
				f.PullOverride = func(p *gitea.Pull) {
					if f.PullReads == read {
						tc.change(p)
					}
				}
				state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				if err != nil || f.MergePosts != 0 || f.PullReads != read {
					t.Fatalf("mismatch not held immediately: status=%s err=%v reads=%d", state.Status, err, f.PullReads)
				}
				gotPhase, checks, waiting := heldPullChecks(t, state)
				if gotPhase != phase || !reflect.DeepEqual(checks, tc.want) || waiting {
					t.Fatalf("diagnostics: phase=%s checks=%v waiting=%v", gotPhase, checks, waiting)
				}
				saved, err := f.Executor.Store.Load(f.Job.ID)
				if err != nil || saved == nil || !bytes.Equal(saved.FinalState, encode(state)) || !hasJSON(saved.Test) || !hasJSON(saved.Review) || strings.Contains(string(saved.FinalState), "private-response-value") {
					t.Fatal("held result lost artifacts/diagnostics or leaked values", err)
				}
				assertTurnReleased(t, f)
			})
		}
	}
}

func TestMergeReadinessTimeoutSavedAndResent(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	now := time.Unix(1, 0)
	waits := 0
	f.Executor.mergeReadNow = func() time.Time { return now }
	f.Executor.mergeReadWait = func(ctx context.Context, delay time.Duration) error {
		waits++
		now = now.Add(mergeReadLimit)
		return ctx.Err()
	}
	f.PullOverride = func(p *gitea.Pull) {
		if f.PullReads >= 4 {
			p.Mergeable = false
		}
	}
	a := &errorResumeAPI{Client: f.API, assignment: intake.Assignment{Job: &f.Job, Lease: &intake.Lease{Epoch: f.Job.LeaseEpoch, ExpiresAt: time.Now().Add(time.Hour)}}, rejectFinal: true}
	runErrorResumeWithAPI(t, f, a)
	saved, err := f.Executor.Store.Load(f.Job.ID)
	if err != nil || saved == nil || saved.FinalReported || saved.Stage != "held" || a.errorReports != 0 || f.MergePosts != 0 || !hasJSON(saved.Test) || !hasJSON(saved.Review) {
		t.Fatal("timeout did not preserve unacknowledged hold", saved, err)
	}
	var state intake.State
	if json.Unmarshal(saved.FinalState, &state) != nil {
		t.Fatal("invalid saved hold")
	}
	_, checks, waiting := heldPullChecks(t, state)
	if !waiting || !reflect.DeepEqual(checks, []pullCheck{{"mergeable", "unconfirmed"}}) || f.PullReads != 4 || waits != 1 || now.Sub(time.Unix(1, 0)) != mergeReadLimit {
		t.Fatalf("unbounded or misclassified wait: checks=%v waiting=%v reads=%d", checks, waiting, f.PullReads)
	}
	assertTurnReleased(t, f)
	before, calls := append([]byte(nil), saved.FinalState...), len(f.Calls)
	f.PullOverride = nil
	runErrorResume(t, f)
	saved, err = f.Executor.Store.Load(f.Job.ID)
	if err != nil || !saved.FinalReported || !bytes.Equal(before, saved.FinalState) || len(f.Calls) != calls || f.MergePosts != 0 {
		t.Fatal("saved held result was replaced or not acknowledged", err)
	}
	got := f.ReceivedStates[len(f.ReceivedStates)-1]
	if !bytes.Equal(encode(got), before) {
		t.Fatal("intake did not receive the original saved result")
	}
}

func TestMergeReadinessRechecksProtectionAfterWait(t *testing.T) {
	parallelCases := make(chan struct{}, 4)
	for _, kind := range []string{"stop", "lease", "epoch", "turn", "policy", "permission", "deletion", "review"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			parallelCases <- struct{}{}
			defer func() { <-parallelCases }()
			mode := "approve"
			if kind == "deletion" {
				mode = "sql"
			}
			f := newPipeline(t, mode)
			f.Executor.mergeReadWait = func(context.Context, time.Duration) error { return nil }
			waitRead := 5
			if kind == "deletion" {
				prepareMergeRecovery(t, f)
				f.CurrentPolicy = nil
				r, err := f.Executor.Store.Load(f.Job.ID)
				if err != nil {
					t.Fatal(err)
				}
				f.CurrentView = map[string]json.RawMessage{"deletionApproved": encode(map[string]any{"head": r.Head})}
				f.PullReads = 0
				waitRead = 4
			}
			waiting := false
			leaseLost := errors.New("fixture lease lost")
			f.PullOverride = func(p *gitea.Pull) {
				if f.PullReads == waitRead {
					p.Mergeable = false
					waiting = true
				}
			}
			applied := false
			waitFences := 0
			fence := func(context.Context) error {
				if !waiting || applied {
					return nil
				}
				waitFences++
				if kind == "lease" && waitFences == 1 {
					return nil
				}
				applied = true
				switch kind {
				case "stop":
					f.Commands = []map[string]any{{"id": "stop-wait", "kind": "stop", "payload": map[string]any{"jobId": 1, "epoch": 3}}}
				case "lease":
					return leaseLost
				case "epoch":
					f.CurrentLeaseEpoch = 4
				case "turn":
					f.TurnRenewStatus = http.StatusConflict
				case "policy":
					f.CurrentPolicy = currentPolicy(f, false)
				case "permission":
					f.CurrentPolicy = currentPolicy(f, true)
					f.CurrentPolicy.ReleasePolicy = &ReleasePolicy{PermissionFiles: []string{"feature.txt"}}
				case "deletion":
					f.CurrentView = nil
				case "review":
					f.Reviews = append(f.Reviews, map[string]any{"state": "REQUEST_CHANGES"})
				}
				return nil
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, fence)
			if !applied || f.MergePosts != 0 {
				t.Fatalf("protection not exercised: status=%s err=%v", state.Status, err)
			}
			if kind == "lease" || kind == "epoch" || kind == "turn" {
				saved, loadErr := f.Executor.Store.Load(f.Job.ID)
				if err == nil || loadErr != nil || hasJSON(saved.FinalState) {
					t.Fatal("lost ownership finalized a result", err, loadErr)
				}
			} else if err != nil || (kind == "stop" && state.Status != "stopped") || (kind != "stop" && state.Status != "held") {
				t.Fatalf("unsafe continuation: status=%s err=%v", state.Status, err)
			}
			assertTurnReleased(t, f)
		})
	}
}

func TestMergeReadinessTargetChangeRetests(t *testing.T) {
	for _, target := range []string{"base", "head"} {
		t.Run(target, func(t *testing.T) {
			f := newPipeline(t, "approve")
			diffLog := logMergeDiffs(t, f)
			f.Executor.mergeReadWait = func(context.Context, time.Duration) error { return nil }
			advanced := false
			if target == "head" {
				f.ScopeAdd = func(w http.ResponseWriter, add []string) {
					if !reflect.DeepEqual(add, []string{"README.md"}) {
						t.Errorf("unexpected scope addition: %v", add)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"result": "granted", "job": map[string]any{"id": 1, "scope": []string{"README.md", "feature.txt"}}})
				}
			}
			var oldHead string
			f.PullOverride = func(p *gitea.Pull) {
				if f.PullReads == 5 {
					p.Mergeable = false
					r, err := f.Executor.Store.Load(f.Job.ID)
					if err != nil {
						t.Fatal(err)
					}
					oldHead = r.Head
					source := filepath.Join(f.root, "source")
					if target == "head" {
						source = r.Tree
					}
					if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("advanced target\n"), 0600); err != nil {
						t.Fatal(err)
					}
					f.git(source, "add", "README.md")
					f.git(source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "advance during readiness wait")
					if target == "base" {
						f.git(source, "push", f.bare, "main")
					}
					advanced = true
				}
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || !advanced || f.MergePosts != 1 || len(f.Reviews) != 2 {
				t.Fatalf("changed target not revalidated: status=%s err=%v reviews=%d", state.Status, err, len(f.Reviews))
			}
			saved, err := f.Executor.Store.Load(f.Job.ID)
			var tested TestResult
			if err != nil || json.Unmarshal(saved.Test, &tested) != nil || testEvidenceReason(tested, saved.Head, saved.Base) != "" || saved.Head == oldHead {
				t.Fatal("merged head does not match new fixed test", err)
			}
			runs, err := os.ReadFile(diffLog)
			if err != nil || !strings.Contains(string(runs), oldHead+"\n") || !strings.Contains(string(runs), saved.Head+"\n") {
				t.Fatalf("changed commit did not get a fresh diff: %s (%v)", runs, err)
			}
			assertTurnReleased(t, f)
		})
	}
}

func TestMergeReadinessBudgetDoesNotInterruptGit(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"success", "fetch-error"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			f := newPipeline(t, "approve")
			armed := filepath.Join(f.root, "delay-fetch")
			finished := filepath.Join(f.root, "fetch-finished")
			started := filepath.Join(f.root, "fetch-started")
			resume := filepath.Join(f.root, "fetch-resume")
			var clockMu sync.Mutex
			now := time.Unix(1, 0)
			f.Executor.mergeReadNow = func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()
				return now
			}
			f.Executor.mergeReadWait = func(ctx context.Context, delay time.Duration) error {
				clockMu.Lock()
				now = now.Add(delay)
				clockMu.Unlock()
				return ctx.Err()
			}
			wrapper := filepath.Join(f.root, "git-wrapper")
			// Arm only after the final PR check returns false, so the next
			// fetch runs during the retry. It outlasts the confirmation budget.
			exitCode := 0
			if outcome == "fetch-error" {
				exitCode = 73
			}
			script := fmt.Sprintf(`#!/bin/sh
case " $* " in
  *' fetch '*)
    if [ -f '%s' ]; then
      rm '%s'
      touch '%s'
      while [ ! -f '%s' ]; do sleep 0.01; done
      /usr/bin/git "$@" || exit "$?"
      touch '%s'
      exit %d
    fi
    ;;
esac
exec /usr/bin/git "$@"
`, armed, armed, started, resume, finished, exitCode)
			if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			f.Executor.Config.Git = wrapper
			done := make(chan struct{})
			defer close(done)
			clockAdvanced := make(chan error, 1)
			go func() {
				for {
					if _, err := os.Stat(started); err == nil {
						clockMu.Lock()
						now = now.Add(mergeReadLimit)
						clockMu.Unlock()
						clockAdvanced <- os.WriteFile(resume, nil, 0600)
						return
					}
					select {
					case <-done:
						return
					case <-time.After(10 * time.Millisecond):
					}
				}
			}()
			f.PullOverride = func(p *gitea.Pull) {
				if f.PullReads == 5 {
					p.Mergeable = false
					if err := os.WriteFile(armed, nil, 0600); err != nil {
						t.Error(err)
					}
				}
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if _, markerErr := os.Stat(started); markerErr != nil {
				t.Fatal("fixture did not cross confirmation budget during fetch")
			}
			if advanceErr := <-clockAdvanced; advanceErr != nil {
				t.Fatal(advanceErr)
			}
			if _, markerErr := os.Stat(finished); markerErr != nil {
				t.Fatalf("git did not finish: %v (execution error: %v)", markerErr, err)
			}
			saved, loadErr := f.Executor.Store.Load(f.Job.ID)
			if loadErr != nil || saved == nil {
				t.Fatal("missing execution record", loadErr)
			}
			// The common Git directory includes the mirror and linked worktree
			// metadata; Worker ownership locks are outside this directory.
			common := f.git(saved.Tree, "rev-parse", "--git-common-dir")
			if !filepath.IsAbs(common) {
				common = filepath.Join(saved.Tree, common)
			}
			if walkErr := filepath.WalkDir(common, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if !entry.IsDir() && (strings.HasSuffix(entry.Name(), ".lock") || entry.Name() == "MERGE_HEAD") {
					t.Errorf("unfinished git state: %s", path)
				}
				return nil
			}); walkErr != nil {
				t.Fatal(walkErr)
			}
			if outcome == "success" {
				if err != nil || state.Status != "merged" || f.MergePosts != 1 || !bytes.Equal(saved.FinalState, encode(state)) {
					t.Fatalf("completed git did not continue: status=%s merges=%d err=%v", state.Status, f.MergePosts, err)
				}
			} else {
				var hold *releaseHold
				if err == nil || errors.As(err, &hold) || !strings.Contains(err.Error(), "73") || hasJSON(saved.FinalState) || f.MergePosts != 0 {
					t.Fatalf("git failure became a confirmation hold: status=%s final=%s merges=%d err=%v", state.Status, saved.FinalState, f.MergePosts, err)
				}
			}
			assertTurnReleased(t, f)
		})
	}
}

func TestMergeReadinessReusesDiffWithinAttempt(t *testing.T) {
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	r, err := f.Executor.Store.Load(f.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	logPath := logMergeDiffs(t, f)
	w, err := gitwork.Open(t.Context(), f.Executor.gitConfig(), r.Repo, r.TargetBase, r.Branch, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	merged, changed, err := f.Executor.merge(t.Context(), d, r, w, func(context.Context) error { return nil })
	if err != nil || !merged || changed || f.MergePosts != 1 {
		t.Fatalf("merge did not complete: merged=%v changed=%v posts=%d err=%v", merged, changed, f.MergePosts, err)
	}
	runs, err := os.ReadFile(logPath)
	if err != nil || string(runs) != "paths "+r.Head+"\npatch "+r.Head+"\n" {
		t.Fatalf("expected one diff for the unchanged target, got %q (%v)", runs, err)
	}
}

func logMergeDiffs(t *testing.T, f *pipelineFixture) string {
	t.Helper()
	logPath := filepath.Join(f.root, "merge-diffs")
	wrapper := filepath.Join(f.root, "count-git-diffs")
	script := fmt.Sprintf(`#!/bin/sh
case " $* " in
  *' diff --no-ext-diff --no-textconv --no-renames '*) for head do :; done; printf 'patch %%s\n' "$head" >> %q ;;
  *' diff --name-status -z --no-renames '*) for head do :; done; printf 'paths %%s\n' "$head" >> %q ;;
esac
exec /usr/bin/git "$@"
`, logPath, logPath)
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Git = wrapper
	return logPath
}
