package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/jobspec"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

// This is the shipped provider output shape, including the four kinds reported
// for common issue 4553. improvementSpecs is added only by the Worker.
func minorReview4553() json.RawMessage {
	raw := json.RawMessage(`{"decision":"approve","summary":"軽微な指摘のみ","breaking":false,"tooLarge":false,"findings":["回帰の軽微な指摘","改善A","改善B","受入条件の軽微な指摘"],"findingKinds":["regression","other","other","acceptance"],"findingSeverities":["minor","minor","minor","minor"],"findingSpecs":[{"group":"regression","problem":"回帰の軽微な指摘","expected":"元の動作を保つ","test":"回帰テストを実行"},{"group":"other","problem":"改善A","expected":"Aを改善","test":"Aのテスト"},{"group":"other","problem":"改善B","expected":"Bを改善","test":"Bのテスト"},{"group":"acceptance","problem":"受入条件の軽微な指摘","expected":"表示を改善","test":"表示のテスト"}],"followups":[],"followupSpecs":[],"improvements":["既存の改善"],"postMergeChecks":[],"userJudgments":[],"resolvableUncertainties":["追加確認"]}`)
	var review Review
	_ = json.Unmarshal(raw, &review)
	for i := range review.FindingSpecs {
		review.FindingSpecs[i].Expected = fixtureEvidence + review.FindingSpecs[i].Expected
		review.FindingSpecs[i].Test = fixtureVerification + " " + review.FindingSpecs[i].Test
	}
	for _, problem := range append(review.Improvements, review.ResolvableUncertainties...) {
		item := fixtureProposal(problem)
		review.FindingSpecs = append(review.FindingSpecs, reviewFindingSpec{Problem: problem, Expected: item.Expected, Test: item.Test})
	}
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)
	obj["findingSpecs"] = encode(review.FindingSpecs)
	return encode(obj)
}

func TestRepeatedHeavyFindingExecution(t *testing.T) {
	for _, tc := range []struct {
		name      string
		findings  []string
		severity  string
		want      string
		wantCount int
	}{
		{"SecondReview different", []string{"第一の問題", "第二の問題"}, "must", "merged", 3},
		{"same", []string{"残る問題", "残る問題"}, "must", "held", 2},
		{"minor", []string{"軽い問題"}, "minor", "merged", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			original := f.Executor.Call
			reviews, implementations := 0, 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a, err := original(ctx, req)
				if err != nil {
					return a, err
				}
				if !req.Review {
					implementations++
					if implementations > 1 {
						if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte(strings.Repeat("revision\n", implementations)), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return a, nil
				}
				if reviews == len(tc.findings) && tc.name == "SecondReview different" {
					reviews++
					return a, nil
				}
				if reviews >= len(tc.findings) {
					t.Fatal("unexpected review")
				}
				var v map[string]any
				if err := json.Unmarshal(a.JSON, &v); err != nil {
					t.Fatal(err)
				}
				v["decision"] = "hold"
				v["findings"] = []string{tc.findings[reviews]}
				v["findingKinds"] = []string{"acceptance"}
				v["findingSeverities"] = []string{tc.severity}
				a.JSON = encode(v)
				reviews++
				return a, nil
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != tc.want || reviews != tc.wantCount {
				t.Fatalf("state=%+v err=%v reviews=%d", state, err, reviews)
			}
			if tc.name == "SecondReview different" && (implementations != 3 || !f.merged) {
				t.Fatalf("new must did not return to implementation: implementations=%d merged=%v", implementations, f.merged)
			}
			if tc.want == "held" && (!strings.Contains(string(state.Result), "修正後も残る指摘") || !strings.Contains(string(state.Result), tc.findings[len(tc.findings)-1])) {
				t.Fatal(string(state.Result))
			}
		})
	}
}

