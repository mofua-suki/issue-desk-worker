package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"log"
	"strings"
)

func (e *Executor) checkpoint(ctx context.Context, d Descriptor, r *journal.Record, stage string) error {
	r.Stage = stage
	if err := e.saveStage(r); err != nil {
		return err
	}
	phase := "running"
	if stage == "publish" {
		phase = "publishing"
	}
	if stage == "merge" || stage == "review" {
		phase = "releasing"
	}
	view := map[string]any{"phase": phase, "executionSettings": map[string]string{"provider": r.Provider}}
	e.addRecordView(view, r, stage)
	state := recordState(r, "running", "", r.Head, view, nil)
	e.addDiffFingerprint(ctx, r, &state)
	if stage == "merge" {
		result := map[string]any{"implementationContent": r.Head}
		if hasJSON(r.Result) {
			result["implementationResult"] = json.RawMessage(r.Result)
		}
		if hasJSON(r.Test) {
			result["fixedTest"] = json.RawMessage(r.Test)
		}
		state.Result = e.redactResult(encode(result))
		if err := boundFinalResult(r.Epoch, &state); err != nil {
			return err
		}
	}
	if err := e.API.State(ctx, d.ID, r.Epoch, state); err != nil {
		return err
	}
	if stage == "implement" {
		_, _ = e.issueNotice(ctx, r, "start", fmt.Sprintf("作業を開始しました（実行 %d）。調査・実装・テストを進めます。", r.Generation))
	}
	return nil
}

// addRecordView adds the fields every state the record reports carries in its
// view, for both a checkpoint and the final state.
func (e *Executor) addRecordView(view map[string]any, r *journal.Record, stage string) {
	view["reviewHolds"] = r.ReviewHolds
	view["reviewHoldHead"] = r.ReviewHead
	workerView := workerExecutionView(r, stage)
	e.addCarriedHeldReview(workerView, r)
	view["workerExecution"] = workerView
	providerSwitchView(view, r)
}

// recordState builds the state sent for the record: the view and result as
// given, plus the controller's test and report and the PR when present.
func recordState(r *journal.Record, status, reason, head string, view map[string]any, result json.RawMessage) intake.State {
	state := intake.State{Status: status, HoldReason: reason, Branch: r.Branch, HeadSHA: head, View: encode(view), Result: result}
	if r.ExecutionPlanSHA != "" && !r.IntakeMergeRecovery {
		var measured TestResult
		if json.Unmarshal(r.Test, &measured) == nil && measured.Passed && measured.Observation != nil {
			state.ControllerTest = encode(map[string]any{"planSHA": r.ExecutionPlanSHA, "test": json.RawMessage(r.Test)})
		}
		if hasJSON(r.ExecutionReport) {
			var proof struct {
				Marker string `json:"marker"`
			}
			modern := fmt.Sprintf(executionReportMarkerPrefix+"%d:g%d:%s:%s -->", r.ID, r.Generation, r.Head, r.ExecutionPlanSHA)
			legacy := fmt.Sprintf(executionReportMarkerPrefix+"%d:%s:%s -->", r.ID, r.Head, r.ExecutionPlanSHA)
			if head == r.Head && json.Unmarshal(r.ExecutionReport, &proof) == nil && (proof.Marker == modern || status != "held" && proof.Marker == legacy) {
				state.ControllerReport = r.ExecutionReport
			}
		}
	}
	if r.PR > 0 {
		state.PR = &r.PR
	}
	return state
}

