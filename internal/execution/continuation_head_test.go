package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func continuationDescriptor(t *testing.T, f *pipelineFixture, head string) Descriptor {
	t.Helper()
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.HeadSHA, d.Branch = head, f.Branch
	return d
}

func continuationWorkspace(t *testing.T, f *pipelineFixture) *gitwork.Workspace {
	t.Helper()
	w, err := gitwork.Open(context.Background(), gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func continuationCommit(t *testing.T, f *pipelineFixture, tree, name, text string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(tree, name), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(tree, "add", name)
	f.git(tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "continuation fixture")
	return f.git(tree, "rev-parse", "HEAD")
}

// This wraps only isolated fixture Git. Unexamined stderr must never enter a hold.
func continuationGitFailure(t *testing.T, f *pipelineFixture, pattern string) {
	t.Helper()
	script := filepath.Join(f.root, "continuation-git")
	body := "#!/bin/sh\ncase \" $* \" in\n  *'" + pattern + "'*) echo 'untrusted-git-output' >&2; exit 128;;\nesac\nexec /usr/bin/git \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Git = script
}

func continuationFixedMarker(t *testing.T, f *pipelineFixture) string {
	t.Helper()
	marker := filepath.Join(f.root, "fixed-test-started")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "touch '"+marker+"'")
	return marker
}

func continuationSameJSON(a, b json.RawMessage) bool {
	if !hasJSON(a) && !hasJSON(b) {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func assertContinuationBlocked(t *testing.T, f *pipelineFixture, state intake.State, marker, cause string, before *journal.Record) *journal.Record {
	t.Helper()
	if state.Status != "held" || state.HoldReason != "review_held" || len(f.Calls) != 0 {
		t.Fatalf("not held before AI: %+v calls=%v", state, f.Calls)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixed test started: %v", err)
	}
	var view struct {
		Head struct {
			Required string `json:"requiredHead"`
			Cause    string `json:"cause"`
		} `json:"continuationHead"`
	}
	if err := json.Unmarshal(state.View, &view); err != nil || view.Head.Cause != cause {
		t.Fatalf("hold cause: %s, %v", state.View, err)
	}
	var descriptor Descriptor
	_ = json.Unmarshal(f.Job.Raw, &descriptor)
	wantRequired := ""
	if gitwork.ValidSHA(descriptor.HeadSHA) {
		wantRequired = descriptor.HeadSHA
	}
	if view.Head.Required != wantRequired {
		t.Fatalf("required commit missing from hold: %s", state.View)
	}
	r, err := f.Executor.Store.Load(1)
	if err != nil || r == nil {
		t.Fatalf("saved record: %v %v", r, err)
	}
	if r.Runs != before.Runs || r.ReportCompletionRuns != before.ReportCompletionRuns || r.UnansweredRuns != before.UnansweredRuns || r.InterruptedRuns != before.InterruptedRuns || r.OpenRun != before.OpenRun || r.WeightedUsage != before.WeightedUsage || !reflect.DeepEqual(r.Usage, before.Usage) || r.UsageSent != before.UsageSent {
		t.Fatalf("counts/usage changed: before=%+v after=%+v", before, r)
	}
	if state.HeadSHA != before.Head || r.Head != before.Head || !continuationSameJSON(r.Result, before.Result) || !continuationSameJSON(r.Review, before.Review) || !continuationSameJSON(r.Test, before.Test) || !reflect.DeepEqual(r.ReportCompletion, before.ReportCompletion) {
		t.Fatalf("evidence changed: before=%+v after=%+v state=%+v", before, r, state)
	}
	if strings.Contains(string(state.View)+string(state.Result), "untrusted-git-output") {
		t.Fatal("raw error leaked")
	}
	return r
}

func TestContinuationHoldStartHistoryLeaseBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		epoch          int64
		local, started bool
		wantExcluded   bool
	}{
		{"continuous local", 2, true, false, true},
		{"lease gap", 3, true, false, false},
		{"remote only", 2, false, false, false},
		{"copied journal", 2, true, false, false},
		{"already started", 2, true, true, false},
		{"old checkpoint", 2, true, false, false},
		{"wrong history generation", 2, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			d := continuationDescriptor(t, f, strings.Repeat("a", 40))
			d.Generation = 2
			gen := 2
			checkpoint := startedExecution{Generation: &gen, StartHistoryGeneration: &gen, StartedGenerations: []int{}, Runs: 3, ReportCompletionRuns: 1, UnansweredRuns: 1}
			if tc.started {
				checkpoint.StartedGenerations = []int{2}
			}
			if tc.name == "old checkpoint" {
				checkpoint.StartHistoryGeneration = nil
			}
			if tc.name == "wrong history generation" {
				oldGen := 1
				checkpoint.StartHistoryGeneration = &oldGen
			}
			d.View["workerExecution"] = encode(checkpoint)
			f.Job.Raw = encode(d)
			f.Job.LeaseEpoch = tc.epoch
			if tc.local {
				r := &journal.Record{ID: 1, Repo: d.Repo, Issue: d.Issue, Generation: 2, Epoch: 1, Branch: f.Branch, Stage: "implement", Runs: 3, ReportCompletionRuns: 1, UnansweredRuns: 1, StartHistory: &journal.StartHistory{Generation: 2, Epoch: 1, Known: true, Host: "fixture"}}
				if tc.name == "copied journal" {
					r.StartHistory.Host = "another-worker"
				}
				if tc.started {
					r.StartedGenerations = []int{2}
				}
				if err := f.Executor.Store.Save(r); err != nil {
					t.Fatal(err)
				}
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if state.Status != "held" || len(f.Calls) != 0 {
				t.Fatalf("state=%+v calls=%v", state, f.Calls)
			}
			r, err := f.Executor.Store.Load(d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(r.ExcludedGenerations, 2) != tc.wantExcluded {
				t.Fatalf("excluded=%v", r.ExcludedGenerations)
			}
			var view struct {
				Worker startedExecution `json:"workerExecution"`
			}
			if err := json.Unmarshal(state.View, &view); err != nil {
				t.Fatal(err)
			}
			if view.Worker.Runs != r.Runs || !reflect.DeepEqual(view.Worker.ExcludedGenerations, r.ExcludedGenerations) {
				t.Fatalf("view=%+v journal=%+v", view.Worker, r)
			}
			if tc.wantExcluded {
				next := newPipeline(t, "approve")
				completionAPI(t, next)
				next.Executor.Config.Assignments.ImplementationRunLimit = 5
				w := continuationWorkspace(t, next)
				nextDesc := continuationDescriptor(t, next, next.git(w.Tree, "rev-parse", "HEAD"))
				nextDesc.Generation = 3
				var rawView map[string]json.RawMessage
				if err := json.Unmarshal(state.View, &rawView); err != nil {
					t.Fatal(err)
				}
				nextDesc.View["workerExecution"] = rawView["workerExecution"]
				next.Job.Raw = encode(nextDesc)
				calls := 0
				askingCall(next, &calls)
				out, err := execute(next)
				if err != nil || calls != 1 || out.HoldReason != "question" {
					t.Fatalf("next=%+v calls=%d err=%v", out, calls, err)
				}
				reloaded, err := next.Executor.Store.Load(1)
				if err != nil || reloaded.Runs != 5 || !slices.Contains(reloaded.ExcludedGenerations, 2) {
					t.Fatalf("next journal=%+v err=%v", reloaded, err)
				}
				acknowledgeFinal(t, f)
				d.View["workerExecution"] = rawView["workerExecution"]
				f.Job.Raw, f.Job.LeaseEpoch = encode(d), 4
				again, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				if err != nil || again.Status != "held" {
					t.Fatalf("gap state=%+v err=%v", again, err)
				}
				afterGap, err := f.Executor.Store.Load(1)
				if err != nil || slices.Contains(afterGap.ExcludedGenerations, 2) || afterGap.Runs != 4 {
					t.Fatalf("gap journal=%+v err=%v", afterGap, err)
				}
			}
		})
	}
}

func TestContinuationHistoryEstablishedBeforeConsecutiveHolds(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	d := continuationDescriptor(t, f, strings.Repeat("a", 40))
	f.Job.LeaseEpoch = 1
	previousRuns := -1
	for _, generation := range []int{0, 1, 1, 1} {
		d.Generation = generation
		f.Job.Raw = encode(d)
		state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
		if err != nil || state.Status != "held" || len(f.Calls) != 0 {
			t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
		}
		r, err := f.Executor.Store.Load(1)
		if err != nil {
			t.Fatal(err)
		}
		want := f.Job.LeaseEpoch >= 2
		if slices.Contains(r.ExcludedGenerations, 1) != want {
			t.Fatalf("epoch=%d exclusions=%v", f.Job.LeaseEpoch, r.ExcludedGenerations)
		}
		if f.Job.LeaseEpoch == 4 && r.Runs != previousRuns {
			t.Fatalf("reprocessing changed runs: %d to %d", previousRuns, r.Runs)
		}
		previousRuns = r.Runs
		var v map[string]json.RawMessage
		if err := json.Unmarshal(state.View, &v); err != nil {
			t.Fatal(err)
		}
		var checkpoint startedExecution
		if err := json.Unmarshal(v["workerExecution"], &checkpoint); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(checkpoint.ContinuationExcludedGenerations, 1) != want {
			t.Fatalf("epoch=%d checkpoint exclusions=%v", f.Job.LeaseEpoch, checkpoint.ContinuationExcludedGenerations)
		}
		d.View["workerExecution"] = v["workerExecution"]
		acknowledgeFinal(t, f)
		f.Job.LeaseEpoch++
	}
}

func TestContinuationHeadBlocksBeforeEveryExecutionStage(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"implement", "implement_report", "test", "test_report", "review", "publish", "merge"} {
		t.Run(stage, func(t *testing.T) {
			f := newPipeline(t, "approve")
			marker := continuationFixedMarker(t, f)
			head := strings.Repeat("a", 40)
			d := continuationDescriptor(t, f, head)
			f.Job.Raw = encode(d)
			before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: stage, Head: head, Runs: 5, OpenRun: 5, InterruptedRuns: maxInterruptedRuns, UnansweredRuns: 1, ReportCompletionRuns: 2, WeightedUsage: 91, Result: implementationFixture(), Review: encode(map[string]any{"decision": "hold", "summary": "previous review"}), Test: encode(TestResult{Passed: true}), Usage: []json.RawMessage{encode(map[string]any{"existing": true})}, ReportCompletion: &journal.ReportCompletion{ID: "original", Original: implementationFixture(), Attempted: true}}
			if err := f.Executor.Store.Save(before); err != nil {
				t.Fatal(err)
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			assertContinuationBlocked(t, f, state, marker, "commit_unavailable", before)
		})
	}
}

// Regression for #5347 job3580/generation0: drain retained the branch name and
// reports but did not prove that the new PC received the previous fixture/code.
func TestContinuationHeadDrainToNewPCKeepsDeliveredEvidence(t *testing.T) {
	t.Parallel()
	// The later 917ef4ed head is a requirement too, never proof that the
	// improved fixtures reported on nab-9s were received by gt12.
	for _, head := range []string{"39b24f41d0076b29c1626bff7685959c9b4564d6", "917ef4ed8241264c08859c49c32f0a7985a8d382"} {
		t.Run(head, func(t *testing.T) {
			f := newPipeline(t, "approve")
			marker := continuationFixedMarker(t, f)
			d := continuationDescriptor(t, f, head)
			review := encode(map[string]string{"summary": "completed fixture and fake intake still required"})
			d.Result = encode(map[string]any{"implementationResult": implementationFixture(), "review": review, "implementationContent": head, "fixedTest": encode(TestResult{Passed: true})})
			d.View["workerExecution"] = encode(map[string]any{"implementationRuns": 7, "reportCompletionRuns": 2, "unansweredRuns": 1, "openRun": 7, "openRunGeneration": 0, "interruptedRuns": 2, "weightedUsage": 123})
			d.View["continuation"] = encode(map[string]any{"host": "nab-9s", "pending": false, "to": "gt12"})
			f.Job.Raw = encode(d)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			before := &journal.Record{Runs: 7, ReportCompletionRuns: 2, UnansweredRuns: 1, OpenRun: 7, InterruptedRuns: 2, WeightedUsage: 123, Head: head, Test: encode(TestResult{Passed: true})}
			r := assertContinuationBlocked(t, f, state, marker, "commit_unavailable", before)
			if !reflect.DeepEqual(r.PreviousResult, implementationFixture()) || !reflect.DeepEqual(r.PreviousReview, review) || !strings.Contains(string(state.Result), "previousImplementationResult") || !strings.Contains(string(state.Result), "completed fixture") {
				t.Fatalf("delivered evidence lost: %+v %s", r, state.Result)
			}
			if got := f.git(r.Tree, "rev-parse", "HEAD"); got == head || state.HeadSHA != head {
				t.Fatal("default HEAD replaced requested evidence")
			}
		})
	}
}

func TestContinuationHeadRejectsMissingOldAndUnverifiableTrees(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, failure, cause string }{
		{"old tree", "", "not_included"},
		{"same file only", "", "not_included"},
		{"missing commit", " cat-file -t ", "commit_unavailable"},
		{"status failed", " status --porcelain -z ", "status_unavailable"},
		{"HEAD failed", " rev-parse HEAD ", "head_unavailable"},
		{"comparison failed", " merge-base --is-ancestor ", "comparison_unavailable"},
		{"target comparison failed", " show-ref --verify --quiet ", "target_comparison_unavailable"},
		{"open fetch failed", " fetch --prune origin ", "workspace_unavailable"},
		{"invalid SHA", "", "invalid_sha"},
		{"blob SHA", "", "not_commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			w := continuationWorkspace(t, f)
			head := continuationCommit(t, f, w.Tree, "feature.txt", "accepted feature\n")
			base := f.git(w.Tree, "rev-parse", "HEAD^")
			f.git(w.Tree, "reset", "--hard", base)
			if tc.name == "same file only" {
				if err := os.WriteFile(filepath.Join(w.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "invalid SHA" {
				head = "untrusted-git-output;--HEAD"
			}
			if tc.name == "blob SHA" {
				head = f.git(w.Tree, "rev-parse", "HEAD:README.md")
			}
			d := continuationDescriptor(t, f, head)
			f.Job.Raw = encode(d)
			before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Stage: "implement", Branch: f.Branch, Runs: 9, Head: base, WeightedUsage: 23, PreviousResult: implementationFixture()}
			if err := f.Executor.Store.Save(before); err != nil {
				t.Fatal(err)
			}
			if tc.failure != "" {
				continuationGitFailure(t, f, tc.failure)
			}
			marker := continuationFixedMarker(t, f)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			assertContinuationBlocked(t, f, state, marker, tc.cause, before)
		})
	}
}

func TestContinuationHeadAllowsIncludedLocalPublishedAndFollowedTrees(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"local equal", "local ancestor", "published new PC", "target FF", "held target FF", "first without head"} {
		t.Run(mode, func(t *testing.T) {
			f := newPipeline(t, "approve")
			w := continuationWorkspace(t, f)
			base := f.git(w.Tree, "rev-parse", "HEAD")
			head := continuationCommit(t, f, w.Tree, "feature.txt", "accepted feature\n")
			if mode == "local ancestor" {
				continuationCommit(t, f, w.Tree, "README.md", "descendant\n")
			}
			if mode == "published new PC" || strings.Contains(mode, "target FF") {
				if err := w.Push(context.Background()); err != nil {
					t.Fatal(err)
				}
				if mode == "published new PC" {
					runtime := t.TempDir()
					store, err := journal.Open(runtime)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = store.Close() })
					f.Executor.Store, f.Executor.Config.RuntimeDir = store, runtime
				} else {
					f.git(w.Tree, "reset", "--hard", base)
					stage := "review"
					if mode == "held target FF" {
						stage = "held"
					}
					r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: stage, Branch: f.Branch, Provider: "codex", Head: base, Result: implementationFixture(), Review: encode(map[string]any{"decision": "approve", "summary": "old tree approval"}), Test: encode(TestResult{Passed: true})}
					if err := f.Executor.Store.Save(r); err != nil {
						t.Fatal(err)
					}
				}
			}
			d := continuationDescriptor(t, f, head)
			if mode == "first without head" {
				d.HeadSHA = ""
			}
			f.Job.Raw = encode(d)
			marker := continuationFixedMarker(t, f)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("fixed test did not run: %v", err)
			}
			if strings.Contains(mode, "target FF") {
				r, _ := f.Executor.Store.Load(1)
				if len(f.Calls) != 1 || f.Calls[0] != "claude" || !strings.Contains(string(r.PreviousReview), "old tree approval") {
					t.Fatalf("stale approval or lost evidence: calls=%v saved=%+v", f.Calls, r)
				}
			} else if len(f.Calls) != 2 {
				t.Fatalf("normal pipeline did not run: %v", f.Calls)
			}
		})
	}
}