func TestRepeatedHeavyFindingMatching(t *testing.T) {
	previous := encode(Review{Findings: []string{" 同じ\n問題 ", "軽い", "種類"}, Kinds: []string{"acceptance", "other", "data"}, Severities: []string{"must", "minor", "must"}})
	current := Review{Findings: []string{"同じ 問題", "軽い", "種類", "新規"}, Kinds: []string{"acceptance", "other", "other", "data"}, Severities: []string{"must", "must", "must", "must"}}
	if got := repeatedHeavyFindings(previous, current); !reflect.DeepEqual(got, []string{"同じ 問題", "種類"}) {
		t.Fatal(got)
	}
	if got := repeatedHeavyFindings(nil, current); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestRepeatedHeavyFindingStructuredProblem(t *testing.T) {
	previous := json.RawMessage(`{"findings":["旧表現", "別問題"],"findingKinds":["regression", "data"],"findingSeverities":["must", "must"],"findingSpecs":[{"group":"regression","problem":"保存時にレコードが欠落する問題"},{"group":"data","problem":"別の欠落"}]}`)
	var current Review
	if err := json.Unmarshal([]byte(`{"findings":["新しい表現", "他の問題"],"findingKinds":["acceptance", "data"],"findingSeverities":["must", "must"],"findingSpecs":[{"group":"acceptance","problem":"保存時にレコードが欠落する問題"},{"group":"data","problem":"異なる問題"}]}`), &current); err != nil {
		t.Fatal(err)
	}
	if got := repeatedHeavyFindings(previous, current); !reflect.DeepEqual(got, []string{"新しい表現"}) {
		t.Fatal(got)
	}
}

func TestShortStructuredProblemDoesNotBlockNewFinding(t *testing.T) {
	prior := secondReviewResponse("acceptance", "保存時のテストが不足している")
	prior = changeSecondReview(prior, "findingSpecs", []reviewFindingSpec{{Problem: "テスト不足"}})
	current := secondReviewResponse("acceptance", "認証時のテストが不足している")
	current = changeSecondReview(current, "findingSpecs", []reviewFindingSpec{{Problem: "テスト不足"}})
	r := proposalRecord()
	r.Head = strings.Repeat("b", 40)
	r.Review = stampReviewEvidence(prior, 1, 1, strings.Repeat("a", 40))
	r.ReviewHolds = 1
	review, _, repeated := decideReview(r, current)
	if review.Approved() || !reflect.DeepEqual(review.Severities, []string{"must"}) || len(repeated) != 0 || len(review.DeferredReviewItems) != 0 {
		t.Fatalf("new finding must require repair without repeated hold: review=%+v repeated=%v", review, repeated)
	}
}

func TestReview4553ApprovesAndPreservesSpecsAcrossNormalization(t *testing.T) {
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(minorReview4553(), &raw)
	var schema struct {
		Required   []string
		Properties map[string]json.RawMessage
	}
	if err := json.Unmarshal(providers.Schema(true), &schema); err != nil {
		t.Fatal(err)
	}
	if err := providers.Validate(minorReview4553(), true); err != nil {
		t.Fatalf("旧レビューの受信: %v", err)
	}
	// 旧レビューを送信用の新契約に拡張して全必須欄を照合する。
	raw["recordOnly"] = json.RawMessage(`[]`)
	raw["previousFindings"] = json.RawMessage(`[]`)
	raw["interrupts"] = json.RawMessage(`[]`)
	for _, key := range schema.Required {
		if raw[key] == nil {
			t.Fatalf("shipped schema field missing: %s", key)
		}
	}
	for key := range raw {
		if schema.Properties[key] == nil {
			t.Fatalf("unknown provider field: %s", key)
		}
	}
	var original Review
	if err := json.Unmarshal(minorReview4553(), &original); err != nil || !original.Approved() {
		t.Fatal(original, err)
	}
	review, normalized := normalizeReview(minorReview4553())
	if !review.Approved() || len(review.Findings) != 0 || len(review.FindingSpecs) != 0 || len(review.Improvements) != 6 {
		t.Fatal(review)
	}
	if review.ImprovementSpecs[1].Expected != fixtureEvidence+"元の動作を保つ" || review.ImprovementSpecs[1].Test != fixtureVerification+" 回帰テストを実行" {
		t.Fatal(review.ImprovementSpecs)
	}
	var reloaded Review
	if err := json.Unmarshal(normalized, &reloaded); err != nil || !reflect.DeepEqual(reviewProposalItems(original), reviewProposalItems(reloaded)) {
		t.Fatal("saved review lost details", err)
	}
	_, again := normalizeReview(normalized)
	if string(again) != string(normalized) {
		t.Fatal("normalization duplicated improvements")
	}
}

func TestReviewDecisionRetainsBlockingBoundaries(t *testing.T) {
	for _, kind := range []string{"regression", "other", "acceptance", "security", "data", "blocking", "unknown", ""} {
		for _, severity := range []string{"minor", "must", "", "blocking"} {
			r := Review{Decision: "approve", Findings: []string{"指摘"}, Kinds: []string{kind}, Severities: []string{severity}}
			want := severity == "minor" && (kind == "regression" || kind == "other" || kind == "acceptance")
			if r.Approved() != want {
				t.Fatalf("%s/%s: %v", kind, severity, r)
			}
		}
	}
	for _, mutate := range []func(*Review){
		func(r *Review) { r.Breaking = true }, func(r *Review) { r.TooLarge = true },
		func(r *Review) { r.UserJudgments = []string{"判断"} }, func(r *Review) { r.Uncertainties = []string{"未確認"} },
		func(r *Review) { r.Kinds = nil }, func(r *Review) { r.Severities = nil },
	} {
		var r Review
		_ = json.Unmarshal(minorReview4553(), &r)
		mutate(&r)
		if r.Approved() {
			t.Fatal(r)
		}
		_, raw := normalizeReview(encode(r))
		var held Review
		_ = json.Unmarshal(raw, &held)
		if len(held.Findings) != 4 {
			t.Fatal("held findings were removed")
		}
	}
}

func TestExternalCheckOnlyFixedReviewDecision(t *testing.T) {
	const raw = `{"decision":"approve","summary":"外部DBの接続は未確認","breaking":false,"tooLarge":false,"findings":[],"findingKinds":[],"findingSeverities":[],"findingSpecs":[],"followups":[],"followupSpecs":[],"improvements":["外部DBとの接続は実装PCでは未確認。配備後に接続試験を行う"],"postMergeChecks":[],"userJudgments":[],"resolvableUncertainties":[],"recordOnly":[]}`
	for _, tc := range []struct {
		decision string
		approved bool
	}{{"approve", true}, {"hold", false}} {
		t.Run(tc.decision, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				t.Fatal(err)
			}
			value["decision"] = tc.decision
			input := encode(value)
			if err := providers.Validate(input, true); err != nil {
				t.Fatal(err)
			}
			review, saved := normalizeReview(input)
			if review.Approved() != tc.approved || string(saved) != string(input) || len(review.Improvements) != 1 {
				t.Fatalf("review=%+v saved=%s", review, saved)
			}
		})
	}
}

