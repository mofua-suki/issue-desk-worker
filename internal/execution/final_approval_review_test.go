package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"issue-desk-worker/internal/providers"
)

func finalApprovalReviewAnswer(f *pipelineFixture, fields map[string]any) {
	original := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := original(ctx, req)
		if err == nil && req.Review {
			var value map[string]any
			_ = json.Unmarshal(answer.JSON, &value)
			value["decision"] = "hold"
			value["userJudgments"] = []string{"マージ前に最終承認が必要"}
			for key, v := range fields {
				value[key] = v
			}
			answer.JSON = encode(value)
		}
		return answer, err
	}
}

func TestFinalApprovalReviewRecordedApproval(t *testing.T) {
	f, head := finalPublicationPipeline(t, "approve")
	finalApprovalReviewAnswer(f, nil)
	state, err := execute(f)
	saved, loadErr := f.Executor.Store.Load(1)
	if err != nil || loadErr != nil || state.Status != "merged" || f.MergePosts != 1 || f.CreatePosts != 0 || strings.Join(f.Calls, ",") != "claude" || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head {
		t.Fatalf("state=%+v err=%v load=%v calls=%v", state, err, loadErr, f.Calls)
	}
	var review map[string]json.RawMessage
	if json.Unmarshal(saved.Review, &review) != nil || !strings.Contains(string(review["postMergeChecks"]), "最終承認") || string(review["decision"]) != `"approve"` {
		t.Fatalf("review=%s", saved.Review)
	}
}

func TestFinalApprovalReviewKeepsOtherHolds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		fields map[string]any
	}{
		{"other judgment", map[string]any{"userJudgments": []string{"最終承認が必要", "別の判断が必要"}}},
		{"breaking", map[string]any{"breaking": true}},
		{"must", map[string]any{"findings": []string{"重大な問題"}, "findingKinds": []string{"security"}, "findingSeverities": []string{"must"}}},
		{"new regression", map[string]any{"findings": []string{"新たな回帰"}, "findingKinds": []string{"regression"}, "findingSeverities": []string{"must"}}},
		{"uncertainty", map[string]any{"uncertainties": []string{"未確認"}}},
		{"too large", map[string]any{"tooLarge": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := finalPublicationPipeline(t, "approve")
			finalApprovalReviewAnswer(f, tc.fields)
			state, err := execute(f)
			saved, loadErr := f.Executor.Store.Load(1)
			if err != nil || loadErr != nil || state.Status != "held" || state.HoldReason != "review_held" || f.MergePosts != 0 {
				t.Fatalf("state=%+v err=%v load=%v", state, err, loadErr)
			}
			var review Review
			if json.Unmarshal(saved.Review, &review) != nil || (tc.name == "other judgment" && (len(review.UserJudgments) != 1 || review.UserJudgments[0] != "別の判断が必要")) {
				t.Fatalf("review=%s", saved.Review)
			}
		})
	}
	t.Run("new must after prior hold", func(t *testing.T) {
		f, head := finalPublicationPipeline(t, "approve")
		var d Descriptor
		if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
			t.Fatal(err)
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(d.Result, &result); err != nil {
			t.Fatal(err)
		}
		result["review"] = stampReviewEvidence(secondReviewResponse("other", "以前の指摘"), d.Generation, 2, head)
		d.Result = encode(result)
		d.View["reviewHolds"] = encode(1)
		f.Job.Raw, f.CurrentView = encode(d), d.View
		finalApprovalReviewAnswer(f, map[string]any{"findings": []string{"新しい問題"}, "findingKinds": []string{"other"}, "findingSeverities": []string{"must"}, "findingSpecs": []reviewFindingSpec{{Group: "other", Problem: "新しい問題", Expected: "修正", Test: "確認"}}})
		state, err := execute(f)
		saved, loadErr := f.Executor.Store.Load(1)
		var review Review
		if err != nil || loadErr != nil || state.Status != "held" || state.HoldReason != "review_held" || f.MergePosts != 0 || json.Unmarshal(saved.Review, &review) != nil || len(review.Severities) != 1 || review.Severities[0] != "must" {
			t.Fatalf("state=%+v err=%v load=%v review=%+v", state, err, loadErr, review)
		}
	})
}

func TestFinalApprovalReviewKeepsNewAcceptanceMust(t *testing.T) {
	f, head := finalPublicationPipeline(t, "approve")
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(d.Result, &result); err != nil {
		t.Fatal(err)
	}
	result["review"] = stampReviewEvidence(secondReviewResponse("other", "以前の指摘"), d.Generation, 2, head)
	d.Result = encode(result)
	d.View["reviewHolds"] = encode(1)
	f.Job.Raw, f.CurrentView = encode(d), d.View
	finalApprovalReviewAnswer(f, map[string]any{"findings": []string{"受入条件を満たさない"}, "findingKinds": []string{"acceptance"}, "findingSeverities": []string{"must"}})
	state, err := execute(f)
	saved, loadErr := f.Executor.Store.Load(1)
	if err != nil || loadErr != nil || saved == nil {
		t.Fatalf("state=%+v err=%v load=%v", state, err, loadErr)
	}
	var review Review
	if json.Unmarshal(saved.Review, &review) != nil || state.Status != "held" || state.HoldReason != "review_held" || f.MergePosts != 0 || len(review.Severities) != 1 || review.Severities[0] != "must" || len(review.Findings) != 1 || review.Findings[0] != "受入条件を満たさない" {
		t.Fatalf("state=%+v review=%s merges=%d", state, saved.Review, f.MergePosts)
	}
}

func TestFinalApprovalReviewRequiresMatchingContinuation(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"false", "missing verified", "missing publication", "head mismatch", "retryFromBase", "reimplementExpected"} {
		t.Run(variant, func(t *testing.T) {
			f, _ := finalPublicationPipeline(t, "approve")
			var d Descriptor
			if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
				t.Fatal(err)
			}
			p := map[string]any{"generation": d.Generation, "branch": d.Branch, "pr": d.PR, "headSha": d.HeadSHA, "verified": true}
			switch variant {
			case "false":
				p["verified"] = false
			case "missing verified":
				delete(p, "verified")
			case "head mismatch":
				p["headSha"] = strings.Repeat("b", 40)
			case "retryFromBase", "reimplementExpected":
				d.View[variant] = encode(map[string]int{"generation": d.Generation})
			}
			if variant == "missing publication" {
				delete(d.View, "continuation")
			} else {
				d.View["continuation"] = encode(map[string]any{"publication": p})
			}
			f.Job.Raw, f.CurrentView = encode(d), d.View
			finalApprovalReviewAnswer(f, nil)
			state, err := execute(f)
			if err != nil || state.Status != "held" || state.HoldReason != "review_held" || f.MergePosts != 0 {
				t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil || saved == nil || !strings.Contains(string(saved.Review), "最終承認") {
				t.Fatalf("review not retained: saved=%+v err=%v calls=%v", saved, err, f.Calls)
			}
		})
	}
}
