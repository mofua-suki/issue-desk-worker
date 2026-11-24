package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTestStepZeroAndMissingPersist(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	zero := int64(0)
	r := &Record{ID: 1, Timing: &StageTiming{TestSteps: []TestStepTiming{{Step: "measured", ElapsedMillis: &zero}, {Step: "missing"}}}}
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(store.Root, "state", "job-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var timing struct {
		TestSteps []map[string]json.RawMessage `json:"testSteps"`
	}
	if err := json.Unmarshal(raw["stageTiming"], &timing); err != nil {
		t.Fatal(err)
	}
	if len(timing.TestSteps) != 2 || string(timing.TestSteps[0]["elapsedMillis"]) != "0" {
		t.Fatalf("measured zero: %s", data)
	}
	if _, ok := timing.TestSteps[1]["elapsedMillis"]; ok {
		t.Fatalf("missing duration present: %s", data)
	}
	loaded, err := store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	steps := loaded.Timing.TestSteps
	if len(steps) != 2 || steps[0].ElapsedMillis == nil || *steps[0].ElapsedMillis != 0 || steps[1].ElapsedMillis != nil {
		t.Fatalf("reloaded: %+v", steps)
	}
}

func TestStageTimingTransitionsAndResumeGap(t *testing.T) {
	r := &Record{Stage: "implement"}
	r.StartStageTiming(3, 7)
	start := r.Timing.Since
	r.Stage = "implement_report"
	r.observeStage(start.Add(time.Second))
	r.Stage = "test_report"
	r.observeStage(start.Add(3 * time.Second))
	r.Stage = "review"
	r.observeStage(start.Add(6 * time.Second))
	r.Stage = "merge"
	r.observeStage(start.Add(10 * time.Second))
	if r.Timing.Millis["implementing"] != 3000 || r.Timing.Millis["test"] != 3000 || r.Timing.Millis["review"] != 4000 {
		t.Fatalf("incorrect phase totals: %#v", r.Timing)
	}
	if _, ok := r.Timing.Millis["deploy"]; ok {
		t.Fatal("unobserved deploy is not zero")
	}
	snapshot := r.Timing.SnapshotAt(start.Add(4 * time.Second))
	if snapshot.LastStage != "test_report" || snapshot.Millis["test"] != 1000 {
		t.Fatalf("event before later checkpoint misclassified: %#v", snapshot)
	}
	if _, ok := snapshot.Millis["review"]; ok {
		t.Fatal("future stage counted at event")
	}
	// A loaded record has no live clock. Restart must not count downtime.
	r.timingRunning = false
	before := r.Timing.Millis["review"]
	r.StartStageTiming(3, 7)
	if !r.Timing.Gaps || r.Timing.Millis["review"] != before {
		t.Fatal("resume counted an unknown interval")
	}
	r.StartStageTiming(4, 8)
	if len(r.Timing.Millis) != 0 || r.Timing.Gaps {
		t.Fatal("new lease reused old totals")
	}
}

func TestStageTimingPersistsAndReloadsWithoutCountingDowntime(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r := &Record{ID: 1, Epoch: 2, Generation: 3, Stage: "implement_report"}
	r.StartStageTiming(3, 2)
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	r.Stage = "test_report"
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Load(1)
	if err != nil || saved.Timing == nil || saved.Timing.Stage != "test_report" || len(saved.Timing.Intervals) == 0 {
		t.Fatalf("timing not persisted: %#v %v", saved, err)
	}
	before := saved.Timing.Millis["implementing"]
	// This simulates a long gap without waiting or adding the gap to a duration.
	saved.Timing.Since = saved.Timing.Since.Add(-24 * time.Hour)
	saved.StartStageTiming(3, 2)
	if saved.Timing.Millis["implementing"] != before || !saved.Timing.Gaps {
		t.Fatal("resume added downtime")
	}
	if err := store.Save(saved); err != nil {
		t.Fatal(err)
	}
	again, err := store.Load(1)
	if err != nil || again.Timing.Millis["implementing"] != before || !again.Timing.Gaps {
		t.Fatalf("resume not persisted: %#v %v", again, err)
	}
}

func TestStageTimingGenerationAndReload(t *testing.T) {
	cases := []struct {
		name       string
		oldTiming  bool
		generation int
		epoch      int64
		wantGap    bool
		retain     bool
	}{
		{"same generation and epoch", true, 3, 7, true, true},
		{"epoch changed", true, 3, 8, true, false},
		{"generation changed", true, 4, 7, false, false},
		{"generation and epoch changed", true, 4, 8, false, false},
		{"legacy same generation", false, 3, 7, true, false},
		{"legacy pending flag", false, 3, 7, true, false},
		{"legacy new generation", false, 4, 8, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			r := &Record{ID: 1, Generation: 3, Epoch: 7, Stage: "implement"}
			if tc.oldTiming {
				start := time.Now().UTC().Add(-time.Hour)
				r.Timing = &StageTiming{Generation: 3, Epoch: 7, Stage: "implement", Since: start, Millis: map[string]int64{"implementing": 1000}, Gaps: true, Intervals: []StageInterval{{Stage: "implement", Start: start.Add(-time.Second), End: start}}}
			}
			if !tc.oldTiming {
				data := `{"id":1,"generation":3,"epoch":7,"stage":"implement"}`
				if tc.name == "legacy pending flag" {
					data = `{"id":1,"generation":3,"epoch":7,"stage":"implement","stageTimingPending":true}`
				}
				if err := os.WriteFile(filepath.Join(store.Root, "state", "job-1.json"), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := store.Save(r); err != nil {
				t.Fatal(err)
			}
			r, err = store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			r.StartStageTiming(tc.generation, tc.epoch)
			if r.Timing.Gaps != tc.wantGap || (len(r.Timing.Intervals) > 0) != tc.retain {
				t.Fatalf("unexpected timing: %#v", r.Timing)
			}
			if tc.retain && r.Timing.Millis["implementing"] != 1000 {
				t.Fatal("downtime counted")
			}
			if err := store.Save(r); err != nil {
				t.Fatal(err)
			}
			r, err = store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			if r.Timing.Gaps != tc.wantGap || (len(r.Timing.Intervals) > 1) != tc.retain {
				t.Fatalf("reload: %#v", r.Timing)
			}
			before := r.Timing.Millis["implementing"]
			r.StartStageTiming(tc.generation, tc.epoch)
			if !r.Timing.Gaps || r.Timing.Millis["implementing"] != before {
				t.Fatal("second resume lost gap or counted downtime")
			}
		})
	}
}

func TestStageTimingFreshSavedBeforeTracking(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r := &Record{ID: 1, Generation: 4, Stage: "implement"}
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	r, err = store.LoadAfterSave(r)
	if err != nil {
		t.Fatal(err)
	}
	r.StartStageTiming(4, 2)
	if r.Timing.Gaps {
		t.Fatal("fresh saved record marked as resumed")
	}
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	r, err = store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	r.StartStageTiming(4, 2)
	if !r.Timing.Gaps {
		t.Fatal("subsequent resume must have a gap")
	}
}