func TestSameHeadResumeAfterUserJudgmentReturnsToImplementation(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	original := f.Executor.Call
	reviews, implementations := 0, 0
	var reviewHeads []string
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		a, err := original(ctx, req)
		if err != nil {
			return a, err
		}
		if !req.Review {
			implementations++
			if implementations > 1 {
				if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte("review repair\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			return a, nil
		}
		reviews++
		reviewHeads = append(reviewHeads, f.git(req.Policy.Tree, "rev-parse", "HEAD"))
		if reviews <= 2 {
			var v map[string]any
			if err := json.Unmarshal(a.JSON, &v); err != nil {
				t.Fatal(err)
			}
			v["decision"] = "hold"
			v["findings"] = []string{"同じ重大な問題"}
			v["findingKinds"] = []string{"acceptance"}
			v["findingSeverities"] = []string{"must"}
			if reviews == 1 {
				v["userJudgments"] = []string{"判断が必要"}
			} else {
				v["userJudgments"] = []string{}
			}
			a.JSON = encode(v)
		}
		return a, nil
	}
	ctx := context.Background()
	first, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || first.Status != "held" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	head := saved.Head
	if head == "" || saved.ReviewHead != head {
		t.Fatalf("head=%q reviewHead=%q", head, saved.ReviewHead)
	}
	saved.FinalReported = true
	if err := f.Executor.Store.Save(saved); err != nil {
		t.Fatal(err)
	}
	setGeneration(t, f, 0, map[string]any{"instructions": []map[string]string{{"id": "human:answer", "text": "判断に回答"}}})
	second, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || second.Status != "merged" || implementations != 2 || reviews != 3 {
		t.Fatalf("second=%+v err=%v implementations=%d reviews=%d", second, err, implementations, reviews)
	}
	if reviewHeads[0] != reviewHeads[1] || reviewHeads[1] == reviewHeads[2] {
		t.Fatalf("review heads=%v", reviewHeads)
	}
}

func carrySecondReviewJob(t *testing.T, f *pipelineFixture, state intake.State) {
	t.Helper()
	nextGeneration(t, f, state)
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.Branch, d.HeadSHA = state.Branch, state.HeadSHA
	if state.PR != nil {
		d.PR = *state.PR
	}
	f.Job.Raw = encode(d)
}

func TestSecondReviewReturnToOldPC(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	original := f.Executor.Call
	reviews, implementations := 0, 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		a, err := original(ctx, req)
		if !req.Review {
			implementations++
			if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte(strings.Repeat("revision\n", implementations)), 0600); err != nil {
				t.Fatal(err)
			}
			return a, err
		}
		reviews++
		switch reviews {
		case 1:
			a.JSON = changeSecondReview(secondReviewResponse("security", "PC Aの古い問題"), "breaking", true)
		case 2:
			a.JSON = secondReviewResponse("data", "PC Bの重大指摘")
			a.JSON = changeSecondReview(a.JSON, "findings", []string{"PC Bの重大指摘", "PC Bの後続案"})
			a.JSON = changeSecondReview(a.JSON, "findingKinds", []string{"data", "other"})
			a.JSON = changeSecondReview(a.JSON, "findingSeverities", []string{"must", "must"})
			a.JSON = changeSecondReview(a.JSON, "findingSpecs", []reviewFindingSpec{{Problem: "PC Bの重大指摘"}, {Problem: "PC Bの後続案", Expected: fixtureEvidence + "Bの期待", Test: fixtureVerification + " Bの検査"}})
			a.JSON = changeSecondReview(a.JSON, "breaking", true)
		case 3:
			// A changed kind must still match B's latest heavy finding.
			a.JSON = secondReviewResponse("acceptance", "PC Bの重大指摘")
		}
		return a, err
	}
	ctx := context.Background()
	state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || reviews != 1 {
		t.Fatal(state, err, reviews)
	}
	r, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	r.FinalReported = true
	// Retain A's journal, including both an old Review and PreviousReview.
	r.PreviousReview = r.Review
	oldA := encode(r)
	carrySecondReviewJob(t, f, state)
	journalPath := filepath.Join(f.Executor.Store.Root, "state", "job-1.json")
	if err := os.Remove(journalPath); err != nil {
		t.Fatal(err)
	}
	state, err = f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || reviews != 2 || len(proposalBodies(t, f)) != 0 {
		t.Fatal(state, err, reviews)
	}
	var carry map[string]json.RawMessage
	_ = json.Unmarshal(state.Result, &carry)
	carry["review"] = changeSecondReview(carry["review"], "deferredReviewItems", []followupProposalItem{{Problem: "PC Bの後続案", Expected: fixtureEvidence + "Bの期待", Test: fixtureVerification + " Bの検査"}})
	state.Result = encode(carry)
	carrySecondReviewJob(t, f, state)
	// Move the job back to the PC with A's stale journal still on disk.
	if err := os.WriteFile(journalPath, oldA, 0600); err != nil {
		t.Fatal(err)
	}
	state, err = f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || state.HoldReason != "review_held" || reviews != 3 || implementations != 3 || len(proposalBodies(t, f)) != 0 {
		t.Fatalf("latest finding must remain heavy: state=%+v err=%v reviews=%d implementations=%d", state, err, reviews, implementations)
	}
	r, err = f.Executor.Store.Load(1)
	var review Review
	if err != nil || json.Unmarshal(r.Review, &review) != nil || !reflect.DeepEqual(review.Severities, []string{"must"}) {
		t.Fatal(r, err)
	}
	carrySecondReviewJob(t, f, state)
	state, err = f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || reviews != 4 {
		t.Fatal(state, err, reviews)
	}
	bodies := proposalBodies(t, f)
	var items []followupProposalItem
	want := []followupProposalItem{{Problem: "PC Bの後続案", Expected: fixtureEvidence + "Bの期待", Test: fixtureVerification + " Bの検査"}}
	if len(bodies) != 1 || json.Unmarshal(bodies[0]["items"], &items) != nil || !reflect.DeepEqual(items, want) {
		t.Fatalf("lost/duplicated B proposal: %v", bodies)
	}
}

