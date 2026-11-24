package execution

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestExternalCheckWordsAndExplicitClassificationDelivery(t *testing.T) {
	cases := []struct {
		text    string
		deliver bool
	}{
		{"配備後に本番DBの接続を確認する。", false},
		{"配備先の稼働を照合する", false},
		{"本番記録を確かめる", false},
		{"実機で確認する", false},
		{"別PCで確認する", false},
		{"他のPCで確認する", false},
		{"コントローラーが確認する", false},
		{"Gitea APIの状態を確認する", false},
		{"実データを確認する", false},
		{"外部DBを確認する", false},
		{"外部データベースを確認する", false},
		{"外部サービスを確認する", false},
		{"配備先で検証する", false},
		{"配備先で接続試験を行う", false},
		{"外部DBとの接続は実装PCでは未確認。配備後に接続試験を行う", false},
		{"配備後の状態に問題がある", true},
		{"接続を確認する", true},
		{"配備先を確認しコードを直す", true},
		{"配備先を確認し修正する", true},
		{"本番を確認し検査を追加する", true},
		{"本番を確認し設定を変更する", true},
		{"実機で確認し実装する", true},
		{"本番を確認しテストを足す", true},
		{"本番を確認し改修する", true},
		{"本番を確認しコードを作成する", true},
	}
	f := newPipeline(t, "approve")
	var problems, want []string
	for _, c := range cases {
		problems = append(problems, c.text)
		if c.deliver {
			want = append(want, c.text)
		}
	}
	yes, no := true, false
	review := Review{Decision: "approve", Improvements: problems, Findings: []string{"外部の稼働状況", "配備後に本番DBの接続を確認する"}, Kinds: []string{"other", "other"}, Severities: []string{"minor", "minor"}, FindingSpecs: []reviewFindingSpec{{Problem: "外部の稼働状況", Expected: "運用で確認", Test: "作業後に確認", ExternalCheckOnly: &yes}, {Problem: "配備後に本番DBの接続を確認する", Expected: "運用で確認", Test: "配備後に本番で確認", ExternalCheckOnly: &no}}}
	for i := range review.FindingSpecs {
		review.FindingSpecs[i].Expected = fixtureEvidence + "期待される状態"
		review.FindingSpecs[i].Test = fixtureVerification
	}
	for _, problem := range problems {
		review.FindingSpecs = append(review.FindingSpecs, reviewFindingSpec{Problem: problem, Expected: fixtureEvidence + "期待される状態", Test: fixtureVerification})
	}
	want = append(want, "配備後に本番DBの接続を確認する")
	_, raw := normalizeReview(encode(review))
	r := proposalRecord()
	r.Review = raw
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, body := range proposalBodies(t, f) {
		var items []followupProposalItem
		if err := json.Unmarshal(body["items"], &items); err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.ExternalCheckOnly != nil {
				t.Fatal("internal classification leaked into proposal")
			}
			got = append(got, item.Problem)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	var saved Review
	if err := json.Unmarshal(r.Review, &saved); err != nil || len(saved.Improvements) != len(review.Improvements)+2 || saved.ImprovementSpecs[len(saved.ImprovementSpecs)-2].ExternalCheckOnly == nil || !*saved.ImprovementSpecs[len(saved.ImprovementSpecs)-2].ExternalCheckOnly {
		t.Fatalf("lost review: %+v %v", saved, err)
	}
}
