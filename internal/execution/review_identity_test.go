package execution

import (
	"context"
	"issue-desk-worker/internal/jobspec"
	"strings"
	"testing"

	"issue-desk-worker/internal/journal"
)

func TestReviewRequestIdentity(t *testing.T) {
	d := Descriptor{Descriptor: jobspec.Descriptor{Generation: 2}}
	hash := reviewRequestHash(d, "題", "本文")
	r := &journal.Record{ReviewBaseline: &journal.ReviewBaseline{Generation: 2, RequestHash: hash}}
	if !reviewBaselineMatches(r, d, hash) {
		t.Fatal("same request rejected")
	}
	for _, tc := range []struct {
		d           Descriptor
		title, body string
	}{
		{Descriptor{Descriptor: jobspec.Descriptor{Generation: 3}}, "題", "本文"},
		{d, "別題", "本文"}, {d, "題", "別本文"},
	} {
		if reviewBaselineMatches(r, tc.d, reviewRequestHash(tc.d, tc.title, tc.body)) {
			t.Fatal("changed request accepted")
		}
	}
	r.ReviewBaseline.RequestHash = ""
	if reviewBaselineMatches(r, d, hash) {
		t.Fatal("missing identity accepted")
	}
}

func TestSkippedReviewNoticeIdentifiesOriginalHead(t *testing.T) {
	f := newPipeline(t, "approve")
	head := strings.Repeat("a", 40)
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, PR: 1, Head: strings.Repeat("b", 40), Base: strings.Repeat("c", 40), Review: encode(Review{Decision: "approve", Summary: strings.Repeat("長", 61000)}), ReviewSkipped: true, ReviewBaseline: &journal.ReviewBaseline{Head: head}}
	done, err := f.Executor.reviewNotice(context.Background(), r)
	if err != nil || !done || len(f.Reviews) != 1 {
		t.Fatalf("notice: %v", err)
	}
	body, _ := f.Reviews[0]["body"].(string)
	if !strings.Contains(body, "承認を引き継ぎました") || !strings.Contains(body, head) {
		t.Fatal("inherited approval missing")
	}
}