func TestSecondReviewHeadEvidenceExecution(t *testing.T) {
	for _, path := range []string{"carried different head", "saved same head before ReviewHead update"} {
		t.Run(path, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			original := f.Executor.Call
			reviews, implementations := 0, 0
			var reviewHeads []string
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a, err := original(ctx, req)
				if !req.Review {
					implementations++
					if implementations > 1 {
						if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte("repair"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return a, err
				}
				reviews++
				reviewHeads = append(reviewHeads, f.git(req.Policy.Tree, "rev-parse", "HEAD"))
				if reviews <= 2 {
					a.JSON = secondReviewResponse("acceptance", "残る同一指摘")
					if reviews == 1 {
						a.JSON = changeSecondReview(a.JSON, "userJudgments", []string{"判断待ち"})
					}
				}
				return a, err
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "held" || reviews != 1 {
				t.Fatal(state, err, reviews)
			}
			if path == "carried different head" {
				carrySecondReviewJob(t, f, state)
				if err := os.Remove(filepath.Join(f.Executor.Store.Root, "state", "job-1.json")); err != nil {
					t.Fatal(err)
				}
			} else {
				r, err := f.Executor.Store.Load(1)
				if err != nil {
					t.Fatal(err)
				}
				// Reproduce saving the new review before updating ReviewHead.
				r.ReviewHead, r.Stage = r.Base, "review"
				r.FinalState = nil
				if err := f.Executor.Store.Save(r); err != nil {
					t.Fatal(err)
				}
				setGeneration(t, f, 0, map[string]any{"instructions": []map[string]string{{"id": "human:answer", "text": "判断に回答"}}})
			}
			state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if path == "carried different head" {
				if state.Status != "held" || state.HoldReason != "review_held" || reviews != 2 || implementations != 2 {
					t.Fatal(state, reviews, implementations)
				}
			} else if state.Status != "merged" || reviews != 3 || implementations != 2 {
				t.Fatalf("same head must return to implementation: %+v reviews=%d implementations=%d", state, reviews, implementations)
			} else if reviewHeads[0] != reviewHeads[1] || reviewHeads[1] == reviewHeads[2] {
				t.Fatalf("review heads=%v", reviewHeads)
			}
		})
	}
}

func TestSecondReviewSameGenerationLeaseEvidence(t *testing.T) {
	for _, newer := range []string{"local", "delivered"} {
		t.Run(newer, func(t *testing.T) {
			f := newPipeline(t, "approve")
			r := proposalRecord()
			r.Generation, r.Epoch, r.ReviewHolds = 2, 9, 1
			f.CurrentLeaseEpoch = r.Epoch
			r.Head = strings.Repeat("c", 40)
			localEpoch, deliveredEpoch := int64(8), int64(7)
			if newer == "delivered" {
				localEpoch, deliveredEpoch = deliveredEpoch, localEpoch
			}
			local := stampReviewEvidence(secondReviewResponse("data", "local must"), 2, localEpoch, strings.Repeat("a", 40))
			delivered := stampReviewEvidence(secondReviewResponse("data", "delivered must"), 2, deliveredEpoch, strings.Repeat("b", 40))
			local = changeSecondReview(local, "deferredReviewItems", []followupProposalItem{{Problem: "local proposal", Expected: fixtureEvidence + "local expected", Test: fixtureVerification + " local test"}})
			delivered = changeSecondReview(delivered, "deferredReviewItems", []followupProposalItem{{Problem: "delivered proposal", Expected: fixtureEvidence + "delivered expected", Test: fixtureVerification + " delivered test"}})
			r.Review, r.PreviousReview = local, local
			d := Descriptor{Descriptor: jobspec.Descriptor{Generation: 2, Result: encode(map[string]any{"review": delivered})}}
			seedPreviousResults(r, d)
			review, saved, repeated := decideReview(r, secondReviewResponse("other", newer+" must"))
			if review.Approved() || !reflect.DeepEqual(repeated, []string{newer + " must"}) {
				t.Fatalf("wrong lease selected: %s repeated=%v", saved, repeated)
			}
			r.Review = saved
			// Re-seeding the descriptor must not discard a new local decision.
			seedPreviousResults(r, d)
			review, _, repeated = decideReview(r, secondReviewResponse("other", newer+" must"))
			if review.Approved() || len(repeated) != 0 {
				t.Fatalf("local same-head decision was replaced by the descriptor: %+v repeated=%v", review, repeated)
			}
			_, r.Review, _ = decideReview(r, encode(map[string]any{"decision": "approve", "findings": []string{}, "findingKinds": []string{}, "findingSeverities": []string{}}))
			if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			bodies := proposalBodies(t, f)
			var items []followupProposalItem
			want := []followupProposalItem{{Problem: "local proposal", Expected: fixtureEvidence + "local expected", Test: fixtureVerification + " local test"}, {Problem: "delivered proposal", Expected: fixtureEvidence + "delivered expected", Test: fixtureVerification + " delivered test"}}
			if len(bodies) != 1 || json.Unmarshal(bodies[0]["items"], &items) != nil || !reflect.DeepEqual(items, want) {
				t.Fatal(bodies)
			}
			savedRecord, err := f.Executor.Store.Load(r.ID)
			if err != nil || len(savedRecord.FollowupProposals) != 1 || savedRecord.FollowupProposals[0].Status != "sent" {
				t.Fatal(savedRecord, err)
			}
		})
	}
}

func TestPreviousFindingsParaphraseDecision(t *testing.T) {
	for _, kind := range []string{"acceptance", "other"} {
		t.Run(kind, func(t *testing.T) {
			r := &journal.Record{Review: secondReviewResponse(kind, "前の表現"), ReviewHead: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), ReviewHolds: 1}
			raw := changeSecondReview(secondReviewResponse(kind, "別の表現"), "previousFindings", []any{map[string]any{"index": 0, "status": "unresolved", "current": 0}})
			review, _, repeated := decideReview(r, raw)
			if review.Approved() || len(review.Severities) != 1 || review.Severities[0] != "must" || !reflect.DeepEqual(repeated, []string{"別の表現"}) {
				t.Fatalf("review=%+v repeated=%v", review, repeated)
			}
		})
	}
}