// transferReport validates the two fields together before they can reach intake.
func transferReport(raw json.RawMessage) (json.RawMessage, json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil, nil, nil
	}
	transfers, present := obj["acceptanceTransfers"]
	if !present {
		return nil, nil, nil
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(transfers, &entries) != nil || entries == nil {
		return nil, nil, errors.New("acceptanceTransfers は配列である必要があります")
	}
	if len(entries) == 0 {
		return nil, nil, nil
	}
	var issues []map[string]json.RawMessage
	if len(entries) > 30 || json.Unmarshal(obj["followupIssues"], &issues) != nil || issues == nil || len(issues) > 25 {
		return nil, nil, errors.New("移送または後続依頼の件数が不正です")
	}
	validText := func(raw json.RawMessage, max int) bool {
		var v string
		return json.Unmarshal(raw, &v) == nil && strings.TrimSpace(v) != "" && len([]rune(v)) <= max
	}
	validList := func(raw json.RawMessage, max int) bool {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) != nil || len(list) < 1 || len(list) > 30 {
			return false
		}
		for _, item := range list {
			if !validText(item, max) {
				return false
			}
		}
		return true
	}
	for _, issue := range issues {
		if !validText(issue["title"], 200) || !validText(issue["purpose"], 8000) || !validList(issue["acceptance"], 2000) {
			return nil, nil, errors.New("後続依頼の内容が不正です")
		}
		if kind, ok := issue["kind"]; ok {
			var value string
			if json.Unmarshal(kind, &value) != nil || value != "implementation" {
				return nil, nil, errors.New("後続依頼の kind が不正です")
			}
		}
	}
	for _, entry := range entries {
		if len(entry) != 3 || !validText(entry["criterion"], 2000) || !validList(entry["artifacts"], 2000) {
			return nil, nil, errors.New("受入条件の移送内容が不正です")
		}
		var numbers []int
		if json.Unmarshal(entry["followupNumbers"], &numbers) != nil || len(numbers) == 0 {
			return nil, nil, errors.New("後続依頼の番号が不正です")
		}
		seen := map[int]bool{}
		for _, n := range numbers {
			if n < 1 || n > len(issues) || seen[n] {
				return nil, nil, errors.New("後続依頼の番号が範囲外または重複しています")
			}
			seen[n] = true
		}
	}
	var compact bytes.Buffer
	data, err := json.Marshal(map[string]json.RawMessage{"acceptanceTransfers": transfers, "followupIssues": obj["followupIssues"]})
	if err != nil || json.Compact(&compact, data) != nil || compact.Len() > 24*1024 {
		return nil, nil, cause.New("移送報告が24KiBを超えています", err)
	}
	return transfers, obj["followupIssues"], nil
}

// redactResult hides credentials anywhere in the final result: the AI's
// implementation report and review are sent as they were written. The
// patterns match only letters, digits, _ and - after a fixed prefix, and the
// replacement has no quote or backslash, so the JSON stays valid; the check
// below is a guard for that, not an expected path.
func (e *Executor) redactResult(raw json.RawMessage) json.RawMessage {
	hidden := json.RawMessage(e.redact(string(raw)))
	if !json.Valid(hidden) {
		return raw
	}
	return hidden
}

