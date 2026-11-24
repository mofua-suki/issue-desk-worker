package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func (e *Executor) implementationRunLimit(d Descriptor) int {
	limit := e.Config.Assignments.ImplementationRunLimit
	for _, approval := range field[[]struct {
		ID        string `json:"id"`
		Statement string `json:"statement"`
		Runs      int    `json:"implementationRuns"`
	}](d.View, "overLimitApprovals") {
		if approval.ID != "" && approval.Statement != "" && approval.Runs+1 > limit {
			limit = approval.Runs + 1
		}
	}
	return limit
}

// Persist the receipt separately from dispatch. Re-entry resumes this report,
// rather than starting a new implementation or reviving a completion budget.
func (e *Executor) receiveImplementationReport(r *journal.Record, data json.RawMessage, stage string) error {
	r.AppliedEdits = nil
	r.EditRequestedAt = time.Now().UTC()
	r.EditWorkerID = e.Config.Host
	_, r.EditRequestHash, _ = editRequestHash(data)
	r.Result = append(json.RawMessage(nil), data...)
	r.ReportCompletion = &journal.ReportCompletion{ID: providers.UUID(), Generation: r.Generation, Stage: stage, Provider: r.Provider, Session: r.Session, Original: append(json.RawMessage(nil), data...)}
	r.Stage = stage
	return e.Store.Save(r)
}

func (e *Executor) completeImplementationReport(ctx context.Context, d Descriptor, r *journal.Record, remediation bool) (*intake.State, error) {
	stop := func(reason, summary string, view map[string]any) (*intake.State, error) {
		out, err := e.finish(r, "held", reason, summary, view)
		return &out, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reportErr := providers.ValidateReport(r.Result, remediation)
	if reportErr == nil {
		return nil, nil
	}
	// Only a structurally received completed report is eligible.
	if err := providers.Validate(r.Result, false); err != nil {
		return stop("review_held", reportErr.Error(), nil)
	}
	var original Implementation
	_ = json.Unmarshal(r.Result, &original)
	if original.Status != "completed" {
		return stop("review_held", reportErr.Error(), nil)
	}
	receipt := r.ReportCompletion
	if receipt == nil || receipt.Generation != d.Generation || receipt.ID == "" {
		return stop("review_held", reportErr.Error()+"。元報告の保存記録を確認できません", nil)
	}
	if receipt.Attempted {
		summary := reportErr.Error() + "。この報告の補完は試行済みです"
		if receipt.Error != "" {
			summary += ": " + receipt.Error
		}
		return stop("review_held", summary, nil)
	}
	if r.Runs >= e.implementationRunLimit(d) {
		return stop("question", "実装回数の上限に達しました", map[string]any{"implementationRunLimit": true})
	}
	request, err := e.reportCompletionRequest(ctx, d, r, reportErr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return stop("review_held", reportErr.Error()+"。"+err.Error(), nil)
	}
	if err := e.flushUsage(ctx, r); err != nil {
		return nil, err
	}
	if err := e.checkImplementationUsage(ctx, d, r, true, false); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return stop("question", err.Error(), map[string]any{"implementationUsageStop": true})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	receipt.Attempted = true
	r.Runs++
	r.ReportCompletionRuns++
	// Save and report the consumed run before any CLI call, including after a crash.
	if err := e.checkpoint(ctx, d, r, receipt.Stage); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	answer, callErr := e.callReportCompletion(ctx, d, r, request)
	if err := e.flushUsage(ctx, r); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if callErr != nil {
		receipt.Error = callErr.Error()
		if err := e.Store.Save(r); err != nil {
			return nil, err
		}
		var reportOnlyErr *providers.ReportOnlyError
		var exhausted *transientExhausted
		if !errors.As(callErr, &exhausted) && errors.As(callErr, &reportOnlyErr) {
			return stop("review_held", callErr.Error(), nil)
		}
		out, err := e.finish(r, "failed", "", callErr.Error(), nil)
		return &out, err
	}
	merged, err := retainImplementationEdits(receipt.Original, answer.JSON)
	if err != nil {
		return stop("review_held", err.Error(), nil)
	}
	r.Result = merged
	if err := providers.ValidateReport(r.Result, remediation); err != nil {
		receipt.Error = err.Error()
		return stop("review_held", err.Error(), nil)
	}
	var completed Implementation
	_ = json.Unmarshal(r.Result, &completed)
	receipt.Validated = completed.Status == "completed"
	if err := e.Store.Save(r); err != nil {
		return nil, err
	}
	return nil, nil
}

// The report-only response cannot replace the implementation's controller plan.
// Absence in the original (legacy receive contract) also discards new requests.
func retainImplementationEdits(original, supplemented json.RawMessage) (json.RawMessage, error) {
	var before, after map[string]json.RawMessage
	if json.Unmarshal(original, &before) != nil || json.Unmarshal(supplemented, &after) != nil || after == nil {
		return nil, fmt.Errorf("報告補完のJSONが不正です")
	}
	for _, key := range []string{"deleteFiles", "addDependencies", "scopeExtensions"} {
		delete(after, key)
		if value, ok := before[key]; ok {
			after[key] = value
		}
	}
	return encode(after), nil
}

// Unknown controller outcomes are held; replay cannot apply a dependency plan twice.
func (e *Executor) applyCompletedReportRequests(ctx context.Context, r *journal.Record) error {
	receipt := r.ReportCompletion
	if receipt.EditsApplied {
		return nil
	}
	if receipt.EditsStarted {
		return fmt.Errorf("報告補完後の編集要求は反映開始済みで、完了を確認できません")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	receipt.EditsStarted = true
	if err := e.Store.Save(r); err != nil {
		return err
	}
	if err := e.applyImplementationRequests(ctx, r); err != nil {
		return err
	}
	receipt.EditsApplied = true
	if err := e.noteAppliedRequests(r); err != nil {
		return err
	}
	return e.Store.Save(r)
}
