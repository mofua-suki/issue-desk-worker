package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"strings"
	"time"
)

func (e *Executor) recordUsage(r *journal.Record, d Descriptor, settings providers.Settings, step string, answer providers.Answer, sizes ...usageSize) error {
	usage := answer.Usage
	valid := func(value *int64) *int64 {
		if value != nil && *value >= 0 {
			return value
		}
		return nil
	}
	usage.Input, usage.Output, usage.Cached, usage.CacheWrite = valid(usage.Input), valid(usage.Output), valid(usage.Cached), valid(usage.CacheWrite)
	usage.Calls, usage.ContextMaxTokens, usage.ContextTotalTokens = valid(usage.Calls), valid(usage.ContextMaxTokens), valid(usage.ContextTotalTokens)
	row := map[string]any{"at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), "host": e.Config.Host, "source": "worker", "repo": d.Repo, "issue": d.Issue, "kind": "normal", "step": step, "provider": settings.Provider, "model": settings.Model, "effort": settings.Effort, "tier": d.Tier, "attempt": 1, "generation": max(r.Runs-nonGenerationRuns(r), 1), "dispatchGeneration": d.Generation, "leaseEpoch": r.Epoch, "outcome": answer.Outcome, "elapsedMs": answer.Elapsed.Milliseconds(), "inputTokens": usage.Input, "outputTokens": usage.Output, "cacheReadTokens": usage.Cached, "cacheWriteTokens": usage.CacheWrite, "usageMissingReason": nil}
	addGenerationTrigger(row, d, r, step)
	for _, size := range sizes {
		size.add(row)
	}
	row["billing"] = "subscription"
	if answer.APIKeyMode {
		row["billing"] = "api_credit"
	}
	row["costUsd"] = nil
	if settings.Provider == "claude" && providers.ValidCostUSD(usage.CostUSD) {
		row["costUsd"] = *usage.CostUSD
	}
	row["calls"], row["contextMaxTokens"], row["contextTotalTokens"] = usage.Calls, usage.ContextMaxTokens, usage.ContextTotalTokens
	if usage.Input == nil || usage.Output == nil {
		row["usageMissingReason"] = "usage-unavailable"
	}
	if usage.Cached == nil && settings.Provider == "codex" {
		row["usagePartial"] = true
	}
	if d.Kind == "followup" {
		row["kind"] = "followup"
	}
	if row["outcome"] == "" {
		row["outcome"] = "failed"
	}
	if usage.Input != nil && usage.Output != nil {
		input := *usage.Input
		cached := int64(0)
		write := int64(0)
		if usage.Cached != nil {
			cached = *usage.Cached
		}
		if usage.CacheWrite != nil {
			write = *usage.CacheWrite
		}
		if settings.Provider == "codex" {
			input -= cached
			if input < 0 {
				input = 0
			}
		}
		r.WeightedUsage += float64(input) + 0.1*float64(cached) + 1.25*float64(write) + 5*float64(*usage.Output)
	}
	r.Usage = append(r.Usage, encode(row))
	r.Limits = append(r.Limits, limitRows(e.Config.Host, answer.Limits)...)
	return e.Store.Save(r)
}

var errUsageRefused = errors.New("使用量の報告が受付に保存されませんでした")

// Each count is bounded before adding, so large responses cannot overflow the sum.
func usageReceipt(body []byte, rows int) (rejected int, valid bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return 0, false
	}
	remaining := rows
	for _, name := range []string{"added", "duplicate", "rejected"} {
		var count *int
		if json.Unmarshal(fields[name], &count) != nil || count == nil || *count < 0 || *count > remaining {
			return 0, false
		}
		remaining -= *count
		if name == "rejected" {
			rejected = *count
		}
	}
	return rejected, remaining == 0
}

func (e *Executor) flushUsage(ctx context.Context, r *journal.Record) error {
	for r.UsageSent < len(r.Usage) {
		end := r.UsageSent + 500
		if end > len(r.Usage) {
			end = len(r.Usage)
		}
		response, err := e.API.Explicit(ctx, "POST", "/api/v1/usage", encode(map[string]any{"usage": r.Usage[r.UsageSent:end], "limits": []any{}}))
		if err != nil {
			return err
		}
		if response.Status < 200 || response.Status >= 300 {
			return fmt.Errorf("%w: HTTP %d", errUsageRefused, response.Status)
		}
		rejected, valid := usageReceipt(response.Body, end-r.UsageSent)
		if !valid {
			return errors.New("使用量報告の応答が不正です")
		}
		if rejected > 0 {
			return fmt.Errorf("%w: 拒否した行 %d", errUsageRefused, rejected)
		}
		r.UsageSent = end
		if err := e.Store.Save(r); err != nil {
			return err
		}
	}
	return e.flushLimits(ctx, r)
}

// usageSize は使用量の行に足す入力の大きさ。受付の使用量の行の検査（issue-desk-intake
// server/internal/usage）が0以上の整数として受け付ける欄だけを書く。負の値は書かない。
type usageSize struct {
	promptBytes, diffFiles, diffLines int
	review                            bool
}

func (s usageSize) add(row map[string]any) {
	if s.promptBytes >= 0 {
		row["promptTotalBytes"] = s.promptBytes
	}
	if s.review && s.diffFiles >= 0 && s.diffLines >= 0 {
		row["reviewDiffFiles"], row["reviewDiffLines"] = s.diffFiles, s.diffLines
	}
}

type reviewDiffSizeKey struct{}

// withReviewDiffSize はレビューで渡す差分のファイル数と変更行数を、呼び出しの使用量の行へ運ぶ。
func withReviewDiffSize(ctx context.Context, paths []string, diff string) context.Context {
	// ファイルの見出し（diff --git から最初の @@ まで）の ---/+++ を除き、ハンクの中の
	// 追加・削除の行だけを数える。中身が "--- " や "+++ " で始まる行も数える。
	lines := 0
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case inHunk && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")):
			lines++
		}
	}
	return context.WithValue(ctx, reviewDiffSizeKey{}, [2]int{len(paths), lines})
}

func callUsageSize(ctx context.Context, prompt string, review bool) usageSize {
	size := usageSize{promptBytes: len(prompt), diffFiles: -1, diffLines: -1, review: review}
	if value, ok := ctx.Value(reviewDiffSizeKey{}).([2]int); ok && review {
		size.diffFiles, size.diffLines = value[0], value[1]
	}
	return size
}