func TestPreviousFindingsFallback(t *testing.T) {
	r := &journal.Record{Review: secondReviewResponse("other", "前の表現"), ReviewHead: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), ReviewHolds: 1}
	for _, links := range []any{nil, []any{map[string]any{"index": 1, "status": "unresolved", "current": 0}}, []any{map[string]any{"index": 0, "status": "unresolved", "current": 2}}, []any{map[string]any{"index": 0, "status": "unresolved", "current": 0}, map[string]any{"index": 0, "status": "unresolved", "current": 0}}} {
		raw := secondReviewResponse("other", "別の表現")
		if links != nil {
			raw = changeSecondReview(raw, "previousFindings", links)
		}
		review, _, repeated := decideReview(r, raw)
		if review.Approved() || !reflect.DeepEqual(review.Severities, []string{"must"}) || len(repeated) != 0 {
			t.Fatalf("links=%v review=%+v repeated=%v", links, review, repeated)
		}
	}
	minor := changeSecondReview(secondReviewResponse("other", "別の表現"), "findingSeverities", []string{"minor"})
	minor = changeSecondReview(minor, "previousFindings", []any{map[string]any{"index": 0, "status": "unresolved", "current": 0}})
	review, _, repeated := decideReview(r, minor)
	if !review.Approved() || len(repeated) != 0 {
		t.Fatalf("minor link: %+v %v", review, repeated)
	}
	raw := changeSecondReview(secondReviewResponse("other", "前の表現"), "previousFindings", []any{map[string]any{"index": 0, "status": "resolved", "current": nil}})
	_, _, repeated = decideReview(r, raw)
	if len(repeated) != 1 {
		t.Fatalf("resolved suppressed match: %v", repeated)
	}
}

