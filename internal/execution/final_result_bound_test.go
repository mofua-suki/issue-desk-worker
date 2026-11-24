package execution

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"issue-desk-worker/internal/intake"
)

// 保存済みの最終報告を送り直すときも、状態によらず受付の result 上限に収める。
// 古いWorkerが上限を超えたまま保存した保留・失敗・停止の報告が、断られ続けないようにする。
func TestBoundFinalResultForEveryStatus(t *testing.T) {
	for _, status := range []string{"held", "failed", "stopped", "merged"} {
		t.Run(status, func(t *testing.T) {
			result := encode(map[string]any{"status": status, "summary": "理由", "implementationResult": map[string]any{"detail": strings.Repeat("あ", 40000)}})
			state := intake.State{Status: status, Result: result}
			if err := boundFinalResult(3, &state); err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(state.Result, &decoded); err != nil {
				t.Fatal(err)
			}
			if units := resultUnits(decoded); units > 32768 {
				t.Fatalf("result units %d", units)
			}
			if decoded["summary"] != "理由" || decoded["status"] != status {
				t.Fatalf("summary or status lost: %v", decoded)
			}
		})
	}
}

// 省けない保持レビューだけで上限を超える保留の報告は、実装結果を省かずに従来どおり送る。
// 届かない報告のために、保存する最終状態から実装結果を消さない。
func TestBoundFinalResultKeepsImplementationWhenRetainedReviewOverflows(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	text := make([]byte, 40000)
	for i := range text {
		text[i] = "abcdefghijklmnopqrstuvwxyz0123456789"[rng.Intn(36)]
	}
	review := map[string]any{"decision": "request_changes", "deferredReviewItems": []followupProposalItem{{Problem: string(text), Expected: "期待", Test: "検査"}}}
	result := encode(map[string]any{"status": "held", "summary": "理由", "review": review, "implementationResult": map[string]any{"detail": "質問の全文"}})
	state := intake.State{Status: "held", HoldReason: "review_held", Result: result}
	if err := boundFinalResult(3, &state); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(state.Result, &decoded); err != nil {
		t.Fatal(err)
	}
	if impl, _ := decoded["implementationResult"].(map[string]any); impl["detail"] != "質問の全文" {
		t.Fatalf("implementation result stripped: %v", decoded["implementationResult"])
	}
	merged := intake.State{Status: "merged", Result: result}
	if err := boundFinalResult(3, &merged); err == nil {
		t.Fatal("merged report over the limit must keep the journal and fail")
	}
}
