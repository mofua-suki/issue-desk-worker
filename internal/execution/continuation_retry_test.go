package execution

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestRetryAbandonmentSurvivesNextGeneration(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"old branch remains", "old branch missing", "old source updated", "new source"} {
		t.Run(mode, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			old, prior := priorJobFixture(t, f)
			prior.Head = "" // The abandoned branch may later disappear entirely.
			if err := f.Executor.Store.Save(prior); err != nil {
				t.Fatal(err)
			}
			d := continuationDescriptor(t, f, "")
			d.Generation = 1
			d.View["retryFromBase"] = encode(map[string]int{"generation": 1})
			f.Job.Raw = encode(d)
			var retryHead string
			observed := 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				observed++
				retryHead = continuationCommit(t, f, req.Policy.Tree, "retry.txt", "replacement artifact\n")
				f.git(req.Policy.Tree, "push", "origin", "HEAD:refs/heads/codex/issue-7-job-1-retry-1")
				return providers.Answer{}, errors.New("fixture stopped after retry artifact")
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "failed" || observed != 1 {
				t.Fatalf("retry did not run: state=%+v err=%v calls=%d", state, err, observed)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil || saved == nil || len(saved.DiscardedContinuationSources) != 1 {
				t.Fatalf("abandonment not persisted: %+v %v", saved, err)
			}
			if mode == "old branch missing" {
				f.git(f.root, "--git-dir", old.Mirror, "worktree", "remove", old.Tree)
				f.git(f.root, "--git-dir", old.Mirror, "update-ref", "-d", "refs/heads/"+old.Branch)
				f.git(f.root, "--git-dir", f.bare, "update-ref", "-d", "refs/heads/"+old.Branch)
			}
			if mode == "old source updated" || mode == "new source" {
				if mode == "old source updated" {
					prior.Generation++
				} else {
					prior.ID = 3
				}
				prior.Branch, prior.Head = "codex/new-unavailable", ""
				if err := f.Executor.Store.Save(prior); err != nil {
					t.Fatal(err)
				}
			}
			// Reload the real JSON journal through the next execution. No retry
			// instruction remains; its persisted scope is the only exclusion.
			d.Generation, d.Branch = 2, saved.Branch
			delete(d.View, "retryFromBase")
			f.Job.Raw = encode(d)
			f.Calls = nil
			observed = 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				observed++
				f.git(req.Policy.Tree, "merge-base", "--is-ancestor", retryHead, "HEAD")
				data, err := os.ReadFile(filepath.Join(req.Policy.Tree, "retry.txt"))
				if err != nil || string(data) != "replacement artifact\n" {
					t.Fatalf("retry artifact missing at AI start: %q %v", data, err)
				}
				return providers.Answer{}, errors.New("fixture stopped at verified AI start")
			}
			marker := continuationFixedMarker(t, f)
			state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if mode == "old source updated" || mode == "new source" {
				assertContinuationBlocked(t, f, state, marker, "source_missing", saved)
				if observed != 0 {
					t.Fatal("new provenance skipped by old abandonment")
				}
			} else if state.Status != "failed" || observed != 1 {
				t.Fatalf("retry continuation blocked: %+v calls=%d", state, observed)
			}
		})
	}
}

func TestRetryOwnPRIsRequiredBeforeAI(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"delivered PR", "same saved PR", "saved PR with old descriptor", "branch omitted", "PR unavailable", "PR head missing", "PR wrong branch"} {
		t.Run(mode, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			f.Branch = "codex/issue-7-job-1-retry-1"
			w := continuationWorkspace(t, f)
			base := f.git(w.Tree, "rev-parse", "HEAD")
			head := continuationCommit(t, f, w.Tree, "retry.txt", "PR retry artifact\n")
			// Retain the object but no local/remote branch at its tip. Only the
			// PR response can identify it; the current tree is deliberately stale.
			f.git(w.Tree, "reset", "--hard", base)
			generation := 1
			before := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Generation: 1, Branch: f.Branch, Stage: "stopped", RetryBaseGeneration: &generation, Runs: 1, WeightedUsage: 9}
			d := continuationDescriptor(t, f, "")
			d.Generation, d.PR = 1, 1
			d.View["retryFromBase"] = encode(map[string]int{"generation": 1})
			if mode == "same saved PR" || mode == "saved PR with old descriptor" {
				before.PR = 1
			}
			if mode == "saved PR with old descriptor" {
				d.Branch = "codex/abandoned"
			}
			if mode == "branch omitted" {
				d.Branch = ""
			}
			if err := f.Executor.Store.Save(before); err != nil {
				t.Fatal(err)
			}
			f.PullOverride = func(p *gitea.Pull) {
				p.Head.SHA = head
				if mode == "PR head missing" {
					p.Head.SHA = ""
				}
				if mode == "PR wrong branch" {
					p.Head.Ref = "codex/abandoned"
				}
			}
			if mode == "PR unavailable" {
				target, err := url.Parse(f.server.URL)
				if err != nil {
					t.Fatal(err)
				}
				proxy := httputil.NewSingleHostReverseProxy(target)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path == "/api/v1/repos/owner/project/pulls/1" {
						w.WriteHeader(503)
						return
					}
					proxy.ServeHTTP(w, req)
				}))
				t.Cleanup(server.Close)
				f.Executor.Gitea, err = gitea.New(server.URL, "fixture")
				if err != nil {
					t.Fatal(err)
				}
			}
			f.Job.Raw = encode(d)
			observed := 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				observed++
				f.git(req.Policy.Tree, "merge-base", "--is-ancestor", head, "HEAD")
				if _, err := os.Stat(filepath.Join(req.Policy.Tree, "retry.txt")); err != nil {
					t.Fatal(err)
				}
				return providers.Answer{}, errors.New("fixture stopped at verified AI start")
			}
			marker := continuationFixedMarker(t, f)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			cause := map[string]string{"PR unavailable": "source_pr_unavailable", "PR head missing": "invalid_sha", "PR wrong branch": "source_pr_mismatch"}[mode]
			if before.PR > 0 {
				// Published-branch protection independently requires its remote
				// reference. Even then the PR must be resolved and followed first.
				cause = "target_comparison_unavailable"
				f.git(w.Tree, "merge-base", "--is-ancestor", head, "HEAD")
			}
			if cause != "" {
				assertContinuationBlocked(t, f, state, marker, cause, before)
				if observed != 0 {
					t.Fatal("AI started without PR verification")
				}
			} else if state.Status != "failed" || observed != 1 || f.PullReads == 0 {
				t.Fatalf("PR not included at AI start: %+v calls=%d reads=%d", state, observed, f.PullReads)
			}
		})
	}
}
