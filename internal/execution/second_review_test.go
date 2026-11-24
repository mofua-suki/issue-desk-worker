package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

// Start with the shipped provider shape rather than a reduced decision object.
func secondReviewResponse(kind, text string) json.RawMessage {
	var value map[string]json.RawMessage
	_ = json.Unmarshal(minorReview4553(), &value)
	value["decision"] = encode("hold")
	value["findings"], value["findingKinds"], value["findingSeverities"] = encode([]string{text}), encode([]string{kind}), encode([]string{"must"})
	value["findingSpecs"] = encode([]reviewFindingSpec{{Group: kind, Problem: text, Expected: fixtureEvidence + "期待: " + text, Test: fixtureVerification + " 検査: " + text}})
	value["improvements"], value["resolvableUncertainties"], value["recordOnly"] = encode([]string{}), encode([]string{}), encode([]string{})
	return encode(value)
}

func changeSecondReview(raw json.RawMessage, key string, value any) json.RawMessage {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(raw, &object)
	object[key] = encode(value)
	return encode(object)
}

func TestSecondReviewDecisionBoundaries(t *testing.T) {
	prior := secondReviewResponse("acceptance", "前の問題")
	current := secondReviewResponse("acceptance", "新しい問題")
	for _, tc := range []struct {
		name    string
		prior   json.RawMessage
		current json.RawMessage
		change  func(*journal.Record)
	}{
		{name: "first acceptance", current: current},
		{name: "first other", current: secondReviewResponse("other", "新しい問題")},
		{name: "new acceptance hold", prior: prior, current: current},
		{name: "new other", prior: prior, current: secondReviewResponse("other", "新しい問題")},
		{name: "head differs without holds", prior: prior, current: current, change: func(r *journal.Record) { r.ReviewHolds = 0 }},
		{name: "other head differs without holds", prior: prior, current: secondReviewResponse("other", "新しい問題"), change: func(r *journal.Record) { r.ReviewHolds = 0 }},
		{name: "other same head holds", prior: prior, current: secondReviewResponse("other", "新しい問題"), change: func(r *journal.Record) { r.ReviewHead = r.Head }},
		{name: "carried head without holds", prior: changeSecondReview(prior, "reviewedHead", strings.Repeat("a", 40)), current: current, change: func(r *journal.Record) {
			r.ReviewHolds = 0
			r.ReviewHead = ""
			r.PreviousReview, r.Review = r.Review, nil
		}},
		{name: "same head holds", prior: prior, current: current, change: func(r *journal.Record) { r.ReviewHead = r.Head }},
		{name: "same head no holds", prior: prior, current: current, change: func(r *journal.Record) { r.ReviewHead = r.Head; r.ReviewHolds = 0 }},
		{name: "unknown head no holds", prior: prior, current: current, change: func(r *journal.Record) { r.ReviewHead = ""; r.ReviewHolds = 0 }},
		{name: "carried review", prior: prior, current: current, change: func(r *journal.Record) { r.PreviousReview, r.Review = r.Review, nil }},
		{name: "local precedes carried", prior: prior, current: prior, change: func(r *journal.Record) { r.PreviousReview = current }},
		{name: "missing despite count and head", current: current},
		{name: "invalid", prior: json.RawMessage(`{"findings":`), current: current},
		{name: "omitted", prior: json.RawMessage(`{"omitted":true}`), current: current},
		{name: "missing findings", prior: json.RawMessage(`{"decision":"hold"}`), current: current},
		{name: "prior classification mismatch", prior: changeSecondReview(prior, "findingKinds", []string{}), current: current},
		{name: "prior unknown kind", prior: secondReviewResponse("unknown", "前の問題"), current: current},
		{name: "prior invalid reviewed head", prior: changeSecondReview(prior, "reviewedHead", "invalid"), current: current},
		{name: "prior invalid reviewed generation", prior: changeSecondReview(prior, "reviewedGeneration", -1), current: current},
		{name: "prior invalid reviewed epoch", prior: changeSecondReview(prior, "reviewedEpoch", -1), current: current},
		{name: "prior spec mismatch", prior: changeSecondReview(prior, "findingSpecs", []reviewFindingSpec{{Problem: "a"}, {Problem: "b"}}), current: current},
		{name: "invalid local cannot use older carried", prior: json.RawMessage(`{"omitted":true}`), current: current, change: func(r *journal.Record) { r.PreviousReview = prior }},
		{name: "current classification mismatch", prior: prior, current: changeSecondReview(current, "findingSeverities", []string{})},
		{name: "current unknown kind", prior: prior, current: secondReviewResponse("unknown", "新しい問題")},
		{name: "text whitespace and changed kind", prior: secondReviewResponse("data", " 前の\n問題 "), current: secondReviewResponse("other", "前の 問題")},
		{name: "structured problem", prior: changeSecondReview(prior, "findingSpecs", []reviewFindingSpec{{Problem: "共通の problem"}}), current: changeSecondReview(current, "findingSpecs", []reviewFindingSpec{{Problem: " 共通の\nproblem "}})},
		{name: "current text prior problem", prior: changeSecondReview(prior, "findingSpecs", []reviewFindingSpec{{Problem: "新しい問題に関する詳細な記述"}}), current: secondReviewResponse("acceptance", "新しい問題に関する詳細な記述")},
		{name: "current problem prior text", prior: secondReviewResponse("acceptance", "前の問題に関する詳細な記述"), current: changeSecondReview(current, "findingSpecs", []reviewFindingSpec{{Problem: "前の問題に関する詳細な記述"}})},
		{name: "prior minor is not heavy", prior: changeSecondReview(current, "findingSeverities", []string{"minor"}), current: current},
		{name: "duplicate matched musts", prior: prior, current: changeSecondReview(changeSecondReview(changeSecondReview(changeSecondReview(prior, "findings", []string{"前の問題", "前の問題"}), "findingKinds", []string{"acceptance", "other"}), "findingSeverities", []string{"must", "must"}), "findingSpecs", []reviewFindingSpec{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &journal.Record{Review: tc.prior, ReviewHead: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), ReviewHolds: 1}
			if tc.change != nil {
				tc.change(r)
			}
			review, saved, _ := decideReview(r, tc.current)
			if review.Approved() {
				t.Fatalf("must review approved: %s", saved)
			}
			if len(review.Severities) == 1 && len(review.Kinds) == 1 && (review.Kinds[0] == "acceptance" || review.Kinds[0] == "other") && review.Severities[0] != "must" {
				t.Fatalf("must lost: %s", saved)
			}
			if len(review.DeferredReviewItems) != 0 {
				t.Fatalf("ineligible must was deferred: %s", saved)
			}
		})
	}
}

