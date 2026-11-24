package execution

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/jobspec"
	"os"
	"path/filepath"
	"testing"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

func TestRetryFromBasePipelineAndRestart(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(w.Tree, "old.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = w.Stage(ctx); err != nil {
		t.Fatal(err)
	}
	old, err := w.Commit(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Push(ctx); err != nil {
		t.Fatal(err)
	}
	f.pull = true
	f.merged = true
	f.head = old
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Generation: 0, Stage: "held", Branch: f.Branch, PR: 1, Head: old, Session: "old", ReportCompletion: &journal.ReportCompletion{ID: "old"}, ReportCompletionRuns: 2, UnansweredRuns: 97, ReviewHolds: 4, Runs: 100, WeightedUsage: 12, Usage: []json.RawMessage{json.RawMessage(`{"x":1}`)}, UsageSent: 1}
	if err = f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	var d Descriptor
	if err = json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.View = map[string]json.RawMessage{"retryFromBase": json.RawMessage(`{"generation":0}`), "reviewHolds": json.RawMessage(`4`)}
	f.Job.Raw = encode(d)
	state, err := f.Executor.execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if state.Status == "merged" {
		t.Fatal("old merged PR accepted")
	}
	r, err = f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if r.RetryBaseGeneration == nil || *r.RetryBaseGeneration != 0 || r.PR != 0 || r.Session != "" || r.ReportCompletion != nil || r.ReportCompletionRuns != 2 || r.ReviewHolds != 0 || r.Runs != 100 || r.WeightedUsage != 12 || len(r.Usage) != 1 || r.UsageSent != 1 {
		t.Fatalf("record: %+v", r)
	}
	if r.Branch == f.Branch || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != old {
		t.Fatal("old branch changed")
	}
	if f.git(r.Tree, "rev-parse", "HEAD") != f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/main") {
		t.Fatal("not based on default")
	}
	if _, err = os.Stat(filepath.Join(r.Tree, "old.txt")); !os.IsNotExist(err) {
		t.Fatal("old tree retained")
	}
	if err = os.WriteFile(filepath.Join(r.Tree, "resume.txt"), []byte("resume"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = f.Executor.Store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := journal.Open(f.Executor.Config.RuntimeDir)
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.Store = store
	t.Cleanup(func() { _ = store.Close() })
	_, err = f.Executor.execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(r.Tree, "resume.txt")); err != nil {
		t.Fatal("restart reset twice", err)
	}
	retry, err := gitwork.Open(ctx, c, "owner/project", "main", r.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = retry.Stage(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = retry.Commit(ctx, "retry"); err != nil {
		t.Fatal(err)
	}
	if err = retry.Push(ctx); err != nil {
		t.Fatal(err)
	}
	if f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != old {
		t.Fatal("published old branch changed")
	}
}

func TestRetryFromBaseGenerationValidation(t *testing.T) {
	for _, tc := range []struct {
		raw        string
		generation int
		want       bool
	}{{`{"generation":0}`, 0, true}, {`{"generation":1}`, 0, false}, {`{}`, 0, false}, {`{"generation":"0"}`, 0, false}, {`null`, 0, false}, {`{"generation":0.5}`, 0, false}} {
		d := Descriptor{Descriptor: jobspec.Descriptor{Generation: tc.generation, View: map[string]json.RawMessage{"retryFromBase": json.RawMessage(tc.raw)}}}
		if got := retryBaseNeeded(d, &journal.Record{}); got != tc.want {
			t.Errorf("%s: %v", tc.raw, got)
		}
	}
}

func TestRetryFromBaseResumesPublishedBranchOnAnotherWorker(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	branch := "codex/issue-7-job-1-retry-0"
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(w.Tree, "published.txt"), []byte("published"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = w.Stage(ctx); err != nil {
		t.Fatal(err)
	}
	head, err := w.Commit(ctx, "published retry")
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Push(ctx); err != nil {
		t.Fatal(err)
	}

	// A different worker has its own journal and mirror, but sees the same origin.
	runtime := t.TempDir()
	store, err := journal.Open(runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.Executor.Store = store
	f.Executor.Config.RuntimeDir = runtime
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "held", Branch: f.Branch, UnansweredRuns: 99, Runs: 100, WeightedUsage: 12, Usage: []json.RawMessage{json.RawMessage(`{"x":1}`)}, UsageSent: 1}
	if err = store.Save(r); err != nil {
		t.Fatal(err)
	}
	var d Descriptor
	if err = json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.View = map[string]json.RawMessage{"retryFromBase": json.RawMessage(`{"generation":0}`), "reviewHolds": json.RawMessage(`1`)}
	f.Job.Raw = encode(d)
	for i := 0; i < 2; i++ {
		if _, err = f.Executor.execute(ctx, f.Job, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
		r, err = store.Load(1)
		if err != nil {
			t.Fatal(err)
		}
		if r.Branch != branch || r.RetryBaseGeneration == nil || *r.RetryBaseGeneration != 0 || r.Runs != 100 || r.WeightedUsage != 12 || len(r.Usage) != 1 || r.UsageSent != 1 {
			t.Fatalf("record: %+v", r)
		}
		if r.ReviewHolds != 1 {
			t.Fatalf("review rounds restarted on another PC: %d", r.ReviewHolds)
		}
		if got := f.git(r.Tree, "rev-parse", "HEAD"); got != head {
			t.Fatalf("head = %s, want %s", got, head)
		}
		if _, err = os.Stat(filepath.Join(r.Tree, "published.txt")); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err = os.WriteFile(filepath.Join(r.Tree, "resume.txt"), []byte("resume"), 0600); err != nil {
				t.Fatal(err)
			}
		} else if _, err = os.Stat(filepath.Join(r.Tree, "resume.txt")); err != nil {
			t.Fatal("second run reset the tree", err)
		}
	}
	if got := f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+branch); got != head {
		t.Fatal("published branch changed")
	}
}

// The tree may be gone or half removed when the previous attempt crashed before saving.
func TestPrepareRetryConvergesAfterAnInterruptedRemoval(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(w.Tree, ".git")); err != nil {
		t.Fatal(err)
	}
	branch := "codex/issue-7-job-1-retry-0"
	for i := 0; i < 2; i++ {
		if err = gitwork.PrepareRetry(ctx, c, "owner/project", 1, branch); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if _, err = os.Lstat(w.Tree); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("attempt %d left the tree: %v", i, err)
		}
	}
	// Crash point: the tree is removed and the journal still lacks the generation.
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "held", Branch: f.Branch}
	if err = f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	var d Descriptor
	if err = json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.View = map[string]json.RawMessage{"retryFromBase": json.RawMessage(`{"generation":0}`)}
	f.Job.Raw = encode(d)
	if _, err = f.Executor.execute(ctx, f.Job, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if r, err = f.Executor.Store.Load(1); err != nil {
		t.Fatal(err)
	}
	if r.RetryBaseGeneration == nil || r.Branch != branch {
		t.Fatalf("record: %+v", r)
	}
}

func TestRetryFromBaseDoesNotResetAClosedIssue(t *testing.T) {
	f := newPipeline(t, "approve")
	f.closed = true
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "held", Branch: f.Branch, PR: 1, Head: "old"}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.View = map[string]json.RawMessage{"retryFromBase": json.RawMessage(`{"generation":0}`)}
	f.Job.Raw = encode(d)
	state, err := f.Executor.execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "stopped" {
		t.Fatalf("state: %+v", state)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RetryBaseGeneration != nil || saved.Branch != f.Branch || saved.PR != 1 {
		t.Fatalf("record was reset: %+v", saved)
	}
}