func TestContinuationHeadRetryContractAndGeneration(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"explicit old head", "wrong generation", "retry continuation missing", "retry continuation included", "applied marker without instruction", "new generation missing"} {
		t.Run(mode, func(t *testing.T) {
			f := newPipeline(t, "approve")
			d := continuationDescriptor(t, f, strings.Repeat("a", 40))
			d.View["retryFromBase"] = encode(map[string]int{"generation": 0})
			before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "implement", Runs: 0, WeightedUsage: 19, Result: implementationFixture()}
			if mode == "wrong generation" {
				d.View["retryFromBase"] = encode(map[string]int{"generation": 1})
			}
			if strings.HasPrefix(mode, "retry continuation") || mode == "applied marker without instruction" {
				f.Branch = "codex/issue-7-job-1-retry-0"
				before.Branch = f.Branch
				generation := 0
				before.RetryBaseGeneration = &generation
				if mode != "applied marker without instruction" {
					d.Branch = f.Branch
				} else {
					delete(d.View, "retryFromBase")
				}
				if mode == "retry continuation included" {
					w := continuationWorkspace(t, f)
					d.HeadSHA = continuationCommit(t, f, w.Tree, "feature.txt", "accepted feature\n")
				}
			}
			if mode == "new generation missing" {
				d.Generation = 1
				delete(d.View, "retryFromBase")
				before.Runs, before.Head = 8, d.HeadSHA
			}
			f.Job.Raw = encode(d)
			if err := f.Executor.Store.Save(before); err != nil {
				t.Fatal(err)
			}
			marker := continuationFixedMarker(t, f)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if mode == "explicit old head" || mode == "retry continuation included" {
				if state.Status != "merged" || len(f.Calls) != 2 {
					t.Fatalf("valid retry blocked: %+v calls=%v", state, f.Calls)
				}
			} else {
				before.Head = d.HeadSHA // a new journal receives the delivered head as evidence
				r := assertContinuationBlocked(t, f, state, marker, "commit_unavailable", before)
				if r.Generation != before.Generation || !reflect.DeepEqual(r.RetryBaseGeneration, before.RetryBaseGeneration) {
					t.Fatalf("generation/reset mutated on hold: %+v", r)
				}
			}
		})
	}
}

