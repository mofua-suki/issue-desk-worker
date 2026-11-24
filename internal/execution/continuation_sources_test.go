package execution

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/jobspec"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

// The prior identity is journal data, deliberately unrelated to branch digits
// or ordering of job IDs. Git storage and HTTP are the shipped pipeline fixtures.
func priorJobFixture(t *testing.T, f *pipelineFixture) (*gitwork.Workspace, *journal.Record) {
	t.Helper()
	current := continuationWorkspace(t, f)
	old, err := gitwork.Open(context.Background(), current.Config, current.Repo, current.Default, "codex/prior-work", 2)
	if err != nil {
		t.Fatal(err)
	}
	head := continuationCommit(t, f, old.Tree, "prior.txt", "previous job artifact\n")
	if err := old.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	prior := &journal.Record{ID: 2, Repo: "owner/project", Issue: 7, Branch: old.Branch, Stage: "stopped", Head: head}
	return old, prior
}

func TestChangedJobContinuationIncludesPriorSourceAtAIStart(t *testing.T) {
	parallelCases := make(chan struct{}, 4)
	for _, mode := range []string{"resume", "next generation", "new journal", "recorded head", "recorded PR", "intake branch", "ancestor candidates", "current remote", "unpublished previous branch", "generation reset with new job"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			parallelCases <- struct{}{}
			defer func() { <-parallelCases }()
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			old, prior := priorJobFixture(t, f)
			head := prior.Head
			if mode == "current remote" {
				publishContinuationFixture(t, f)
			}
			d := continuationDescriptor(t, f, "")
			if mode != "resume" {
				d.Generation = 1
			}
			if mode == "generation reset with new job" {
				d.Generation, prior.Generation = 0, 3
			}
			if mode == "unpublished previous branch" {
				prior.Head = ""
				f.git(f.root, "--git-dir", f.bare, "update-ref", "-d", "refs/heads/"+old.Branch)
			}
			if mode == "recorded head" {
				prior.Branch = ""
			}
			if mode == "recorded PR" {
				prior.Branch, prior.Head, prior.PR = "", "", 1
				reads := 0
				f.PullOverride = func(p *gitea.Pull) {
					reads++
					if reads == 1 {
						p.Head.Ref, p.Head.SHA = old.Branch, head
					}
				}
			}
			if mode == "intake branch" {
				// Intake carries the previous branch; a saved current branch must not win.
				d.Branch = old.Branch
			} else if err := f.Executor.Store.Save(prior); err != nil {
				t.Fatal(err)
			}
			if mode == "ancestor candidates" {
				next := *prior
				next.ID, next.Branch = 3, ""
				next.Head = continuationCommit(t, f, old.Tree, "later.txt", "later work\n")
				if err := old.Push(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := f.Executor.Store.Save(&next); err != nil {
					t.Fatal(err)
				}
				head = next.Head
			}
			if mode != "new journal" {
				if err := f.Executor.Store.Save(&journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "stopped", Runs: 1}); err != nil {
					t.Fatal(err)
				}
			}
			f.Job.Raw = encode(d)
			original := f.Executor.Call
			observed := 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				observed++
				// The request's actual sandbox tree, not a checkpoint/notification.
				if f.git(req.Policy.Tree, "merge-base", "--is-ancestor", head, "HEAD") != "" {
					t.Fatal("prior head missing at AI start")
				}
				data, err := os.ReadFile(filepath.Join(req.Policy.Tree, "prior.txt"))
				if err != nil || string(data) != "previous job artifact\n" {
					t.Fatalf("artifact missing: %q %v", data, err)
				}
				return original(ctx, req)
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || observed == 0 {
				t.Fatalf("state=%+v err=%v observed=%d", state, err, observed)
			}
		})
	}
}