func TestSecondReviewProtectedKindsAndMixedHolds(t *testing.T) {
	for _, boundary := range []string{"security", "regression", "data", "blocking", "breaking", "tooLarge", "userJudgments", "uncertainties"} {
		t.Run(boundary, func(t *testing.T) {
			r := &journal.Record{Review: secondReviewResponse("other", "前回"), Head: strings.Repeat("b", 40), ReviewHolds: 1}
			current := secondReviewResponse("acceptance", "後続に残す")
			switch boundary {
			case "breaking", "tooLarge":
				current = changeSecondReview(current, boundary, true)
			case "userJudgments", "uncertainties":
				current = changeSecondReview(current, boundary, []string{"保留"})
			default:
				// First check the protected must by itself, then with a target must.
				review, _, _ := decideReview(r, secondReviewResponse(boundary, "重大"))
				if review.Approved() || review.Severities[0] != "must" {
					t.Fatal(review)
				}
				current = changeSecondReview(current, "findings", []string{"後続に残す", "重大"})
				current = changeSecondReview(current, "findingKinds", []string{"acceptance", boundary})
				current = changeSecondReview(current, "findingSeverities", []string{"must", "must"})
				current = changeSecondReview(current, "findingSpecs", []reviewFindingSpec{{Problem: "後続に残す", Expected: "期待", Test: "検査"}, {Problem: "重大"}})
			}
			review, saved, _ := decideReview(r, current)
			if review.Approved() || review.Severities[0] != "must" || len(review.DeferredReviewItems) != 0 {
				t.Fatalf("boundary lost: %s", saved)
			}

		})
	}
}