func TestContinuationHeadHoldHonorsLeaseStopAndCancellation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"lease lost", "stop", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newPipeline(t, "approve")
			d := continuationDescriptor(t, f, strings.Repeat("a", 40))
			f.Job.Raw = encode(d)
			before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Epoch: 3, Stage: "implement", Branch: f.Branch, Runs: 5, WeightedUsage: 29, Head: d.HeadSHA, Result: implementationFixture()}
			if err := f.Executor.Store.Save(before); err != nil {
				t.Fatal(err)
			}
			marker := continuationFixedMarker(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lost := errors.New("fixture lease lost")
			fences := 0
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error {
				fences++
				if mode == "stop" {
					active := f.Executor.controls.get(1)
					active.mu.Lock()
					active.reason = "stop"
					active.cancel()
					active.mu.Unlock()
					return context.Canceled
				}
				if mode == "cancel" {
					cancel()
					return nil
				}
				return lost
			})
			if fences != 1 || len(f.Calls) != 0 {
				t.Fatalf("boundary bypassed: fences=%d calls=%v", fences, f.Calls)
			}
			if mode == "stop" {
				if err != nil || state.Status != "stopped" {
					t.Fatal(state, err)
				}
			} else if mode == "lease lost" && !errors.Is(err, lost) || mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			r, err := f.Executor.Store.Load(1)
			if err != nil || r.Runs != before.Runs || r.WeightedUsage != before.WeightedUsage || !reflect.DeepEqual(r.Result, before.Result) || mode != "stop" && hasJSON(r.FinalState) {
				t.Fatalf("unsafe save after ownership loss: %+v %v", r, err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("test started despite cancellation")
			}
		})
	}
}