func TestChangedJobContinuationHoldsUnprovableSources(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, cause string }{
		{"no source or head", "source_missing"},
		{"only current remote", "source_missing"},
		{"current journal and current remote", "source_missing"},
		{"unrelated issue", "source_missing"},
		{"unrelated repo", "source_missing"},
		{"missing prior branch", "source_missing"},
		{"prior fetch failure", "workspace_unavailable"},
		{"prior reference failure", "source_branch_unavailable"},
		{"divergent candidates", "source_ambiguous"},
		{"divergent current", "not_included"},
		{"dirty current", "target_dirty"},
		{"PR unavailable", "source_pr_unavailable"},
		{"PR mismatch", "source_pr_mismatch"},
		{"journal unavailable", "source_records_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			old, prior := priorJobFixture(t, f)
			d := continuationDescriptor(t, f, "")
			d.Generation = 1
			before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "stopped", Runs: 5, OpenRun: 5, InterruptedRuns: 2, ReportCompletionRuns: 1, WeightedUsage: 17, Result: implementationFixture(), Test: encode(TestResult{Passed: true}), Review: encode(map[string]string{"decision": "approve"}), ReportCompletion: &journal.ReportCompletion{ID: "received", Original: implementationFixture(), Attempted: true}, Usage: []json.RawMessage{encode(map[string]int{"existing": 1})}}
			switch tc.name {
			case "current journal and current remote":
				publishContinuationFixture(t, f)
				prior = nil
				before.Stage, before.Generation = "implement", d.Generation
			case "only current remote":
				publishContinuationFixture(t, f)
				fallthrough
			case "no source or head":
				prior = nil
			case "unrelated issue":
				prior.Issue = 8
			case "unrelated repo":
				prior.Repo = "owner/other"
			case "missing prior branch":
				prior.Head, prior.Branch = "", "codex/missing"
			case "prior fetch failure":
				continuationGitFailure(t, f, " fetch --prune origin ")
			case "prior reference failure":
				continuationGitFailure(t, f, "refs/remotes/origin/"+old.Branch+"^{commit}")
			case "divergent candidates":
				base := f.git(old.Tree, "rev-parse", "HEAD^")
				f.git(old.Tree, "reset", "--hard", base)
				other := *prior
				other.ID, other.Branch = 3, ""
				other.Head = continuationCommit(t, f, old.Tree, "other.txt", "independent work\n")
				// Objects are already in the shared mirror, independent of remote refs.
				if err := f.Executor.Store.Save(&other); err != nil {
					t.Fatal(err)
				}
			case "divergent current":
				w := continuationWorkspace(t, f)
				continuationCommit(t, f, w.Tree, "independent.txt", "independent current\n")
			case "dirty current":
				w := continuationWorkspace(t, f)
				if err := os.WriteFile(filepath.Join(w.Tree, "pending.txt"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "PR unavailable":
				prior.PR = 1
				target, err := url.Parse(f.server.URL)
				if err != nil {
					t.Fatal(err)
				}
				proxy := httputil.NewSingleHostReverseProxy(target)
				unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path == "/api/v1/repos/owner/project/pulls/1" {
						w.WriteHeader(503)
						return
					}
					proxy.ServeHTTP(w, req)
				}))
				t.Cleanup(unavailable.Close)
				client, err := gitea.New(unavailable.URL, "fixture")
				if err != nil {
					t.Fatal(err)
				}
				f.Executor.Gitea = client
			case "PR mismatch":
				prior.PR = 1
				f.PullOverride = func(p *gitea.Pull) { p.Head.Repo.FullName = "other/project" }
			case "journal unavailable":
				if err := os.WriteFile(filepath.Join(f.Executor.Store.Root, "state", "job-999.json"), []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if prior != nil {
				if err := f.Executor.Store.Save(prior); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name != "only current remote" && tc.name != "no source or head" && tc.name != "unrelated issue" && tc.name != "unrelated repo" {
				if err := f.Executor.Store.Save(before); err != nil {
					t.Fatal(err)
				}
			} else {
				// Preserve supplied intake counts without manufacturing a prior journal.
				d.View["workerExecution"] = encode(map[string]any{"implementationRuns": 5, "openRun": 5, "interruptedRuns": 2, "reportCompletionRuns": 1, "weightedUsage": 17})
				before.Result, before.Test, before.Review, before.ReportCompletion, before.Usage = nil, nil, nil, nil, nil
			}
			f.Job.Raw = encode(d)
			marker := continuationFixedMarker(t, f)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			saved := assertContinuationBlocked(t, f, state, marker, tc.cause, before)
			if tc.name == "only current remote" {
				// Saving/acknowledging the first hold must not make its newly
				// generated current branch count as historical evidence.
				saved.FinalReported = true
				if err := f.Executor.Store.Save(saved); err != nil {
					t.Fatal(err)
				}
				state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				if err != nil {
					t.Fatal(err)
				}
				assertContinuationBlocked(t, f, state, marker, tc.cause, before)
			}

			if saved.Generation != before.Generation && tc.name != "only current remote" && tc.name != "no source or head" && tc.name != "unrelated issue" && tc.name != "unrelated repo" {
				t.Fatal("generation consumed on hold")
			}
		})
	}
}