func TestSecondReviewExecutionDelivery(t *testing.T) {
	for _, kind := range []string{"acceptance", "other"} {
		for _, delivery := range []string{"sent", "refused"} {
			t.Run(kind+"/"+delivery, func(t *testing.T) {
				f := newPipeline(t, "approve")
				f.AllowUsage = true
				if delivery == "refused" {
					refuseOnlyProposalPosts(t, f)
				}
				original := f.Executor.Call
				reviews, implementations := 0, 0
				f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
					a, err := original(ctx, req)
					if !req.Review {
						implementations++
						if implementations > 1 {
							if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte("修正"), 0600); err != nil {
								t.Fatal(err)
							}
						}
						return a, err
					}
					reviews++
					text := "初回の重大指摘"
					if reviews == 2 {
						text = "後続に回す本文"
					} else if reviews > 2 {
						t.Fatal("second review should approve")
					}
					a.JSON = secondReviewResponse(kind, text)
					if reviews == 2 {
						a.JSON = changeSecondReview(a.JSON, "findingSeverities", []string{"minor"})
					}
					a.JSON = changeSecondReview(a.JSON, "improvements", []string{"無関係な記録"})
					a.JSON = changeSecondReview(a.JSON, "recordOnly", []string{"無関係な記録"})
					return a, err
				}
				state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				if err != nil || state.Status != "merged" || reviews != 2 || implementations != 2 || f.MergePosts != 1 {
					t.Fatalf("state=%+v err=%v reviews=%d implementations=%d", state, err, reviews, implementations)
				}
				r, err := f.Executor.Store.Load(1)
				if err != nil || len(r.FollowupProposals) != 1 {
					t.Fatal(r, err)
				}
				var review Review
				_ = json.Unmarshal(r.Review, &review)
				want := followupProposalItem{Problem: "後続に回す本文", Expected: fixtureEvidence + "期待: 後続に回す本文", Test: fixtureVerification + " 検査: 後続に回す本文"}
				if !review.Approved() || !reflect.DeepEqual(review.RecordOnly, []string{"無関係な記録"}) || len(review.Improvements) != 2 || review.ImprovementSpecs[1] != want {
					t.Fatal(review)
				}
				if delivery == "refused" {
					if r.FollowupProposals[0].Status != "failed" || r.FollowupProposals[0].Error == "" || len(proposalBodies(t, f)) != 0 {
						t.Fatal(r.FollowupProposals)
					}
					return
				}
				bodies := proposalBodies(t, f)
				var items []followupProposalItem
				if len(bodies) != 1 || json.Unmarshal(bodies[0]["items"], &items) != nil || !reflect.DeepEqual(items, []followupProposalItem{want}) || r.FollowupProposals[0].Status != "sent" {
					t.Fatal(bodies, r.FollowupProposals)
				}
			})
		}
	}
}