func TestContinuationHeadChecksBeforeReportCompletionCall(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	d := continuationDescriptor(t, f, strings.Repeat("a", 40))
	f.Job.Raw = encode(d)
	calls := 0
	f.Executor.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		calls++
		return providers.Answer{}, errors.New("must not start report completion")
	}
	r := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "implement_report", Head: d.HeadSHA, Runs: 3, ReportCompletionRuns: 2, Result: encode(map[string]string{"status": "completed"}), ReportCompletion: &journal.ReportCompletion{ID: "pending-report", Original: implementationFixture()}}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	marker := continuationFixedMarker(t, f)
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || calls != 0 {
		t.Fatal(state, err, calls)
	}
	assertContinuationBlocked(t, f, state, marker, "commit_unavailable", r)
}

func TestContinuationHeadTargetGuardsAndFailedCheckAfterFF(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"dirty", "diverged", "missing after FF"} {
		t.Run(mode, func(t *testing.T) {
			f := newPipeline(t, "approve")
			w := continuationWorkspace(t, f)
			base := f.git(w.Tree, "rev-parse", "HEAD")
			published := continuationCommit(t, f, w.Tree, "feature.txt", "accepted feature\n")
			if err := w.Push(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.git(w.Tree, "reset", "--hard", base)
			wantCause, head := "target_dirty", published
			if mode == "dirty" {
				if err := os.WriteFile(filepath.Join(w.Tree, "local.txt"), []byte("keep\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if mode == "diverged" {
				continuationCommit(t, f, w.Tree, "local.txt", "local branch\n")
				wantCause = "target_diverged"
			} else {
				head, wantCause = strings.Repeat("a", 40), "commit_unavailable"
			}
			d := continuationDescriptor(t, f, head)
			f.Job.Raw = encode(d)
			r := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "review", Provider: "codex", Head: base, Runs: 9, ReportCompletionRuns: 2, OpenRun: 9, InterruptedRuns: 2, WeightedUsage: 37, Result: implementationFixture(), Review: encode(map[string]string{"decision": "approve", "summary": "old approval"}), Test: encode(TestResult{Passed: true})}
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			marker := continuationFixedMarker(t, f)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if mode == "missing after FF" {
				// Approval/test are invalid for the followed tree, while the report
				// and old review remain evidence and budget accounting is untouched.
				r.Test, r.Review = nil, nil
			}
			saved := assertContinuationBlocked(t, f, state, marker, wantCause, r)
			if mode == "missing after FF" && (!strings.Contains(string(saved.PreviousReview), "old approval") || f.git(w.Tree, "rev-parse", "HEAD") != published) {
				t.Fatal("FF evidence lost or the followed tree was not checked")
			}
			if mode == "dirty" {
				data, err := os.ReadFile(filepath.Join(w.Tree, "local.txt"))
				if err != nil || string(data) != "keep\n" || f.git(w.Tree, "rev-parse", "HEAD") != base {
					t.Fatal("dirty worktree changed")
				}
			}
		})
	}
}

func TestContinuationHeadRecoveryResumesReceivedReport(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	w := continuationWorkspace(t, f)
	head := continuationCommit(t, f, w.Tree, "feature.txt", "accepted feature\n")
	base := f.git(w.Tree, "rev-parse", "HEAD^")
	f.git(w.Tree, "reset", "--hard", base)
	d := continuationDescriptor(t, f, head)
	f.Job.Raw = encode(d)
	r := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "implement_report", Provider: "codex", Head: head, Runs: 1, Result: implementationFixture(), ReportCompletion: &journal.ReportCompletion{ID: "received", Original: implementationFixture()}}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	marker := continuationFixedMarker(t, f)
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	r = assertContinuationBlocked(t, f, state, marker, "not_included", r)
	// Intake acknowledges the hold; the original PC then has the requested tree.
	r.FinalReported = true
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	f.git(w.Tree, "reset", "--hard", head)
	state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || len(f.Calls) != 1 || f.Calls[0] != "claude" {
		t.Fatalf("received implementation was restarted: %+v %v calls=%v", state, err, f.Calls)
	}
	if saved, err := f.Executor.Store.Load(1); err != nil || saved.Runs != 1 || saved.ReportCompletionRuns != 0 {
		t.Fatalf("recovery consumed an implementation: %+v %v", saved, err)
	}
}

