package execution

import (
	"encoding/json"
	"strings"
	"testing"

	"issue-desk-worker/internal/journal"
)

func TestRestoredDeliveryUsesOutcomeInsteadOfDiagnosticText(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, diagnostic string
		retained                  bool
	}{
		{"unknown with changed wording", "unknown", "transport response lost", true},
		{"explicit rejection mentioning unknown", "rejected", "未確認の要求を拒否しました", false},
		{"not attempted mentioning unknown", "not_attempted", "未確認の設定", false},
		{"legacy unknown", "", "投稿結果が未確認です", true},
		{"legacy rejected", "", "HTTP 403", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := followupProposalItem{Problem: "delivery finding", Expected: "preserve original identity", Test: "no duplicate post"}
			review := Review{Decision: "approve", Improvements: []string{item.Problem}, ImprovementSpecs: []followupProposalItem{item}}
			r := &journal.Record{ID: 1, Generation: 2, Head: strings.Repeat("a", 40), Review: encode(review)}
			proposal := journal.FollowupProposal{ID: followupProposalID(r, 0), Generation: r.Generation, Head: r.Head, Status: "failed", Error: tc.diagnostic, DeliveryOutcome: tc.outcome}
			r.FollowupProposals = []journal.FollowupProposal{proposal}
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var restored journal.Record
			if err = json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			retainSentReviewDeliveries(&restored)
			_, deliveries := retainedReviewData(restored.Review)
			if (len(deliveries) > 0) != tc.retained {
				t.Fatalf("deliveries=%+v", deliveries)
			}
			if tc.retained && (len(deliveries) != 1 || deliveries[0].Proposal != proposal) {
				t.Fatalf("identity/outcome changed: %+v", deliveries)
			}
		})
	}
}
