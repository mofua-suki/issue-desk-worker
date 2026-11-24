package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestFixedTestStageTimings(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "workspaces"), 0700); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(dir, "tree")
	if err := os.Mkdir(tree, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"package.json": `{"devDependencies":{"prettier":"1"}}`, "go.mod": "module example.test\n"} {
		if err := os.WriteFile(filepath.Join(tree, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sandbox := filepath.Join(dir, "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	pnpm := filepath.Join(dir, "pnpm")
	if err := os.WriteFile(pnpm, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Config: Configuration{Host: "pc-one", Fleet: Fleet{RuntimeDir: dir, Codex: sandbox, PNPM: pnpm, TestTimeoutSeconds: 10}}}
	r := &journal.Record{Tree: tree, Result: json.RawMessage(`{}`)}
	r.StartStageTiming(1, 1)
	repo := Repository{SetupCommand: []string{"/bin/true"}, TestCommand: []string{"/bin/false"}}
	if err := e.setup(context.Background(), repo, r); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fixedTest(context.Background(), repo, r); err != nil {
		t.Fatal(err)
	}
	repo.TestCommand = []string{"/bin/true"}
	if _, err := e.fixedTest(context.Background(), repo, r); err != nil {
		t.Fatal(err)
	}
	for _, x := range r.Timing.TestSteps {
		if x.WorkerID != "pc-one" || x.Attempt < 1 {
			t.Fatalf("identity: %+v", x)
		}
		if x.Status == "success" || x.Status == "failed" {
			if x.ElapsedMillis == nil {
				t.Fatalf("missing elapsed: %+v", x)
			}
		}
	}
	// Real go fmt must not leave an asynchronous telemetry writer behind.
	entries, err := os.ReadDir(filepath.Join(dir, "workspaces"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("sandbox cleanup: entries=%v err=%v", entries, err)
	}
	steps := r.Timing.TestSteps
	if len(steps) != 8 || steps[0].Status != "success" || steps[1].Status != "success" || steps[2].Status != "success" || steps[3].Status != "success" || steps[4].Status != "failed" || steps[7].Status != "success" || steps[7].Attempt != 2 {
		t.Fatalf("steps: %+v", steps)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.fixedTest(ctx, repo, r); err == nil {
		t.Fatal("cancelled test succeeded")
	}
	steps = r.Timing.TestSteps
	if steps[8].Status != "interrupted" || steps[9].Status != "not_run" || steps[8].ElapsedMillis != nil {
		t.Fatalf("cancel: %+v", steps[8:])
	}
	// A failed sandbox start is a missing observation, not a measured zero.
	e.Config.Codex = filepath.Join(dir, "missing")
	if _, err := e.fixedTest(context.Background(), repo, r); err == nil {
		t.Fatal("missing executable succeeded")
	}
	steps = r.Timing.TestSteps
	if steps[11].Status != "missing" || steps[11].ElapsedMillis != nil {
		t.Fatalf("missing: %+v", steps[11:])
	}
}

func TestAIRepairTimingRecorded(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	original := f.Executor.Call
	calls := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := original(ctx, req)
		if !req.Review {
			if calls == 1 {
				saved, loadErr := f.Executor.Store.Load(1)
				if loadErr != nil || saved.Timing == nil || saved.Timing.AIRepairMillis != nil {
					t.Fatalf("repair recorded before call: %v %+v", loadErr, saved)
				}
			}
			calls++
			if calls == 1 {
				if err := os.WriteFile(filepath.Join(req.Policy.Tree, "feature.txt"), []byte("incorrect\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				time.Sleep(20 * time.Millisecond)
			}
		}
		return answer, err
	}
	_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 || r.Timing == nil || r.Timing.AIRepairMillis == nil || *r.Timing.AIRepairMillis < 20 {
		t.Fatalf("calls=%d timing=%+v", calls, r.Timing)
	}
	if len(r.Timing.TestSteps) == 0 {
		t.Fatal("test timings missing")
	}
	r.StartStageTiming(r.Timing.Generation+1, r.Timing.Epoch)
	if r.Timing.AIRepairMillis != nil || len(r.Timing.TestSteps) != 0 {
		t.Fatal("generation did not reset")
	}
	ms := int64(0)
	r.Timing.AIRepairMillis = &ms
	r.Timing.TestSteps = []journal.TestStepTiming{{Status: "success", ElapsedMillis: &ms}}
	r.StartStageTiming(r.Timing.Generation, r.Timing.Epoch+1)
	if r.Timing.AIRepairMillis != nil || len(r.Timing.TestSteps) != 0 {
		t.Fatal("epoch did not reset")
	}
}

func TestTestStepHistoryBoundedAcrossReload(t *testing.T) {
	dir := t.TempDir()
	store, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tree := t.TempDir()
	sandbox := filepath.Join(dir, "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Store: store, Config: Configuration{Host: "pc-one", Fleet: Fleet{RuntimeDir: dir, Codex: sandbox, TestTimeoutSeconds: 10}}}
	r := &journal.Record{ID: 1, Tree: tree, Result: json.RawMessage(`{}`)}
	r.StartStageTiming(1, 1)
	repo := Repository{SetupCommand: []string{"/bin/true"}, TestCommand: []string{"/bin/true"}}
	if err := e.setup(context.Background(), repo, r); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 35; i++ {
		result, err := e.fixedTest(context.Background(), repo, r)
		if err != nil || !result.Passed {
			t.Fatalf("attempt %d: %+v %v", i+1, result, err)
		}
		if len(r.Timing.TestSteps) > maxTestSteps {
			t.Fatalf("unbounded: %d", len(r.Timing.TestSteps))
		}
	}
	r, err = store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Timing.TestSteps) != maxTestSteps {
		t.Fatalf("reloaded count: %d", len(r.Timing.TestSteps))
	}
	last := r.Timing.TestSteps[len(r.Timing.TestSteps)-1]
	if last.Step != "testCommand" || last.Attempt != 35 || last.Status != "success" || last.ElapsedMillis == nil {
		t.Fatalf("last: %+v", last)
	}
	if err := e.setup(context.Background(), repo, r); err != nil {
		t.Fatal(err)
	}
	if r.Timing.TestSteps[len(r.Timing.TestSteps)-2].Attempt != 2 {
		t.Fatal("setup attempt lost after eviction")
	}
	result, err := e.fixedTest(context.Background(), repo, r)
	if err != nil || !result.Passed {
		t.Fatalf("after reload: %+v %v", result, err)
	}
	r, err = store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	last = r.Timing.TestSteps[len(r.Timing.TestSteps)-1]
	if len(r.Timing.TestSteps) != maxTestSteps || last.Attempt != 36 || last.Status != "success" {
		t.Fatalf("after reload: %+v count=%d", last, len(r.Timing.TestSteps))
	}
}
