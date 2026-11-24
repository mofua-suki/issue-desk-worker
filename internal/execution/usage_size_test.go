package execution

import (
	"context"
	"encoding/json"
	"testing"

	"issue-desk-worker/internal/jobspec"
	"issue-desk-worker/internal/providers"
)

// 独立レビューの使用量の行に、渡した差分のファイル数・変更行数とプロンプトの大きさを残す。
// 受付の集計（行数・ファイル数あたりのレビュー費用）が「不明」にならないようにする。
func TestReviewUsageRecordsDiffAndPromptSize(t *testing.T) {
	s := &limitServer{}
	e, r := limitExecutor(t, s)
	// 中身が "--- " や "++ " で始まる行も変更行として数える。
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,3 @@\n-old\n+new\n context\n--- sql comment\ndiff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -0,0 +1,2 @@\n+added\n+++ plus\n"
	ctx := withReviewDiffSize(context.Background(), []string{"a.go", "b.go"}, diff)
	answer := providers.Answer{Outcome: "completed"}
	if err := e.recordUsage(r, Descriptor{Descriptor: jobspec.Descriptor{Repo: "o/r", Issue: 1}}, providers.Settings{Provider: "claude"}, "独立レビュー", answer, callUsageSize(ctx, "プロンプト", true)); err != nil {
		t.Fatal(err)
	}
	if err := e.recordUsage(r, Descriptor{Descriptor: jobspec.Descriptor{Repo: "o/r", Issue: 1}}, providers.Settings{Provider: "codex"}, "実装", answer, callUsageSize(ctx, "abc", false)); err != nil {
		t.Fatal(err)
	}
	saved, err := e.Store.Load(1)
	if err != nil || len(saved.Usage) != 2 {
		t.Fatal(saved, err)
	}
	var review, implementation map[string]any
	_ = json.Unmarshal(saved.Usage[0], &review)
	_ = json.Unmarshal(saved.Usage[1], &implementation)
	if review["reviewDiffFiles"] != float64(2) || review["reviewDiffLines"] != float64(5) || review["promptTotalBytes"] != float64(len("プロンプト")) {
		t.Fatalf("review row: %v", review)
	}
	if implementation["promptTotalBytes"] != float64(3) || implementation["reviewDiffFiles"] != nil || implementation["reviewDiffLines"] != nil {
		t.Fatalf("implementation row: %v", implementation)
	}
}
