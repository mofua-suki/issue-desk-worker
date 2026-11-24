package execution

import (
	"testing"

	"issue-desk-worker/internal/jobspec"
	"issue-desk-worker/internal/journal"
)

// 別のPCへ移った仕事は、受付に残る最後の結果のレビュー回数から数え続ける。手元の方が多い回数は下げない。
func TestCarryReviewCountsFromLastResult(t *testing.T) {
	d := Descriptor{Descriptor: jobspec.Descriptor{Result: encode(map[string]any{"reviewCounts": map[string]int{"full": 2, "delta": 1, "skipped": 0}})}}
	fresh := &journal.Record{}
	carryReviewCounts(fresh, d)
	fresh.AddReviewRound(journal.ReviewRound{Kind: "delta"})
	if got := *fresh.ReviewCounts; got != (journal.ReviewCounts{Full: 2, Delta: 2}) {
		t.Fatalf("別のPCの回数を引き継ぎません: %+v", got)
	}
	carryReviewCounts(fresh, d)
	if got := *fresh.ReviewCounts; got != (journal.ReviewCounts{Full: 2, Delta: 2}) {
		t.Fatalf("取り込み直しで回数が変わりました: %+v", got)
	}
	local := &journal.Record{}
	for _, kind := range []string{"full", "full", "full", "skipped"} {
		local.AddReviewRound(journal.ReviewRound{Kind: kind})
	}
	carryReviewCounts(local, d)
	if got := *local.ReviewCounts; got != (journal.ReviewCounts{Full: 3, Delta: 1, Skipped: 1}) {
		t.Fatalf("回数: %+v", got)
	}
	none := &journal.Record{}
	carryReviewCounts(none, Descriptor{})
	if none.ReviewCounts != nil {
		t.Fatal("結果が無いのに回数を作りました")
	}
}
