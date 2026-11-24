package execution

import (
	"encoding/json"
	"testing"

	"issue-desk-worker/internal/journal"
)

// Exercise the legacy JSON shape through the real store and finish result.
func TestLegacyReviewCountsMigrationAndResult(t *testing.T) {
	f := newPipeline(t, "approve")
	var r journal.Record
	if err := json.Unmarshal([]byte(`{"id":1,"reviewRounds":[{"kind":"full"},{"kind":"delta"},{"kind":"skipped"}]}`), &r); err != nil {
		t.Fatal(err)
	}
	r.Result = implementationFixture()
	if err := f.Executor.Store.Save(&r); err != nil {
		t.Fatal(err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved == nil {
		t.Fatalf("load: %v", err)
	}
	saved.AddReviewRound(journal.ReviewRound{Kind: "skipped"})
	state, err := f.Executor.finish(saved, "merged", "", "done", nil)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Counts journal.ReviewCounts `json:"reviewCounts"`
	}
	if err := json.Unmarshal(state.Result, &result); err != nil {
		t.Fatal(err)
	}
	want := journal.ReviewCounts{Full: 1, Delta: 1, Skipped: 2}
	if result.Counts != want {
		t.Fatalf("counts = %+v, want %+v", result.Counts, want)
	}
	reloaded, err := f.Executor.Store.Load(1)
	if err != nil || reloaded == nil || reloaded.ReviewCounts == nil || *reloaded.ReviewCounts != want {
		t.Fatalf("reload: %+v, %v", reloaded, err)
	}
}