func TestSecondReviewRetentionAcrossResume(t *testing.T) {
	parallelCases := make(chan struct{}, 4)
	for _, path := range []string{"overwrite", "generation", "moved", "instruction", "retry", "clear"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			parallelCases <- struct{}{}
			defer func() { <-parallelCases }()
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			original := f.Executor.Call
			reviews, implementations := 0, 0
			resuming := false
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				// tooLarge/other must return to implementation without finishing.
				if !req.Review && reviews == 2 && !resuming {
					cancel()
					return providers.Answer{Outcome: "aborted"}, context.Canceled
				}
				a, err := original(ctx, req)
				if !req.Review {
					implementations++
					if implementations > 1 {
						if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte(strings.Repeat("修正\n", implementations)), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return a, err
				}
				reviews++
				switch reviews {
				case 1:
					a.JSON = secondReviewResponse("acceptance", "最初に直す問題")
				case 2:
					a.JSON = secondReviewResponse("other", "保持する本文")
					switch path {
					case "overwrite":
						a.JSON = changeSecondReview(a.JSON, "findings", []string{"保持する本文", "重大な回帰"})
						a.JSON = changeSecondReview(a.JSON, "findingKinds", []string{"other", "regression"})
						a.JSON = changeSecondReview(a.JSON, "findingSeverities", []string{"must", "must"})
						a.JSON = changeSecondReview(a.JSON, "findingSpecs", []reviewFindingSpec{{Problem: "保持する本文", Expected: fixtureEvidence + "期待: 保持する本文", Test: fixtureVerification + " 検査: 保持する本文"}, {Problem: "重大な回帰"}})
					case "generation", "moved":
						a.JSON = changeSecondReview(a.JSON, "breaking", true)
					case "instruction":
						a.JSON = changeSecondReview(a.JSON, "userJudgments", []string{"判断"})
					case "retry":
						a.JSON = changeSecondReview(a.JSON, "uncertainties", []string{"旧判断"})
					case "clear":
						a.JSON = changeSecondReview(a.JSON, "tooLarge", true)
					}
				default:
					// Re-reporting the retained body in improvements must not duplicate it
					// or replace the original structured details with fallback text.
					a.JSON = changeSecondReview(a.JSON, "improvements", []string{"保持する本文", "記録だけ"})
					a.JSON = changeSecondReview(a.JSON, "recordOnly", []string{"保持する本文", "記録だけ"})
				}
				return a, err
			}
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			interrupted := path == "overwrite" || path == "clear"
			if (interrupted && !errors.Is(err, context.Canceled)) || (!interrupted && (err != nil || state.Status != "held")) || reviews != 2 || len(proposalBodies(t, f)) != 0 {
				t.Fatalf("mixed review must not publish: state=%+v err=%v reviews=%d", state, err, reviews)
			}
			r, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			r.Review = changeSecondReview(r.Review, "deferredReviewItems", []followupProposalItem{{Problem: "保持する本文", Expected: fixtureEvidence + "期待: 保持する本文", Test: fixtureVerification + " 検査: 保持する本文"}})
			var held Review
			_ = json.Unmarshal(r.Review, &held)
			if held.Approved() || len(held.DeferredReviewItems) != 1 {
				t.Fatalf("not retained in saved held review: %s", r.Review)
			}
			r.FinalReported = true
			if path == "clear" {
				// This is also used by follow/merge conflict and continuation paths.
				r.Stage = "review"
				clearFollowedResults(r)
				if hasJSON(r.Review) || !hasJSON(r.PreviousReview) {
					t.Fatal("review invalidation lost carryover")
				}
				r.Stage = "implement"
			}
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			if path == "generation" || path == "moved" {
				var result map[string]json.RawMessage
				_ = json.Unmarshal(state.Result, &result)
				result["review"] = r.Review
				state.Result = encode(result)
				nextGeneration(t, f, state)
				if path == "moved" {
					// Intake supplies the published branch/PR/head to the next PC.
					var d Descriptor
					_ = json.Unmarshal(f.Job.Raw, &d)
					d.Branch, d.HeadSHA = state.Branch, state.HeadSHA
					if state.PR != nil {
						d.PR = *state.PR
					}
					f.Job.Raw = encode(d)
					if err := os.Remove(filepath.Join(f.Executor.Store.Root, "state", "job-1.json")); err != nil {
						t.Fatal(err)
					}
				}
			} else if path == "instruction" || path == "retry" {
				var d Descriptor
				_ = json.Unmarshal(f.Job.Raw, &d)
				if path == "instruction" {
					d.View["instructions"] = encode([]map[string]string{{"id": "human:1", "text": "追加指示"}})
				} else {
					d.View["retryFromBase"] = encode(map[string]int{"generation": d.Generation})
				}
				f.Job.Raw = encode(d)
			}
			resuming = true
			state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" {
				t.Fatalf("resume=%+v err=%v", state, err)
			}
			bodies := proposalBodies(t, f)
			var items []followupProposalItem
			want := followupProposalItem{Problem: "保持する本文", Expected: fixtureEvidence + "期待: 保持する本文", Test: fixtureVerification + " 検査: 保持する本文"}
			if len(bodies) != 1 || json.Unmarshal(bodies[0]["items"], &items) != nil || !reflect.DeepEqual(items, []followupProposalItem{want}) {
				t.Fatalf("retained proposal lost/duplicated: %v", bodies)
			}
			var result struct {
				FollowupProposals []journal.FollowupProposal `json:"followupProposals"`
			}
			if json.Unmarshal(state.Result, &result) != nil || len(result.FollowupProposals) != 1 || result.FollowupProposals[0].Status != "sent" {
				t.Fatal(string(state.Result))
			}
		})
	}
}

