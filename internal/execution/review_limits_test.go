package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

func TestReviewRoundsBoundedInStorageAndFinish(t *testing.T) {
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: 1, Result: implementationFixture()}
	for i := 0; i < 1000; i++ {
		r.AddReviewRound(journal.ReviewRound{Kind: "full", Head: fmt.Sprint(i)})
	}
	if len(r.ReviewRounds) != journal.MaxReviewRounds || r.ReviewRounds[0].Head != "980" {
		t.Fatalf("rounds: %+v", r.ReviewRounds)
	}
	// Construct a legacy record with no counts and an oversized history.
	r = &journal.Record{ID: 1, Result: implementationFixture()}
	for i := 0; i < 1000; i++ {
		r.ReviewRounds = append(r.ReviewRounds, journal.ReviewRound{Kind: "skipped", Head: fmt.Sprint(i)})
	}
	state, err := f.Executor.finish(r, "merged", "", "done", nil)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ReviewRounds []journal.ReviewRound `json:"reviewRounds"`
		ReviewCounts journal.ReviewCounts  `json:"reviewCounts"`
	}
	if err := json.Unmarshal(state.Result, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.ReviewRounds) != journal.MaxReviewRounds || result.ReviewRounds[0].Head != "980" || result.ReviewRounds[19].Head != "999" {
		t.Fatalf("result rounds: %+v", result.ReviewRounds)
	}
	if result.ReviewCounts != (journal.ReviewCounts{Skipped: 1000}) {
		t.Fatalf("legacy counts: %+v", result.ReviewCounts)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.ReviewRounds) != journal.MaxReviewRounds {
		t.Fatalf("saved rounds: %d", len(saved.ReviewRounds))
	}
}

func TestReviewMarkerChangeMeasuredAtPreparation(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(w.Tree, "feature.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(w.Tree, "add", "feature.txt")
	f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "feature")
	snap, err := w.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(t.TempDir(), "full.patch")
	if err = os.WriteFile(full, []byte(snap.Diff), 0600); err != nil {
		t.Fatal(err)
	}
	r := &journal.Record{ID: 1}
	review, raw := normalizeReview(encode(map[string]any{"decision": "approve"}))
	body := "受入条件: feature.txtを生成する\n<!-- issue-desk-scope:{\"paths\":[\"feature.txt\"],\"v\":1} -->"
	if err = os.MkdirAll(filepath.Dir(reviewBaselinePath(c.Root, 1)), 0700); err != nil {
		t.Fatal(err)
	}
	if err = f.Executor.saveReviewBaseline(r, snap, review, raw, "full", reviewRequestHash(Descriptor{}, "Implement feature", body)); err != nil {
		t.Fatal(err)
	}
	// Measure decisions on the same real Git snapshot: unchanged body vs rewritten marker.
	changed := strings.Replace(body, "feature.txt\"],", "feature.txt\",\"other.txt\"],", 1)
	if changed == body {
		t.Fatal("marker was not rewritten")
	}
	for _, tc := range []struct{ body, kind string }{{body, "skipped"}, {changed, "full"}} {
		kind, _, _ := f.Executor.prepareReview(ctx, r, w, snap, full, Descriptor{}, reviewRequestHash(Descriptor{}, "Implement feature", tc.body))
		if kind != tc.kind {
			t.Fatalf("got %s, want %s", kind, tc.kind)
		}
	}
	t.Log("同一差分2回: 本文不変は省略1/1、マーカー変更は省略0/1 (本番頻度は未測定)")
}

func TestReviewDiffHashDistinguishesHunksAndMovedLines(t *testing.T) {
	prefix := "diff --git a/a b/a\n--- a/a\n+++ b/a\n"
	patches := []string{
		prefix + "@@ -1,4 +1,4 @@\n first\n-old\n+new\n second\n third\n",
		prefix + "@@ -1,4 +1,4 @@\n first\n second\n-old\n+new\n third\n",
		prefix + "@@ -1,2 +1,2 @@\n first\n-old\n+new\n@@ -4,1 +4,1 @@\n third\n",
	}
	seen := map[string]bool{}
	for _, patch := range patches {
		h, ok := reviewDiffHash(patch)
		if !ok || seen[h] {
			t.Fatalf("patch was not distinguished: %s", patch)
		}
		seen[h] = true
	}
}

func TestReviewCountsPersistAndFinish(t *testing.T) {
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: 1, Result: implementationFixture()}
	kinds := []string{"full", "delta", "skipped"}
	for i := 0; i < 1000; i++ {
		r.AddReviewRound(journal.ReviewRound{Kind: kinds[i%len(kinds)]})
	}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	want := journal.ReviewCounts{Full: 334, Delta: 333, Skipped: 333}
	if saved.ReviewCounts == nil || *saved.ReviewCounts != want || len(saved.ReviewRounds) != journal.MaxReviewRounds {
		t.Fatalf("saved counts=%+v rounds=%d", saved.ReviewCounts, len(saved.ReviewRounds))
	}
	state, err := f.Executor.finish(saved, "merged", "", "done", nil)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ReviewCounts journal.ReviewCounts `json:"reviewCounts"`
	}
	if err := json.Unmarshal(state.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.ReviewCounts != want {
		t.Fatalf("result counts=%+v", result.ReviewCounts)
	}
}

func TestSaveTruncatesCallerReviewRounds(t *testing.T) {
	f := newPipeline(t, "approve")
	r := &journal.Record{ID: 1}
	// Simulate an uninitialized legacy record; Save counts it before truncation.
	for i := 0; i < 25; i++ {
		r.ReviewRounds = append(r.ReviewRounds, journal.ReviewRound{Kind: "full", Head: fmt.Sprint(i)})
	}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	if len(r.ReviewRounds) != journal.MaxReviewRounds || r.ReviewRounds[0].Head != "5" || r.ReviewCounts.Full != 25 {
		t.Fatalf("caller rounds=%+v counts=%+v", r.ReviewRounds, r.ReviewCounts)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved == nil || len(saved.ReviewRounds) != journal.MaxReviewRounds || *saved.ReviewCounts != *r.ReviewCounts {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
}
