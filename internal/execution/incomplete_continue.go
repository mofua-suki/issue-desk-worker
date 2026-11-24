package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-worker/internal/journal"
	"strings"
	"time"
)

// Retain 20% of the configured weighted-token budget for the extra call. The
// same weights are used by recordUsage; the Server remains authoritative and
// can refuse even when this conservative local reservation is available.
const incompleteUsageFraction = 0.8

func beginIncompleteRun(r *journal.Record, now time.Time) {
	if run := r.IncompleteRun; run != nil && run.Generation == r.Generation && run.Run == r.ReviewReworkRuns {
		return
	}
	r.IncompleteRun = &journal.IncompleteRun{Generation: r.Generation, Run: r.ReviewReworkRuns, StartedAt: now.UTC()}
}

func incompleteEligible(e *Executor, d Descriptor, r *journal.Record, now time.Time) bool {
	run := r.IncompleteRun
	if run == nil || run.Generation != d.Generation || run.Run != r.ReviewReworkRuns || run.StartedAt.IsZero() || run.Attempted || now.Before(run.StartedAt) || e.Config.JobTimeoutSeconds <= 0 || now.Sub(run.StartedAt) >= time.Duration(e.Config.JobTimeoutSeconds)*time.Second/2 {
		return false
	}
	var result Implementation
	if json.Unmarshal(r.Result, &result) != nil || result.Status == "completed" || len(result.Questions) != 0 || hasJSON(result.PrerequisiteMissing) {
		return false
	}
	var incomplete struct {
		Remaining []string `json:"remaining"`
	}
	if json.Unmarshal(result.Incomplete, &incomplete) != nil || joinItems(incomplete.Remaining) == "" {
		return false
	}
	limit := e.Config.Assignments.ImplementationUsageLimit
	if limit <= 0 || r.WeightedUsage < 0 || r.WeightedUsage >= float64(limit)*incompleteUsageFraction {
		return false
	}
	// An absent token count makes the locally calculated total incomplete.
	for _, raw := range r.Usage {
		var row struct {
			Input  *int64 `json:"inputTokens"`
			Output *int64 `json:"outputTokens"`
		}
		if json.Unmarshal(raw, &row) != nil || row.Input == nil || row.Output == nil {
			return false
		}
	}
	return true
}

// continueIncomplete saves the consumed slot before dispatch. A crash leaves the
// original report and the consumed slot together, so replay cannot dispatch it.
func (e *Executor) continueIncomplete(ctx context.Context, d Descriptor, r *journal.Record, prompt string, now func() time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !incompleteEligible(e, d, r, now()) {
		return false, nil
	}
	if err := e.flushUsage(ctx, r); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
	}
	// Automatic checking does not create a new admission for this same run.
	if err := e.checkImplementationUsage(ctx, d, r, true, false); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
	}
	if !incompleteEligible(e, d, r, now()) {
		return false, nil
	}
	r.IncompleteRun.Attempted = true
	if err := e.checkpoint(ctx, d, r, r.Stage); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Check time again after the checkpoint, which also fences the lease epoch.
	if now().Before(r.IncompleteRun.StartedAt) || now().Sub(r.IncompleteRun.StartedAt) >= time.Duration(e.Config.JobTimeoutSeconds)*time.Second/2 {
		return false, nil
	}
	answer, callErr := e.callAI(ctx, d, r, prompt+"\n前回の実装結果(データ): "+string(r.Result)+"\n残りの作業を実施して完了させてください。判断が必要な場合、選ぶ案が新たに必要とし依頼本文が指示していない、データ・ファイル・枝などを消して戻せない操作、権限・認証・資格情報の範囲を広げる変更、費用が上限を超える操作の3種だけは質問して利用者を待ちます。それ以外は目的と受入条件に最も合う案を自分で選び、選んだ案・ほかの案・理由を報告します。既存の保護・個別承認や実装回数・使用量上限は解除しません。", false, nil)
	savedReport := callErr == nil && ctx.Err() == nil
	if savedReport {
		if err := e.receiveImplementationReport(r, answer.JSON, r.Stage); err != nil {
			return true, err
		}
	}
	if err := e.flushUsage(ctx, r); err != nil {
		return true, err
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if callErr != nil {
		return true, fmt.Errorf("未完了作業の続行に失敗しました: %w", callErr)
	}
	return true, nil
}

func incompleteHoldSummary(r *journal.Record, result Implementation) string {
	base := result.Summary
	if len(result.Questions) > 0 {
		base = clip(base, 100)
	}
	summary := withIncomplete(base, result.Incomplete)
	if len(result.Questions) > 0 {
		summary += "\n" + clip(strings.Join(result.Questions, "\n"), 300)
	}
	if run := r.IncompleteRun; run != nil && run.Generation == r.Generation && run.Run == r.ReviewReworkRuns && run.Attempted {
		summary = "続行を1回求めました。\n" + clip(summary, 1800)
	}
	return summary
}