func TestSecondReviewUnknownDeliveryDoesNotRepost(t *testing.T) {
	f := newPipeline(t, "approve")
	r := proposalRecord()
	r.ReviewHolds = 1
	r.PreviousReview, r.Review = changeSecondReview(secondReviewResponse("acceptance", "前回"), "deferredReviewItems", []followupProposalItem{{Problem: "降格対象", Expected: fixtureEvidence + "期待: 降格対象", Test: fixtureVerification + " 検査: 降格対象"}}), nil
	raw := changeSecondReview(secondReviewResponse("other", "降格対象"), "findingSeverities", []string{"minor"})
	raw = changeSecondReview(raw, "recordOnly", []string{"降格対象", "記録だけ"})
	raw = changeSecondReview(raw, "improvements", []string{"記録だけ"})
	_, r.Review, _ = decideReview(r, raw)
	f.DropNoticeResponse, f.HideNotices = true, true
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil || f.NoticePosts != 1 || len(r.FollowupProposals) != 1 || r.FollowupProposals[0].Status != "failed" || r.FollowupProposals[0].Error == "" {
		t.Fatalf("err=%v posts=%d proposals=%+v", err, f.NoticePosts, r.FollowupProposals)
	}
	bodies := proposalBodies(t, f)
	var items []followupProposalItem
	if len(bodies) != 1 || json.Unmarshal(bodies[0]["items"], &items) != nil || len(items) != 1 || items[0].Problem != "降格対象" {
		t.Fatal(bodies)
	}
	// Delivery failure is exposed through the same final-result outlet.
	state, err := f.Executor.finish(r, "merged", "", "完了", nil)
	var result struct {
		FollowupProposals []journal.FollowupProposal `json:"followupProposals"`
	}
	if err != nil || json.Unmarshal(state.Result, &result) != nil || len(result.FollowupProposals) != 1 || result.FollowupProposals[0].Status != "failed" || state.Status != "merged" {
		t.Fatal(state, err)
	}
	// A later visible marker recovers the original result by GET, without POST.
	f.HideNotices = false
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil || f.NoticePosts != 1 {
		t.Fatal("unknown delivery was reposted during reconciliation", err, f.NoticePosts)
	}
	saved, err := f.Executor.Store.Load(r.ID)
	if err != nil || len(saved.FollowupProposals) != 1 || saved.FollowupProposals[0].Status != "sent" || saved.FollowupProposals[0].Error != "" {
		t.Fatal("marker reconciliation was not saved", saved, err)
	}
}

func TestSecondReviewMustReturnsToImplementation(t *testing.T) {
	for _, kind := range []string{"acceptance", "other"} {
		t.Run(kind, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			original := f.Executor.Call
			reviews, implementations := 0, 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a, err := original(ctx, req)
				if !req.Review {
					implementations++
					if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte(strings.Repeat("repair\n", implementations)), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					reviews++
					if reviews == 1 {
						a.JSON = secondReviewResponse("security", "最初の問題")
					}
					if reviews == 2 {
						a.JSON = secondReviewResponse(kind, "新しい重大指摘")
					}
				}
				return a, err
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || reviews != 3 || implementations != 3 {
				t.Fatalf("state=%s reviews=%d implementations=%d err=%v", state.Status, reviews, implementations, err)
			}
		})
	}
}

func TestSecondReviewSavedDeliveryJSON(t *testing.T) {
	const saved = `{"decision":"hold","deferredReviewItems":[{"problem":"保存済み","expected":"期待","test":"確認"}],"deferredReviewDeliveries":[{"author":"worker","source":"source","part":0,"itemHash":"hash","proposal":{"id":"saved-id","status":"failed","deliveryOutcome":"unknown"}}]}`
	r := &journal.Record{PreviousReview: json.RawMessage(saved)}
	_, raw, _ := decideReview(r, json.RawMessage(`{"decision":"hold"}`))
	var review Review
	if err := json.Unmarshal(raw, &review); err != nil || len(review.DeferredReviewItems) != 1 || len(review.DeferredReviewDeliveries) != 1 || review.DeferredReviewDeliveries[0].Proposal.ID != "saved-id" || review.DeferredReviewDeliveries[0].Proposal.Status != "failed" || review.DeferredReviewDeliveries[0].Proposal.DeliveryOutcome != "unknown" {
		t.Fatalf("saved delivery changed: %+v, %v", review, err)
	}
}