func TestContinuationDiagnosticIdentifiesMissingHeadInSavedAndReturnedResult(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"descriptor", "journal"} {
		t.Run(source, func(t *testing.T) {
			f := newPipeline(t, "approve")
			marker := continuationFixedMarker(t, f)
			head := strings.Repeat("a", 40)
			d := continuationDescriptor(t, f, "")
			before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "implement"}
			if source == "descriptor" {
				d.HeadSHA = head
			} else {
				before.Head = head
			}
			f.Job.Raw = encode(d)
			if err := f.Executor.Store.Save(before); err != nil {
				t.Fatal(err)
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if state.Status != "held" || len(f.Calls) != 0 {
				t.Fatalf("unexpected result: %+v", state)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fixed test started: %v", err)
			}
			var view struct {
				Head struct {
					Source  string `json:"source"`
					Checked string `json:"checkedHead"`
					Cause   string `json:"cause"`
				} `json:"continuationHead"`
			}
			if err := json.Unmarshal(state.View, &view); err != nil || view.Head.Source != source || view.Head.Checked != head || view.Head.Cause != "commit_unavailable" {
				t.Fatalf("diagnostic: %s, %v", state.View, err)
			}
			var result struct {
				Summary string `json:"summary"`
			}
			if err := json.Unmarshal(state.Result, &result); err != nil || !strings.Contains(result.Summary, head) || !strings.Contains(result.Summary, source) || !strings.Contains(result.Summary, "再開条件") {
				t.Fatalf("result: %s, %v", state.Result, err)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			var final intake.State
			if err := json.Unmarshal(saved.FinalState, &final); err != nil || !reflect.DeepEqual(final.View, state.View) || !reflect.DeepEqual(final.Result, state.Result) {
				t.Fatalf("saved state differs: %v", err)
			}
		})
	}
}

func TestContinuationOldRemoteDoesNotReplaceRequiredHead(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	source := continuationWorkspace(t, f)
	if err := source.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	head := continuationCommit(t, f, source.Tree, "later.txt", "unpublished work\n")
	d := continuationDescriptor(t, f, head)
	f.Job.Raw = encode(d)
	runtime := t.TempDir()
	store, err := journal.Open(runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.Executor.Store, f.Executor.Config.RuntimeDir = store, runtime
	marker := continuationFixedMarker(t, f)
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	r := assertContinuationBlocked(t, f, state, marker, "commit_unavailable", &journal.Record{Head: head})
	var view struct {
		Head struct {
			Source  string `json:"source"`
			Checked string `json:"checkedHead"`
		} `json:"continuationHead"`
	}
	if err := json.Unmarshal(state.View, &view); err != nil || view.Head.Source != "descriptor" || view.Head.Checked != head {
		t.Fatalf("diagnostic: %s: %v", state.View, err)
	}
	r.FinalReported = true
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := source.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatalf("resume: %+v %v", state, err)
	}
}

