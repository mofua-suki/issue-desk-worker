package execution

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"

	"issue-desk-worker/internal/journal"
)

func TestAggregateActualCompletionResults(t *testing.T) {
	f := newPipeline(t, "approve")
	var results []json.RawMessage
	for i, kinds := range [][]string{{"full", "delta", "skipped"}, {"full", "skipped", "skipped", "skipped", "delta"}} {
		r := &journal.Record{ID: int64(i + 1), Result: implementationFixture()}
		for _, kind := range kinds {
			r.AddReviewRound(journal.ReviewRound{Kind: kind})
		}
		if err := f.Executor.Store.Save(r); err != nil {
			t.Fatal(err)
		}
		saved, err := f.Executor.Store.Load(r.ID)
		if err != nil || saved == nil {
			t.Fatalf("load: %v", err)
		}
		state, err := f.Executor.finish(saved, "merged", "", "done", nil)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, state.Result)
	}
	input, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "../../cmd/review-counts")
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("aggregate: %v: %s", err, output)
	}
	var got struct {
		Jobs    int                                    `json:"jobs"`
		Missing int                                    `json:"missing"`
		Counts  journal.ReviewCounts                   `json:"counts"`
		Ratios  struct{ Full, Delta, Skipped float64 } `json:"ratios"`
	}
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatal(err)
	}
	if got.Jobs != 2 || got.Missing != 0 || got.Counts != (journal.ReviewCounts{Full: 2, Delta: 2, Skipped: 4}) || got.Ratios.Full != .25 || got.Ratios.Delta != .25 || got.Ratios.Skipped != .5 {
		t.Fatalf("report: %s", output)
	}
}
