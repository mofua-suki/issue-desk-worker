package execution

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

// Make the prior published branch explicit in tests that resume on another PC.
func publishContinuationFixture(t *testing.T, f *pipelineFixture) string {
	t.Helper()
	w := continuationWorkspace(t, f)
	if err := w.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	head, err := w.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func TestPreCommitResumeWithoutRemoteBranchIsHeld(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	d := continuationDescriptor(t, f, "")
	d.Generation = 1
	f.Job.Raw = encode(d)
	before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "stopped"}
	if err := f.Executor.Store.Save(before); err != nil {
		t.Fatal(err)
	}
	marker := continuationFixedMarker(t, f)
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	saved := assertContinuationBlocked(t, f, state, marker, "commit_unavailable", before)
	if saved.Generation != 0 {
		t.Fatalf("generation changed: %+v", saved)
	}
}

// Exercise the shipped pipeline with an actual fetched branch and a stopped
// journal, including a stale local worktree that cannot safely fast-forward.
func TestStoppedGenerationChecksPublishedContinuation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"new pc", "stale clean tree", "stale local branch", "stale dirty tree", "missing evidence"} {
		t.Run(mode, func(t *testing.T) {
			f := newPipeline(t, "approve")
			w := continuationWorkspace(t, f)
			base := f.git(w.Tree, "rev-parse", "HEAD")
			head := continuationCommit(t, f, w.Tree, "prior.txt", "previous work\n")
			if mode != "missing evidence" {
				if err := w.Push(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			f.git(w.Tree, "reset", "--hard", base)
			if mode == "stale local branch" {
				f.git(f.root, "--git-dir", w.Mirror, "worktree", "remove", w.Tree)
			}
			if mode == "new pc" {
				// Remove the registered tree, leaving a fresh worker runtime and mirror.
				runtime := t.TempDir()
				store, err := journal.Open(runtime)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				f.Executor.Store, f.Executor.Config.RuntimeDir = store, runtime
			}
			if mode == "stale dirty tree" {
				if err := os.WriteFile(filepath.Join(w.Tree, "local.txt"), []byte("uncommitted"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			d := continuationDescriptor(t, f, "")
			d.Generation = 1
			if mode == "stale dirty tree" {
				d.HeadSHA = base
			}
			f.Job.Raw = encode(d)
			before := &journal.Record{ID: 1, Repo: d.Repo, Issue: 7, Branch: f.Branch, Stage: "stopped", Runs: 3}
			if err := f.Executor.Store.Save(before); err != nil {
				t.Fatal(err)
			}
			marker := continuationFixedMarker(t, f)
			originalCall := f.Executor.Call
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				tree := filepath.Join(f.Executor.Config.RuntimeDir, "workspaces", "owner__project", "job-1")
				if f.git(tree, "merge-base", "--is-ancestor", head, "HEAD") != "" {
					t.Fatal("previous head absent at AI start")
				}
				data, err := os.ReadFile(filepath.Join(tree, "prior.txt"))
				if err != nil || string(data) != "previous work\n" {
					t.Fatalf("previous work absent at AI start: %q %v", data, err)
				}
				return originalCall(ctx, req)
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if mode == "stale dirty tree" || mode == "missing evidence" {
				cause := "target_dirty"
				if mode == "missing evidence" {
					cause = "commit_unavailable"
				}
				if d.HeadSHA != "" {
					before.Head = d.HeadSHA
				}
				saved := assertContinuationBlocked(t, f, state, marker, cause, before)
				if saved.Generation != 0 {
					t.Fatalf("generation changed: %+v", saved)
				}
				return
			}
			if state.Status != "merged" || len(f.Calls) == 0 {
				t.Fatalf("state=%+v calls=%v", state, f.Calls)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			if f.git(saved.Tree, "merge-base", "--is-ancestor", head, "HEAD") != "" {
				t.Fatal("previous head absent")
			}
			data, err := os.ReadFile(filepath.Join(saved.Tree, "prior.txt"))
			if err != nil || string(data) != "previous work\n" {
				t.Fatalf("previous work absent: %q %v", data, err)
			}
		})
	}
}