func TestContinuationInvalidJournalHeadIsNotReported(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	d := continuationDescriptor(t, f, "")
	f.Job.Raw = encode(d)
	invalid := "invalid-secret-head"
	before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "implement", Head: invalid}
	if err := f.Executor.Store.Save(before); err != nil {
		t.Fatal(err)
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encode(state)), invalid) || strings.Contains(string(saved.FinalState), invalid) || saved.Head != invalid {
		t.Fatalf("invalid head exposed or evidence lost: %s", saved.FinalState)
	}
}

func TestContinuationRemoteReferenceFailureReportsMissingHead(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	old, prior := priorJobFixture(t, f)
	d := continuationDescriptor(t, f, "")
	d.Generation = 1
	f.Job.Raw = encode(d)
	if err := f.Executor.Store.Save(prior); err != nil {
		t.Fatal(err)
	}
	continuationGitFailure(t, f, "refs/remotes/origin/"+old.Branch+"^{commit}")
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	var view struct {
		Head struct {
			Source  string `json:"source"`
			Checked string `json:"checkedHead"`
			Cause   string `json:"cause"`
		} `json:"continuationHead"`
	}
	if err := json.Unmarshal(state.View, &view); err != nil || view.Head.Source != "remote" || view.Head.Checked != "" || view.Head.Cause != "source_branch_unavailable" {
		t.Fatalf("diagnostic: %s %v", state.View, err)
	}
	if strings.Contains(string(state.Result), "untrusted-git-output") || !strings.Contains(string(state.Result), "未確認") {
		t.Fatalf("unsafe result: %s", state.Result)
	}
}

func TestResumeUncommittedHoldContract(t *testing.T) {
	t.Run("recheck and resume", resumeUncommittedRecheck)
	t.Run("status failure", resumeUncommittedStatusFailure)
	t.Run("interrupted hold", resumeUncommittedInterrupted)
	t.Run("non-behind dirty", resumeUncommittedNonBehind)
	t.Run("follow ordering and reopen", resumeUncommittedFollowOrder)
	t.Run("published PR source", resumeUncommittedPublishedSource)
	for _, required := range []bool{false, true} {
		for _, name := range []string{"README.md", "untracked.txt"} {
			t.Run(fmt.Sprintf("required=%v/%s", required, name), func(t *testing.T) {
				f := newPipeline(t, "approve")
				w := continuationWorkspace(t, f)
				head := f.git(w.Tree, "rev-parse", "HEAD")
				source := filepath.Join(f.root, "source")
				f.git(source, "checkout", "-b", f.Branch)
				remote := continuationCommit(t, f, source, "remote.txt", "remote\n")
				f.git(source, "push", f.bare, f.Branch)
				if err := os.WriteFile(filepath.Join(w.Tree, name), []byte("local\n"), 0600); err != nil {
					t.Fatal(err)
				}
				r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Branch: f.Branch, Stage: "implement", Provider: "codex"}
				if required {
					r.Head = head
				}
				if err := f.Executor.Store.Save(r); err != nil {
					t.Fatal(err)
				}
				marker := continuationFixedMarker(t, f)
				fenced := 0
				state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { fenced++; return nil })
				if err != nil || state.Status != "held" || state.HoldReason != "review_held" || fenced == 0 {
					t.Fatalf("state=%+v fence=%d err=%v", state, fenced, err)
				}
				var view map[string]any
				if err := json.Unmarshal(state.View, &view); err != nil {
					t.Fatal(err)
				}
				if view["releasePending"] != true || view["targetBranchRelation"] != "behind" || !strings.Contains(string(state.Result), errTargetBranchDirty.Error()) {
					t.Fatalf("state=%+v", state)
				}
				if required {
					h, ok := view["continuationHead"].(map[string]any)
					if !ok || h["cause"] != "target_dirty" || h["source"] != "remote" || h["checkedHead"] != remote || !strings.Contains(string(state.Result), "不足内容=") || !strings.Contains(string(state.Result), "再開条件=") {
						t.Fatalf("view=%s", state.View)
					}
				}
				b, err := os.ReadFile(filepath.Join(w.Tree, name))
				if err != nil || string(b) != "local\n" || f.git(w.Tree, "rev-parse", "HEAD") != head || len(f.Calls) != 0 {
					t.Fatalf("content=%q calls=%v err=%v", b, f.Calls, err)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("fixed test started: %v", err)
				}
				saved, err := f.Executor.Store.Load(1)
				if err != nil || !strings.Contains(string(saved.FinalState), errTargetBranchDirty.Error()) {
					t.Fatalf("saved=%+v err=%v", saved, err)
				}
			})
		}
	}
}