func findingsInReviewRequest(t *testing.T, prompt string) []previousFinding {
	t.Helper()
	marker := "前回の重大指摘一覧（データ）: "
	start := strings.Index(prompt, marker)
	if start < 0 {
		t.Fatal("missing list")
	}
	var refs []previousFindingReference
	if err := json.Unmarshal([]byte(strings.SplitN(prompt[start+len(marker):], "\n", 2)[0]), &refs); err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(prompt, "根拠(データ): ", 2)
	if len(parts) != 2 {
		t.Fatal("missing evidence")
	}
	var evidence struct {
		PreviousReview json.RawMessage `json:"previousReview"`
	}
	if err := json.NewDecoder(strings.NewReader(parts[1])).Decode(&evidence); err != nil {
		t.Fatal(err)
	}
	var prior Review
	_ = json.Unmarshal(evidence.PreviousReview, &prior)
	list := make([]previousFinding, 0, len(refs))
	for _, ref := range refs {
		if ref.Source < 0 || ref.Source >= len(prior.Findings) || ref.Source >= len(prior.Kinds) || ref.Source >= len(prior.Severities) {
			t.Fatalf("invalid reference: %+v", ref)
		}
		problem := ""
		if ref.Source < len(prior.FindingSpecs) {
			problem = prior.FindingSpecs[ref.Source].Problem
		}
		list = append(list, previousFinding{Index: ref.Index, Source: ref.Source, Finding: prior.Findings[ref.Source], Kind: prior.Kinds[ref.Source], Severity: prior.Severities[ref.Source], Problem: problem})
	}
	return list
}