func TestContinuationRetrySeparatesPriorJobFromRetryEvidence(t *testing.T) {
	for _, mode := range []string{"explicit reset", "wrong generation", "marker alone", "retry journal head"} {
		t.Run(mode, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			d := continuationDescriptor(t, f, "")
			d.Generation = 1
			d.View["retryFromBase"] = encode(map[string]int{"generation": 1})
			before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "implement", Runs: 1, WeightedUsage: 7}
			// An unavailable previous job is discardable only by the explicit reset.
			if err := f.Executor.Store.Save(&journal.Record{ID: 2, Repo: d.Repo, Issue: 7, Branch: "codex/unavailable", Stage: "stopped"}); err != nil {
				t.Fatal(err)
			}
			if mode == "wrong generation" {
				d.View["retryFromBase"] = encode(map[string]int{"generation": 0})
			}
			if mode == "marker alone" || mode == "retry journal head" {
				generation := 1
				before.RetryBaseGeneration = &generation
				if mode == "marker alone" {
					delete(d.View, "retryFromBase")
				} else {
					f.Branch = "codex/issue-7-job-1-retry-1"
					before.Branch = f.Branch
					w := continuationWorkspace(t, f)
					continuationCommit(t, f, w.Tree, "retry.txt", "retry work\n")
					before.Head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
					// The old descriptor is ignored, but the retry journal head still gates AI.
					d.HeadSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				}
			}
			if err := f.Executor.Store.Save(before); err != nil {
				t.Fatal(err)
			}
			f.Job.Raw = encode(d)
			marker := continuationFixedMarker(t, f)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if mode == "explicit reset" {
				if state.Status != "merged" || len(f.Calls) == 0 {
					t.Fatalf("authorized retry blocked: %+v calls=%v", state, f.Calls)
				}
				return
			}
			cause := "source_missing"
			if mode == "retry journal head" {
				// The old descriptor's head is not a requirement for this retry,
				// but an unavailable retry journal head still blocks the Call.
				var view struct {
					Head struct {
						Cause    string `json:"cause"`
						Required string `json:"requiredHead"`
					} `json:"continuationHead"`
				}
				if err := json.Unmarshal(state.View, &view); err != nil {
					t.Fatal(err)
				}
				saved, err := f.Executor.Store.Load(1)
				if err != nil || state.Status != "held" || state.HoldReason != "review_held" || len(f.Calls) != 0 || view.Head.Cause != "commit_unavailable" || view.Head.Required != "" || saved.Head != before.Head || saved.Runs != before.Runs || saved.WeightedUsage != before.WeightedUsage {
					t.Fatalf("retry evidence not protected: state=%+v saved=%+v err=%v", state, saved, err)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("fixed test started: %v", err)
				}
				return
			}
			assertContinuationBlocked(t, f, state, marker, cause, before)
		})
	}
}

func TestPriorSourceFollowHonorsLostLeaseBeforeMutation(t *testing.T) {
	f := newPipeline(t, "approve")
	_, prior := priorJobFixture(t, f)
	if err := f.Executor.Store.Save(prior); err != nil {
		t.Fatal(err)
	}
	d := continuationDescriptor(t, f, "")
	d.Generation = 1
	f.Job.Raw = encode(d)
	before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "stopped", Runs: 3, Result: implementationFixture(), Test: encode(TestResult{Passed: true})}
	if err := f.Executor.Store.Save(before); err != nil {
		t.Fatal(err)
	}
	w := continuationWorkspace(t, f)
	head := f.git(w.Tree, "rev-parse", "HEAD")
	lost := errors.New("fixture lease lost before source FF")
	_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return lost })
	if !errors.Is(err, lost) || len(f.Calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, f.Calls)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved.Runs != before.Runs || !continuationSameJSON(saved.Result, before.Result) || !continuationSameJSON(saved.Test, before.Test) || hasJSON(saved.FinalState) || f.git(w.Tree, "rev-parse", "HEAD") != head {
		t.Fatalf("mutated after lost lease: %+v err=%v", saved, err)
	}
}