// resumeBehindFixture leaves the local tree at an ancestor of the published branch.
func resumeBehindFixture(t *testing.T, required bool) (*pipelineFixture, *gitwork.Workspace, string, string) {
	t.Helper()
	f := newPipeline(t, "approve")
	w := continuationWorkspace(t, f)
	head := f.git(w.Tree, "rev-parse", "HEAD")
	source := filepath.Join(f.root, "source")
	f.git(source, "checkout", "-b", f.Branch)
	remote := continuationCommit(t, f, source, "feature.txt", "accepted feature\n")
	f.git(source, "push", f.bare, f.Branch)
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Branch: f.Branch, Stage: "review", Provider: "codex", Result: implementationFixture(), Review: encode(map[string]any{"decision": "approve", "summary": "old tree approval"}), Test: encode(TestResult{Passed: true})}
	if required {
		r.Head = head
	} else {
		r.Stage = "implement"
	}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	return f, w, head, remote
}

// The Git wrapper affects only the isolated fixture and changes its real tree
// immediately before the second status call, or fails the first status call.
func resumeStatusInjection(t *testing.T, f *pipelineFixture, mode string) {
	t.Helper()
	script := filepath.Join(f.root, "status-injection")
	counter := filepath.Join(f.root, "status-count")
	action := fmt.Sprintf("if [ \"$n\" = 2 ]; then cp '%s' '%s'; printf 'racing change\\n' > racing.txt; fi", filepath.Join(f.Executor.Store.Root, "state", "job-1.json"), filepath.Join(f.root, "before-recheck.json"))
	if mode == "failure" {
		action = "if [ \"$n\" = 1 ]; then echo untrusted-git-output >&2; exit 128; fi"
	}
	body := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n *' status --porcelain -z --untracked-files=all '*)\n n=0; [ ! -f '%s' ] || n=$(cat '%s'); n=$((n+1)); echo $n > '%s' || exit 1\n %s;;\nesac\nexec /usr/bin/git \"$@\"\n", counter, counter, counter, action)
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Git = script
}

func resumeUncommittedRecheck(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(fmt.Sprintf("required=%v", required), func(t *testing.T) {
			f, w, head, remote := resumeBehindFixture(t, required)
			marker := continuationFixedMarker(t, f)
			resumeStatusInjection(t, f, "dirty")
			fences := 0
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { fences++; return nil })
			if err != nil || state.Status != "held" || state.HoldReason != "review_held" || fences < 2 || !strings.Contains(string(state.Result), errTargetBranchDirty.Error()) {
				t.Fatalf("state=%+v fences=%d err=%v", state, fences, err)
			}
			var view map[string]any
			if err := json.Unmarshal(state.View, &view); err != nil || view["releasePending"] != true || view["targetBranchRelation"] != "behind" {
				t.Fatalf("view=%s err=%v", state.View, err)
			}
			if required {
				h, ok := view["continuationHead"].(map[string]any)
				if !ok || h["cause"] != "target_dirty" || h["source"] != "remote" || h["checkedHead"] != remote {
					t.Fatalf("view=%s", state.View)
				}
			}
			content, err := os.ReadFile(filepath.Join(w.Tree, "racing.txt"))
			if err != nil || string(content) != "racing change\n" || f.git(w.Tree, "rev-parse", "HEAD") != head || len(f.Calls) != 0 {
				t.Fatalf("content=%q calls=%v err=%v", content, f.Calls, err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fixed test started: %v", err)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil || hasJSON(saved.Test) || hasJSON(saved.Review) || !strings.Contains(string(saved.PreviousReview), "old tree approval") || !hasJSON(saved.PreviousResult) {
				t.Fatalf("saved=%+v err=%v", saved, err)
			}
			checkpoint, err := os.ReadFile(filepath.Join(f.root, "before-recheck.json"))
			var before journal.Record
			if err != nil || json.Unmarshal(checkpoint, &before) != nil || hasJSON(before.Test) || hasJSON(before.Review) || !hasJSON(before.PreviousResult) || !strings.Contains(string(before.PreviousReview), "old tree approval") {
				t.Fatalf("invalidations not saved before recheck: %s err=%v", checkpoint, err)
			}
			// Acknowledge the hold and remove the racing file. Resume must follow
			// the published commit and run a new fixed test and review.
			if err := os.Remove(filepath.Join(w.Tree, "racing.txt")); err != nil {
				t.Fatal(err)
			}
			saved.FinalReported = true
			if err := f.Executor.Store.Save(saved); err != nil {
				t.Fatal(err)
			}
			f.Executor.Config.Git = "/usr/bin/git"
			state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || f.git(w.Tree, "rev-parse", "HEAD") != remote || required && (len(f.Calls) != 1 || f.Calls[0] != "claude") || !required && len(f.Calls) == 0 {
				t.Fatalf("resume=%+v calls=%v err=%v", state, f.Calls, err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("fixed test did not rerun: %v", err)
			}
		})
	}
}

func resumeUncommittedStatusFailure(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(fmt.Sprintf("required=%v", required), func(t *testing.T) {
			f, w, head, _ := resumeBehindFixture(t, required)
			resumeStatusInjection(t, f, "failure")
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if required {
				if err != nil || state.Status != "held" || !strings.Contains(string(state.View), "status_unavailable") {
					t.Fatalf("state=%+v err=%v", state, err)
				}
			} else if err == nil {
				t.Fatalf("status failure became hold: %+v", state)
			}
			if strings.Contains(string(state.Result), errTargetBranchDirty.Error()) || strings.Contains(string(state.Result), errTargetBranchDiverged.Error()) || strings.Contains(string(state.Result), "untrusted-git-output") || f.git(w.Tree, "rev-parse", "HEAD") != head || len(f.Calls) != 0 {
				t.Fatalf("state=%+v calls=%v", state, f.Calls)
			}
		})
	}
}

