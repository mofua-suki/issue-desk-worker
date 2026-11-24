package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/jobspec"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"issue-desk-worker/internal/journal"
)

func TestFixedTestTimingPersistence(t *testing.T) {
	root := t.TempDir()
	store, err := journal.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tree := filepath.Join(root, "tree")
	if err := os.Mkdir(tree, 0700); err != nil {
		t.Fatal(err)
	}
	sandbox := filepath.Join(root, "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Store: store, Config: Configuration{Fleet: Fleet{RuntimeDir: root, Codex: sandbox, TestTimeoutSeconds: 10}}}
	r := &journal.Record{ID: 1, Generation: 1, Epoch: 3, Stage: "test", Tree: tree, Result: json.RawMessage(`{}`)}
	r.StartStageTiming(1, 3)
	for i, command := range []string{"/bin/false", "/bin/true"} {
		if i == 1 {
			for len(r.Timing.TestSteps) < maxTestSteps {
				r.Timing.TestSteps = append(r.Timing.TestSteps, journal.TestStepTiming{Step: "prettier", Attempt: 1})
			}
		}
		result, err := e.fixedTest(context.Background(), Repository{TestCommand: []string{command}}, r)
		if err != nil {
			t.Fatal(err)
		}
		saved, err := store.Load(1)
		if err != nil {
			t.Fatal(err)
		}
		got := saved.Timing.FixedTests[len(saved.Timing.FixedTests)-1]
		if got.Passed != result.Passed || got.ElapsedMS == nil || *got.ElapsedMS != result.ElapsedMS {
			t.Fatalf("result=%+v saved=%+v", result, got)
		}
	}
	if r.Timing.FixedTestCount != 2 || r.Timing.FixedTests[0].Passed || !r.Timing.FixedTests[1].Passed {
		t.Fatalf("timing=%+v", r.Timing)
	}
}

func TestWorkerExecutionStageTiming(t *testing.T) {
	r := &journal.Record{Generation: 2, Epoch: 3, Stage: "test"}
	r.StartStageTiming(2, 3)
	r.Timing.Since = time.Now().Add(-time.Second)
	r.Timing.Gaps = true
	ms := int64(0)
	r.Timing.AIRepairMillis = &ms
	view := workerExecutionView(r, "test")
	stages := view["stageTimingMillis"].(map[string]int64)
	if stages["test"] < 1000 || len(stages) != 1 || view["stageTimingGaps"] != true || view["aiRepairMillis"] != int64(0) {
		t.Fatalf("view=%v", view)
	}
	for _, mismatch := range []func(){func() { r.Generation++ }, func() { r.Generation--; r.Epoch++ }} {
		mismatch()
		v := workerExecutionView(r, "test")
		for _, key := range []string{"stageTimingMillis", "stageTimingGaps", "aiRepairMillis", "fixedTests"} {
			if _, ok := v[key]; ok {
				t.Fatalf("mismatched timing: %v", v)
			}
		}
	}
}

func timingStates(t *testing.T) (map[string]any, map[string]any) {
	t.Helper()
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: 1, Generation: 1, Epoch: 3, Stage: "test", Provider: "codex", Branch: "example"}
	r.StartStageTiming(1, 3)
	ms := int64(42)
	r.Timing.FixedTestCount = journal.MaxFixedTestTimings + 1
	for i := 0; i < r.Timing.FixedTestCount; i++ {
		r.Timing.FixedTests = append(r.Timing.FixedTests, journal.FixedTestTiming{ElapsedMS: &ms, Passed: i != 0})
	}
	if err := f.Executor.checkpoint(context.Background(), Descriptor{Descriptor: jobspec.Descriptor{ID: 1}}, r, "test"); err != nil {
		t.Fatal(err)
	}
	var cp map[string]any
	if err := json.Unmarshal(f.ReceivedStates[len(f.ReceivedStates)-1].View, &cp); err != nil {
		t.Fatal(err)
	}
	state, err := f.Executor.finish(r, "failed", "", "test", map[string]any{"other": "kept"})
	if err != nil {
		t.Fatal(err)
	}
	var fin map[string]any
	if err := json.Unmarshal(state.View, &fin); err != nil {
		t.Fatal(err)
	}
	return cp, fin
}

func TestStageTimingSentToIntake(t *testing.T) {
	cp, fin := timingStates(t)
	for _, view := range []map[string]any{cp, fin} {
		execution := view["workerExecution"].(map[string]any)
		tests := execution["fixedTests"].([]any)
		if execution["stageTimingMillis"].(map[string]any)["test"] == nil || execution["fixedTestCount"] != float64(101) || len(tests) != 100 {
			t.Fatalf("execution=%v", execution)
		}
		first := tests[0].(map[string]any)
		if first["passed"] != false || first["elapsedMs"] != float64(42) || tests[1].(map[string]any)["passed"] != true || execution["generation"] != float64(1) {
			t.Fatalf("execution=%v", execution)
		}
	}
}

func TestStageTimingShallowMerge(t *testing.T) {
	cp, fin := timingStates(t)
	saved := map[string]any{"unrelated": "preserved"}
	for _, update := range []map[string]any{cp, fin} {
		for k, v := range update {
			saved[k] = v
		}
	}
	if saved["unrelated"] != "preserved" || !reflect.DeepEqual(saved["workerExecution"], fin["workerExecution"]) {
		t.Fatalf("saved=%v", saved)
	}
	execution := saved["workerExecution"].(map[string]any)
	if execution["stageTimingMillis"] == nil || len(execution["fixedTests"].([]any)) != 100 {
		t.Fatal(execution)
	}
}