func TestPreviousFindingsParaphraseExecution(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
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
		if reviews == 1 {
			a.JSON = secondReviewResponse("acceptance", "前の表現")
		} else {
			list := findingsInReviewRequest(t, req.Prompt)
			if len(list) != 1 || list[0].Finding != "前の表現" {
				t.Fatalf("list=%v", list)
			}
			a.JSON = changeSecondReview(secondReviewResponse("acceptance", "別の表現"), "previousFindings", []any{map[string]any{"index": list[0].Index, "status": "unresolved", "current": 0}})
		}
		return a, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || state.HoldReason != "review_held" || reviews != 2 || implementations != 2 || f.merged {
		t.Fatalf("state=%+v err=%v reviews=%d implementations=%d merged=%v", state, err, reviews, implementations, f.merged)
	}
}

func TestPreviousFindingsReviewRequest(t *testing.T) {
	prior := secondReviewResponse("other", "重大")
	prior = changeSecondReview(prior, "findings", []string{"軽微", "重大"})
	prior = changeSecondReview(prior, "findingKinds", []string{"other", "other"})
	prior = changeSecondReview(prior, "findingSeverities", []string{"minor", "must"})
	prior = changeSecondReview(prior, "findingSpecs", []reviewFindingSpec{{Problem: "軽微"}, {Problem: "重大"}})
	multi := changeSecondReview(prior, "findings", []string{"軽微", "重大", "保護指摘"})
	multi = changeSecondReview(multi, "findingKinds", []string{"other", "other", "security"})
	multi = changeSecondReview(multi, "findingSeverities", []string{"minor", "must", "minor"})
	multi = changeSecondReview(multi, "findingSpecs", []reviewFindingSpec{{Problem: "軽微"}, {Problem: "重大"}, {Problem: "保護問題"}})
	for _, tc := range []struct {
		name  string
		prior json.RawMessage
		count int
	}{{"first", nil, 0}, {"invalid", json.RawMessage(`{"omitted":true}`), 0}, {"comparable", prior, 1}, {"multiple", multi, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			head := strings.Repeat("a", 40)
			r := &journal.Record{Review: tc.prior, ReviewHead: head, Head: head, ReviewHolds: 1}
			prompt := reviewPrompt(Descriptor{}, "", "", r, gitwork.Snapshot{}, "")
			list := findingsInReviewRequest(t, prompt)
			if len(list) != tc.count {
				t.Fatalf("list=%v", list)
			}
			if tc.count == 2 {
				if list[0].Index != 0 || list[0].Source != 1 || list[1].Index != 1 || list[1].Source != 2 || list[1].Finding != "保護指摘" || list[1].Kind != "security" || list[1].Severity != "minor" || list[1].Problem != "保護問題" {
					t.Fatal(list)
				}
				raw := changeSecondReview(secondReviewResponse("other", "新しい表現"), "previousFindings", []any{map[string]any{"index": list[1].Index, "status": "unresolved", "current": 0}})
				review, _, repeated := decideReview(r, raw)
				if review.Approved() || len(repeated) != 0 {
					t.Fatalf("review=%+v repeated=%v", review, repeated)
				}
			}
			if tc.count == 1 {
				if list[0].Index != 0 || list[0].Source != 1 || list[0].Finding != "重大" || list[0].Kind != "other" || list[0].Severity != "must" || list[0].Problem != "重大" {
					t.Fatal(list)
				}
				raw := changeSecondReview(secondReviewResponse("other", "言い換え"), "previousFindings", []any{map[string]any{"index": list[0].Index, "status": "unresolved", "current": 0}})
				review, _, repeated := decideReview(r, raw)
				if review.Approved() || len(repeated) != 0 {
					t.Fatalf("review=%+v repeated=%v", review, repeated)
				}
			}
		})
	}
}