func resumeUncommittedInterrupted(t *testing.T) {
	for _, required := range []bool{false, true} {
		for _, mode := range []string{"cancel", "deadline", "lease lost"} {
			t.Run(fmt.Sprintf("required=%v/%s", required, mode), func(t *testing.T) {
				f, w, head, _ := resumeBehindFixture(t, required)
				if err := os.WriteFile(filepath.Join(w.Tree, "dirty.txt"), []byte("keep\n"), 0600); err != nil {
					t.Fatal(err)
				}
				var ctx context.Context
				var cancel context.CancelFunc
				if mode == "deadline" {
					ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
				} else {
					ctx, cancel = context.WithCancel(context.Background())
				}
				defer cancel()
				lost := &intake.APIError{Status: 409, Code: "lease_lost"}
				fenced := false
				_, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error {
					fenced = true
					switch mode {
					case "cancel":
						cancel()
						return nil
					case "deadline":
						<-ctx.Done()
						return nil
					default:
						return lost
					}
				})
				if !fenced || err == nil {
					t.Fatalf("fenced=%v err=%v", fenced, err)
				}
				if mode == "cancel" && !errors.Is(err, context.Canceled) || mode == "deadline" && !errors.Is(err, context.DeadlineExceeded) || mode == "lease lost" && !errors.Is(err, lost) {
					t.Fatalf("wrong interruption: %v", err)
				}
				saved, loadErr := f.Executor.Store.Load(1)
				data, readErr := os.ReadFile(filepath.Join(w.Tree, "dirty.txt"))
				if loadErr != nil || strings.Contains(string(saved.FinalState), errTargetBranchDirty.Error()) || readErr != nil || string(data) != "keep\n" || f.git(w.Tree, "rev-parse", "HEAD") != head || len(f.Calls) != 0 {
					t.Fatalf("saved=%+v data=%q calls=%v err=%v", saved, data, f.Calls, loadErr)
				}
			})
		}
	}
}

func resumeUncommittedNonBehind(t *testing.T) {
	for _, relation := range []string{"first", "same", "ahead"} {
		t.Run(relation, func(t *testing.T) {
			f := newPipeline(t, "approve")
			w := continuationWorkspace(t, f)
			if relation != "first" {
				if err := w.Push(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if relation == "ahead" {
				continuationCommit(t, f, w.Tree, "local.txt", "ahead\n")
			}
			if err := os.WriteFile(filepath.Join(w.Tree, "dirty.txt"), []byte("dirty\n"), 0600); err != nil {
				t.Fatal(err)
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if strings.Contains(string(state.Result), errTargetBranchDirty.Error()) {
				t.Fatalf("%s: state=%+v err=%v", relation, state, err)
			}
			// The ordinary pipeline must have proceeded beyond the branch guard.
			if len(f.Calls) == 0 {
				t.Fatalf("%s: no AI call, state=%+v err=%v", relation, state, err)
			}
		})
	}
}

func resumeUncommittedFollowOrder(t *testing.T) {
	f, w, _, remote := resumeBehindFixture(t, true)
	marker := continuationFixedMarker(t, f)
	fenceMarker := filepath.Join(f.root, "ownership-confirmed")
	snapshot := filepath.Join(f.root, "before-merge.json")
	script := filepath.Join(f.root, "observe-follow")
	body := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n *' merge --ff-only refs/remotes/origin/'*)\n  [ -f '%s' ] || exit 90\n  cp '%s' '%s' || exit 91\n  /usr/bin/git \"$@\" || exit $?\n  printf '[submodule]\\n' > .gitmodules\n  exit 0;;\nesac\nexec /usr/bin/git \"$@\"\n", fenceMarker, filepath.Join(f.Executor.Store.Root, "state", "job-1.json"), snapshot)
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Git = script
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return os.WriteFile(fenceMarker, []byte("owned"), 0600) })
	if err != nil || state.Status != "held" || !strings.Contains(string(state.View), "workspace_unavailable") || f.git(w.Tree, "rev-parse", "HEAD") != remote || len(f.Calls) != 0 {
		t.Fatalf("state=%+v calls=%v err=%v", state, f.Calls, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixed test started before reopen: %v", err)
	}
	data, err := os.ReadFile(snapshot)
	var before journal.Record
	if err != nil || json.Unmarshal(data, &before) != nil || hasJSON(before.Test) || hasJSON(before.Review) || !hasJSON(before.PreviousResult) || !strings.Contains(string(before.PreviousReview), "old tree approval") {
		t.Fatalf("invalidations not saved before merge: %s err=%v", data, err)
	}
}

func resumeUncommittedPublishedSource(t *testing.T) {
	f, w, head, remote := resumeBehindFixture(t, true)
	f.pull, f.head = true, remote
	r, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	r.PR = 1
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Tree, "pending.txt"), []byte("keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := continuationFixedMarker(t, f)
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	var view map[string]any
	if err != nil || state.Status != "held" || state.HoldReason != "review_held" || json.Unmarshal(state.View, &view) != nil || view["releasePending"] != true || view["targetBranchRelation"] != "behind" || !strings.Contains(string(state.Result), errTargetBranchDirty.Error()) {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	h, ok := view["continuationHead"].(map[string]any)
	if !ok || h["cause"] != "target_dirty" || h["source"] != "remote" || h["checkedHead"] != remote {
		t.Fatalf("view=%s", state.View)
	}
	data, err := os.ReadFile(filepath.Join(w.Tree, "pending.txt"))
	if err != nil || string(data) != "keep\n" || f.git(w.Tree, "rev-parse", "HEAD") != head || len(f.Calls) != 0 {
		t.Fatalf("content=%q calls=%v err=%v", data, f.Calls, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixed test started: %v", err)
	}
}