func TestHeadlessSameGenerationResumeDoesNotBecomeInitialWork(t *testing.T) {
	for _, mode := range []string{"checkpoint", "stopped journal"} {
		t.Run(mode, func(t *testing.T) {
			f := newPipeline(t, "approve")
			d := continuationDescriptor(t, f, "")
			before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "stopped", Runs: 2, OpenRun: 2}
			cause := "source_missing"
			if mode == "checkpoint" {
				d.View["workerExecution"] = encode(map[string]int{"implementationRuns": 2, "openRun": 2})
			} else {
				cause = "commit_unavailable"
				if err := f.Executor.Store.Save(before); err != nil {
					t.Fatal(err)
				}
			}
			f.Job.Raw = encode(d)
			marker := continuationFixedMarker(t, f)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			assertContinuationBlocked(t, f, state, marker, cause, before)
		})
	}
}

func TestMissingSourceHoldCannotBecomePriorJobEvidence(t *testing.T) {
	f := newPipeline(t, "approve")
	publishContinuationFixture(t, f)
	d := continuationDescriptor(t, f, "")
	d.Generation = 1
	f.Job.Raw = encode(d)
	marker := continuationFixedMarker(t, f)
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	assertContinuationBlocked(t, f, state, marker, "source_missing", &journal.Record{})
	// A new descriptor identity must not legitimize the empty branch saved by
	// the previous hold, even though that branch exists in the remote repository.
	d.ID, f.Job.ID = 3, 3
	d.Branch = ""
	f.Job.Raw = encode(d)
	state, err = f.Executor.execute(context.Background(), f.Job, func(context.Context) error { return nil })
	saved, loadErr := f.Executor.Store.Load(3)
	var view struct {
		Head struct {
			Cause string `json:"cause"`
		} `json:"continuationHead"`
	}
	if json.Unmarshal(state.View, &view) != nil || err != nil || loadErr != nil || state.Status != "held" || view.Head.Cause != "source_missing" || len(f.Calls) != 0 || saved == nil || saved.Runs != 0 || saved.OpenRun != 0 {
		t.Fatalf("new job used hold as provenance: state=%+v record=%+v err=%v loadErr=%v", state, saved, err, loadErr)
	}
}

func TestSourceLookupNoFullReread(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := int64(1); i <= 500; i++ {
		if err := store.Save(&journal.Record{ID: i, Repo: "other/project", Issue: i}); err != nil {
			t.Fatal(err)
		}
	}
	e := &Executor{Store: store}
	d := Descriptor{Descriptor: jobspec.Descriptor{ID: 501, Repo: "owner/project", Issue: 7}}
	r := &journal.Record{ID: 501, Repo: d.Repo, Issue: d.Issue}
	run := func() []journal.ContinuationSource {
		discarded, err := e.retryDiscardedSources(d, r)
		if err != nil {
			t.Fatal(err)
		}
		workspace, state, err := e.openRequiredContinuationWorkspace(context.Background(), d, r, "", nil)
		if err != nil || workspace != nil || state != nil {
			t.Fatalf("unexpected continuation: %v %v %v", workspace, state, err)
		}
		return discarded
	}
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.InotifyAddWatch(fd, filepath.Join(store.Root, "state"), syscall.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		count := 0
		buf := make([]byte, 65536)
		for {
			n, err := syscall.Read(fd, buf)
			if err == syscall.EAGAIN {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			for off := 0; off < n; {
				size := int(binary.LittleEndian.Uint32(buf[off+12:]))
				name := strings.TrimRight(string(buf[off+16:off+16+size]), "\x00")
				if strings.HasPrefix(name, "job-") && strings.HasSuffix(name, ".json") {
					count++
				}
				off += 16 + size
			}
		}
		return count
	}
	first := run()
	if n := count(); n != 500 {
		t.Fatalf("initial opens: %d", n)
	}
	second := run()
	if n := count(); n != 0 {
		t.Fatalf("repeated opens: %d", n)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("discarded sources changed")
	}
}