func TestReviewDecisionInterruptNormalization(t *testing.T) {
	base := Review{Decision: "approve", Findings: []string{"軽微", "重大", "安全", "データ", "停止"}, Kinds: []string{"other", "other", "security", "data", "blocking"}, Severities: []string{"minor", "must", "minor", "minor", "minor"}, Improvements: []string{"改善", "記録", "重複"}, ResolvableUncertainties: []string{"未解決", "重複"}, RecordOnly: []string{"記録"}, UserJudgments: []string{"判断"}}
	base.Interrupts = []reviewInterrupt{{"軽微", " 理由 "}, {"改善", strings.Repeat("あ", 500)}, {"未解決", "理由"}, {"軽微 ", "理由"}, {"重大", "理由"}, {"安全", "理由"}, {"データ", "理由"}, {"停止", "理由"}, {"記録", "理由"}, {"判断", "理由"}, {"重複", "理由"}, {"無関係", "理由"}}
	_, raw := normalizeReview(encode(base))
	var got Review
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := []reviewInterrupt{{"軽微", "理由"}, {"改善", strings.Repeat("あ", 500)}, {"未解決", "理由"}}
	if !reflect.DeepEqual(got.Interrupts, want) {
		t.Fatalf("interrupts = %#v", got.Interrupts)
	}
	for _, reason := range []string{"", " \t", strings.Repeat("あ", 501)} {
		r := Review{Decision: "approve", Improvements: []string{"改善"}, Interrupts: []reviewInterrupt{{"改善", reason}}}
		_, saved := normalizeReview(encode(r))
		var out Review
		_ = json.Unmarshal(saved, &out)
		if len(out.Interrupts) != 0 {
			t.Fatalf("invalid reason accepted: %q", reason)
		}
	}
	r := Review{Decision: "approve", Findings: []string{"軽微"}, Kinds: []string{"other"}, Severities: []string{"minor"}, Interrupts: []reviewInterrupt{{"軽微", "理由"}}}
	_, saved := normalizeReview(encode(r))
	var out Review
	_ = json.Unmarshal(saved, &out)
	if len(out.Findings) != 0 || !reflect.DeepEqual(out.Improvements, []string{"軽微"}) || !reflect.DeepEqual(out.Interrupts, r.Interrupts) {
		t.Fatalf("normalized: %+v", out)
	}
	_, again := normalizeReview(saved)
	if string(saved) != string(again) {
		t.Fatalf("normalization is not stable: %s\n%s", saved, again)
	}
	r.Interrupts = append(r.Interrupts, r.Interrupts[0])
	_, saved = normalizeReview(encode(r))
	_ = json.Unmarshal(saved, &out)
	if len(out.Interrupts) != 0 {
		t.Fatal("duplicate requests accepted")
	}
}
