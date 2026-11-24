package execution

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWithIncompleteAddsReasonCompletedAndRemaining(t *testing.T) {
	raw := json.RawMessage(`{"reason":"仕様マニフェストの生成が終わらない","completed":["検索","詳細"],"remaining":["高速化","固定テスト"]}`)
	got := withIncomplete("一部実装した", raw)
	for _, want := range []string{"一部実装した", "未完了の理由: 仕様マニフェストの生成が終わらない", "完了した部分: 検索、詳細", "残りの作業: 高速化、固定テスト"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q が要約にありません: %q", want, got)
		}
	}
}
func TestWithIncompleteKeepsSummaryWhenNothingToAdd(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`{}`), json.RawMessage(`{"reason":" ","completed":null,"remaining":[""]}`), json.RawMessage(`[1]`)} {
		if got := withIncomplete("要約", raw); got != "要約" {
			t.Errorf("%s: 要約が変わりました: %q", raw, got)
		}
	}
}
func TestWithIncompleteSurvivesTheSummaryLimit(t *testing.T) {
	raw := json.RawMessage(`{"reason":"理由","completed":null,"remaining":["残り"]}`)
	got := clip(withIncomplete(strings.Repeat("あ", 5000), raw), 2000)
	if !strings.Contains(got, "未完了の理由: 理由") || !strings.Contains(got, "残りの作業: 残り") {
		t.Fatalf("長い要約で理由が切れました: %d文字", len([]rune(got)))
	}
}
