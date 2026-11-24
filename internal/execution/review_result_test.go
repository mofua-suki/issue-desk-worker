package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/jobspec"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"issue-desk-worker/internal/intake"
)

func TestSecondReviewLongHeldResultCarry(t *testing.T) {
	for _, cleared := range []bool{false, true} {
		t.Run(map[bool]string{false: "review", true: "previousReview"}[cleared], func(t *testing.T) {
			f := newPipeline(t, "approve")
			r := proposalRecord()
			r.ReviewHolds = 1
			r.PreviousReview = secondReviewResponse("data", "previous must")
			r.Review = nil
			// Distinct long fields cannot rely on a small compression window or
			// repeated characters to fit in the real intake request envelope.
			rng := rand.New(rand.NewSource(320))
			fields := make([]string, 3)
			for i := range fields {
				text := make([]byte, 20000)
				for j := range text {
					text[j] = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"[rng.Intn(62)]
				}
				fields[i] = string(text)
			}
			item := followupProposalItem{Problem: fields[0], Expected: fixtureEvidence + fields[1], Test: fixtureVerification + fields[2]}
			r.PreviousReview = changeSecondReview(r.PreviousReview, "deferredReviewItems", []followupProposalItem{item})
			raw := changeSecondReview(secondReviewResponse("acceptance", item.Problem), "breaking", true)
			raw = changeSecondReview(raw, "findingSpecs", []reviewFindingSpec{{Problem: item.Problem, Expected: item.Expected, Test: item.Test}})
			review, saved, _ := decideReview(r, raw)
			r.Review = saved
			if review.Approved() || len(review.DeferredReviewItems) != 1 || len(saved) < 128*1024 {
				t.Fatal("fixture must hold a review larger than the intake limit", len(saved))
			}
			if cleared {
				clearFollowedResults(r)
			}
			state, err := f.Executor.finish(r, "held", "review_held", "breaking", nil)
			if err != nil {
				t.Fatal(err)
			}
			request := encode(struct {
				Epoch int64 `json:"epoch"`
				intake.State
			}{r.Epoch, state})
			if len(request) > 128*1024 {
				t.Fatal("final state exceeds intake limit", len(request))
			}
			// The new PC has neither the original review nor its journal. Its
			// only input is the production finish result accepted by intake.
			next := proposalRecord()
			next.Generation, next.Head, next.Review = 1, strings.Repeat("b", 40), nil
			seedPreviousResults(next, Descriptor{Descriptor: jobspec.Descriptor{Generation: 1, Result: state.Result}})
			var carried Review
			if json.Unmarshal(next.PreviousReview, &carried) != nil || !reflect.DeepEqual(carried.DeferredReviewItems, []followupProposalItem{item}) || !carried.Breaking {
				t.Fatal("long retained details or hold lost in final result")
			}
			_, next.Review, _ = decideReview(next, encode(map[string]any{"decision": "approve"}))
			if err := f.Executor.postFollowupProposals(context.Background(), next); err != nil {
				t.Fatal(err)
			}
			var delivered []followupProposalItem
			for _, body := range proposalBodies(t, f) {
				var items []followupProposalItem
				if err := json.Unmarshal(body["items"], &items); err != nil {
					t.Fatal(err)
				}
				delivered = append(delivered, items...)
			}
			if !reflect.DeepEqual(delivered, proposalWireItems([]followupProposalItem{item})) || len(next.FollowupProposals) != (len(proposalWireItems([]followupProposalItem{item}))+4)/5 {
				t.Fatal("long fields missing or duplicated after resolving hold", len(delivered), next.FollowupProposals)
			}
		})
	}
}
