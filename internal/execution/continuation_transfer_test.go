package execution

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

// Two independent worker runtimes exchange the branch only through the Git remote.
func TestContinuationTransferAfterMissingHeadHold(t *testing.T) {
	f := newPipeline(t, "approve")
	source := continuationWorkspace(t, f)
	head := continuationCommit(t, f, source.Tree, "prior.txt", "work from source worker\n")
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
	originalCall := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		r, err := store.Load(1)
		if err != nil || r == nil {
			t.Fatalf("destination journal: %v", err)
		}
		if f.git(r.Tree, "merge-base", "--is-ancestor", head, "HEAD") != "" {
			t.Fatal("source commit missing at execution")
		}
		data, err := os.ReadFile(filepath.Join(r.Tree, "prior.txt"))
		if err != nil || string(data) != "work from source worker\n" {
			t.Fatalf("source artifact missing at execution: %q %v", data, err)
		}
		return originalCall(ctx, req)
	}

	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	before := &journal.Record{Head: head}
	r := assertContinuationBlocked(t, f, state, marker, "commit_unavailable", before)
	// Intake has accepted the hold before the source publishes its branch.
	r.FinalReported = true
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := source.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || len(f.Calls) != 2 {
		t.Fatalf("destination did not resume: state=%+v err=%v calls=%v", state, err, f.Calls)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fixed test did not run after transfer: %v", err)
	}
}