func (e *Executor) finish(r *journal.Record, status, reason, summary string, view map[string]any) (intake.State, error) {
	finalApprovalHold, _ := view["finalApprovalHold"].(bool)
	delete(view, "finalApprovalHold")
	transfers, followups, transferErr := transferReport(r.Result)
	if transferErr != nil {
		if status == "merged" {
			summary = "受入条件の移送欄を省略しました: " + transferErr.Error() + "。" + summary
		} else if relation, _ := view["targetBranchRelation"].(string); relation != "behind" && relation != "diverged" && view["continuationHead"] == nil {
			if finalApprovalHold {
				summary = "受入条件の移送を報告できません: " + transferErr.Error() + "。" + summary
			} else {
				status, reason, summary = "held", "review_held", "受入条件の移送を報告できません: "+transferErr.Error()
			}
		}
	}
	// The summary is written by the implementing or reviewing AI; hide
	// credentials before it is cut and sent.
	summary = e.redact(summary)
	if status == "merged" && r.PR > 0 && !gitwork.ValidSHA(r.Head) {
		summary = "マージコミットの形式が不正なため欄を省略しました。" + summary
	}
	var diagnosticErr error
	summary, diagnosticErr = e.leaseDiagnosticSummary(r.ID, summary)
	if diagnosticErr != nil {
		log.Printf("lease job=%d event=diagnostic_read_failed", r.ID)
	}
	resultStatus := status
	if status == "held" && reason == "prerequisite_missing" {
		resultStatus = "failed"
	}
	reportHead := r.Head
	if view["continuationHead"] != nil && reportHead != "" && !gitwork.ValidSHA(reportHead) {
		reportHead = ""
	}
	result := map[string]any{"status": resultStatus, "summary": clip(summary, 2000), "implementationResult": json.RawMessage(r.Result), "implementationContent": reportHead, "controllerFileActions": r.ControllerChanges}
	if message, ok := view["mergeRefusalMessage"].(string); ok && status == "held" && reason == "review_held" {
		result["message"] = message
		delete(view, "mergeRefusalMessage")
	}
	if transfers != nil {
		result["acceptanceTransfers"], result["followupIssues"] = transfers, followups
	}
	if status == "held" && reason == "prerequisite_missing" {
		var implementation Implementation
		if json.Unmarshal(r.Result, &implementation) == nil {
			result["prerequisiteMissing"] = implementation.PrerequisiteMissing
		}
		result["questions"] = []string{}
	}
	if status == "merged" && r.PR > 0 && gitwork.ValidSHA(r.Head) {
		result["mergeCommit"] = r.Head
	}
	if hasJSON(r.Review) {
		result["review"] = json.RawMessage(r.Review)
	}
	if len(r.FollowupProposals) > 0 {
		result["followupProposals"] = r.FollowupProposals
	}
	if len(r.ReviewRounds) > 0 {
		result["reviewRounds"] = journal.LimitReviewRounds(r.ReviewRounds)
	}
	r.EnsureReviewCounts()
	result["reviewCounts"] = r.ReviewCounts
	// The next run may start on another PC, whose only copy of these is this result.
	if !hasJSON(r.Review) && hasJSON(r.PreviousReview) {
		result["previousReview"] = json.RawMessage(r.PreviousReview)
	}
	if !hasJSON(r.Result) && hasJSON(r.PreviousResult) {
		result["previousImplementationResult"] = json.RawMessage(r.PreviousResult)
	}
	if hasJSON(r.Test) {
		result["fixedTest"] = json.RawMessage(r.Test)
		var tested TestResult
		if json.Unmarshal(r.Test, &tested) == nil {
			if tested.Observation != nil {
				result["fixedTestCause"] = e.classifyTest(r.Repo, tested.Observation)
			}
			if tested.Diagnostic != nil {
				result["fixedTestDiagnostic"] = tested.Diagnostic
			}
		}
	}
	if status == "failed" {
		result["error"] = clip(summary, 2000)
	}
	if view == nil {
		view = map[string]any{}
	}
	if hasJSON(r.ProviderHold) {
		view["usageLimit"] = json.RawMessage(r.ProviderHold)
		result["usageLimit"] = json.RawMessage(r.ProviderHold)
	}
	e.addRecordView(view, r, r.Stage)
	state := recordState(r, status, reason, reportHead, view, e.redactResult(encode(result)))
	if status != "merged" {
		// After the merge the default branch contains head, so the diff would be empty.
		e.addDiffFingerprint(context.Background(), r, &state)
	}
	if err := boundFinalResult(r.Epoch, &state); err != nil {
		return intake.State{}, err
	}
	r.FinalReported = false
	r.FinalState = encode(state)
	r.FinalReportEpoch = 0
	if status == "merged" {
		r.Stage = "done"
	} else {
		r.Stage = status
	}
	if err := e.Store.Save(r); err != nil {
		return intake.State{}, err
	}
	return state, nil
}
