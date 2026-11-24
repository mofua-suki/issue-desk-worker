package execution

import (
	"encoding/json"
	"slices"
	"strings"

	"issue-desk-worker/internal/journal"
)

// 実装の世代が始まったきっかけ。受付の使用量の行の generationTrigger の値（受付の
// server/internal/usage/schema.json の generationTriggers）で、手戻りの内訳の集計が行の
// generation ごとに読む。最初の世代は記録しない（行は null）。独立レビューの差し戻しからの
// 再実装は行の generation を進めないので、その世代のきっかけを変えない。
var reworkFindingKinds = []string{"data", "security", "regression", "acceptance", "blocking", "other"}

var autoRetryPrefixes = []string{"environment-retry:", "permission-retry:", "usage-limit-retry:", "provider-switch:", "conflict-from-base:"}

// newGenerationTrigger は受付が渡した世代のきっかけを、前の世代の最後の報告と受付の view から決める。
// 判定の順: 受付の取り直しの再開（view.retryRetest がこの世代）→ 自動差し戻しの指示 → 自動再試行の指示・基点からの再試行 → 前の報告がレビュー保留 →
// 前の報告が質問 → 前の報告が失敗 → どれでもなければ unknown（完了後の追加の指示・停止後の
// 指示付きの再開など）。手元の記録が直前の世代のものならその最後の状態を、そうでなければ
// （別のPCで動いた世代・新しい記録）受付が渡す前の報告を使う。
func newGenerationTrigger(d Descriptor, r *journal.Record) *journal.GenerationTrigger {
	if d.Generation <= 0 {
		return nil
	}
	t := &journal.GenerationTrigger{Generation: d.Generation}
	consecutive := r.Generation == d.Generation-1
	var ids []string
	for _, key := range []string{"pending", "instructions"} {
		for _, entry := range field[[]struct {
			ID string `json:"id"`
		}](d.View, key) {
			ids = append(ids, entry.ID)
		}
	}
	var reported struct {
		Status string          `json:"status"`
		Review json.RawMessage `json:"review"`
	}
	if hasJSON(d.Result) {
		_ = json.Unmarshal(d.Result, &reported)
	}
	var retest struct {
		Generation *int `json:"generation"`
	}
	if json.Unmarshal(d.View["retryRetest"], &retest) == nil && retest.Generation != nil && *retest.Generation == d.Generation {
		t.Trigger = "retest"
		return t
	}
	if slices.ContainsFunc(ids, func(id string) bool { return strings.HasPrefix(id, "review:") }) {
		t.Trigger = "review-return"
		// 継続の準備で前の世代のレビューは PreviousReview へ移っていることがある。
		review := reported.Review
		if consecutive && hasJSON(r.Review) {
			review = r.Review
		} else if consecutive && hasJSON(r.PreviousReview) {
			review = r.PreviousReview
		}
		var kinds struct {
			Kinds []string `json:"findingKinds"`
		}
		if hasJSON(review) && json.Unmarshal(review, &kinds) == nil {
			for _, kind := range kinds.Kinds {
				if slices.Contains(reworkFindingKinds, kind) {
					t.FindingKinds = append(t.FindingKinds, kind)
				}
			}
		}
		return t
	}
	var base struct {
		Generation *int `json:"generation"`
	}
	if slices.ContainsFunc(ids, func(id string) bool {
		return slices.ContainsFunc(autoRetryPrefixes, func(prefix string) bool { return strings.HasPrefix(id, prefix) })
	}) || json.Unmarshal(d.View["retryFromBase"], &base) == nil && base.Generation != nil && *base.Generation == d.Generation {
		t.Trigger = "retry"
		return t
	}
	var state struct {
		Status     string `json:"status"`
		HoldReason string `json:"holdReason"`
	}
	if !consecutive || !hasJSON(r.FinalState) || json.Unmarshal(r.FinalState, &state) != nil {
		state.Status = reported.Status
	}
	switch {
	case state.Status == "held" && state.HoldReason == "review_held":
		t.Trigger = "hold-instruction"
	case state.Status == "held" && state.HoldReason == "question":
		t.Trigger = "answer"
	case state.Status == "failed":
		t.Trigger = "retry"
	default:
		t.Trigger = "unknown"
	}
	return t
}

// addGenerationTrigger は実装・競合解消の使用量の行にきっかけと指摘の種類を付ける。他の工程の行には付けない。
// 集計は行の generation ごとにまとめるので、再開や除外で行の generation が進まない配布世代は、
// その行の generation で最初に付けたきっかけを保つ。今の世代の記録が無ければ（最初の世代）どちらも null。
func addGenerationTrigger(row map[string]any, d Descriptor, r *journal.Record, step string) {
	if step != "実装" && step != "競合解消" {
		return
	}
	row["generationTrigger"], row["reviewFindingKinds"] = nil, nil
	generation, _ := row["generation"].(int)
	if r.UsageTrigger == nil || r.UsageTrigger.Generation != generation {
		r.UsageTrigger = &journal.GenerationTrigger{Generation: generation}
		if t := r.GenerationTrigger; t != nil && t.Generation == d.Generation {
			r.UsageTrigger.Trigger, r.UsageTrigger.FindingKinds = t.Trigger, t.FindingKinds
		}
	}
	t := r.UsageTrigger
	if t.Trigger == "" {
		return
	}
	row["generationTrigger"] = t.Trigger
	if t.Trigger == "review-return" && len(t.FindingKinds) > 0 {
		row["reviewFindingKinds"] = t.FindingKinds
	}
}
