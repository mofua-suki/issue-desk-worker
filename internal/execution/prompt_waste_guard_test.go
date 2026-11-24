package execution

import (
	"context"
	"encoding/json"
	"testing"

	"issue-desk-worker/internal/providers"
)

// 出荷契約の固定レビューを通常の実行経路に通し、承認後の後続案まで残ることを確認する。
func TestExternalCheckOnlyReviewRemainsRecorded(t *testing.T) {
	f := newPipeline(t, "approve")
	base := f.Executor.Call
	const note = "外部DBとの接続は実装PCでは未確認。配備後に接続試験を行う"
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		a, err := base(ctx, req)
		if req.Review {
			a.JSON = json.RawMessage(`{"decision":"approve","summary":"外部DBの接続は未確認","breaking":false,"tooLarge":false,"findings":[],"findingKinds":[],"findingSeverities":[],"findingSpecs":[],"followups":[],"followupSpecs":[],"improvements":["外部DBとの接続は実装PCでは未確認。配備後に接続試験を行う"],"postMergeChecks":[],"userJudgments":[],"resolvableUncertainties":[]}`)
		}
		return a, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatalf("外部確認だけで停止: %+v %v", state, err)
	}
	r, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	var review Review
	if err := json.Unmarshal(r.Review, &review); err != nil || !review.Approved() || len(review.Improvements) != 1 || review.Improvements[0] != note {
		t.Fatalf("保存レビュー: %+v %v", review, err)
	}
	bodies := proposalBodies(t, f)
	if len(bodies) != 0 {
		t.Fatalf("外部確認の後続案が作成されました: %v", bodies)
	}
}
