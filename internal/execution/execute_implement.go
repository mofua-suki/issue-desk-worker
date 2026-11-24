package execution

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"issue-desk-worker/internal/intake"
)

// implementStage starts the implementation AI (stage "implement"), completes
// its report (also on "implement_report"), and runs the fixed test.
func (x *executionRun) implementStage() stageOutcome {
	e, r := x.e, x.r
	prompt := implementationPrompt(x.d, x.issue.Title, x.issue.Body, r)
	if r.Stage == "implement" {
		if out := x.startImplementation(prompt); out.action == stageStop {
			return out
		}
	}
	for {
		if terminal, err := e.completeImplementationReport(x.ctx, x.d, r, x.d.Generation > 0 || x.d.Kind == "followup" || strings.Contains(x.issue.Body, "bugfix")); err != nil {
			return fail(err)
		} else if terminal != nil {
			return stopAt(terminal)
		}
		continued, err := e.continueIncomplete(x.ctx, x.d, r, prompt, time.Now)
		if err != nil {
			return fail(err)
		}
		if !continued {
			break
		}
	}
	if _, _, err := transferReport(r.Result); err != nil {
		return stop(e.finish(r, "held", "review_held", "受入条件の移送を報告できません: "+err.Error(), nil))
	}
	var result Implementation
	_ = json.Unmarshal(r.Result, &result)
	if result.Status == "question" || len(result.Questions) > 0 {
		return stop(e.finish(r, "held", "question", incompleteHoldSummary(r, result), nil))
	}
	if result.Status != "completed" {
		reason := "question"
		if hasJSON(result.PrerequisiteMissing) {
			reason = "prerequisite_missing"
		}
		return stop(e.finish(r, "held", reason, incompleteHoldSummary(r, result), nil))
	}
	prepare := e.testTargetPreparer(x.ctx, x.workspace, r, x.issue.Title, x.fence)
	requests, _, requestErr := editRequestHash(r.Result)
	if r.Provider == "codex" && requestErr == nil && len(requests.DeleteFiles) == 0 && len(requests.Dependencies) == 0 && (r.ReportCompletion == nil || !r.ReportCompletion.Attempted) {
		original := prepare
		first := true
		prepare = func() error {
			if err := original(); err != nil {
				return err
			}
			if first {
				first = false
				if out := x.followDefaultBranch(); out.action != stageProceed {
					return testFenceError{initialFollowOutcome{out}}
				}
			}
			return nil
		}
	}
	if terminal, err := e.implementationTests(x.ctx, x.d, x.repository, r, prompt, prepare); err != nil {
		var follow initialFollowOutcome
		if errors.As(err, &follow) {
			return follow.outcome
		}
		return fail(err)
	} else if terminal != nil {
		return stopAt(terminal)
	}
	if err := e.checkpoint(x.ctx, x.d, r, "review"); err != nil {
		return fail(err)
	}
	return repeat()
}

type initialFollowOutcome struct{ outcome stageOutcome }

func (initialFollowOutcome) Error() string {
	return "初回固定テスト前の基点追従を中断しました"
}

// startImplementation checks the run and usage limits, records the start and
// calls the implementation AI. It returns stageProceed once the report is received.
func (x *executionRun) startImplementation(prompt string) stageOutcome {
	// The stage does not change the scope, so a copy of the descriptor is current.
	e, d, r := x.e, x.d, x.r
	if slices.Contains(r.ContinuationExcludedGenerations, d.Generation) {
		prospective := *r
		prospective.ExcludedGenerations = slices.Clone(r.ExcludedGenerations)
		prospective.ContinuationExcludedGenerations = slices.Clone(r.ContinuationExcludedGenerations)
		markImplementationStarted(d, &prospective)
		x.generationRuns = implementationRunCount(d, &prospective)
	}
	// An old journal without a start timestamp cannot establish when an
	// already-started run began; do not create one while resuming it.
	newIncompleteRun := !generationStarted(r, x.generationRuns) || x.reworked
	limit := e.implementationRunLimit(d)
	// A restart of a started generation does not add to this total. Supplements
	// consume the run budget, but do not turn a later user implementation into
	// an automatic one or consume its admission.
	total := x.generationRuns + nonGenerationRuns(r)
	if total > limit {
		// Report the refused total: an over-limit approval raises the limit from this value.
		r.Runs = total
		return stop(e.finish(r, "held", "question", "実装回数の上限に達しました", map[string]any{"implementationRunLimit": true}))
	}
	if err := e.checkImplementationUsage(x.ctx, d, r, x.interrupted || x.reworked || generationStarted(r, x.generationRuns), false); err != nil {
		return stop(e.finish(r, "held", "question", err.Error(), map[string]any{"implementationUsageStop": true}))
	}
	r.Runs = total
	r.OpenRun, r.OpenRunGeneration = r.Runs, d.Generation
	r.OpenRunEpoch, r.RejectedStart = r.Epoch, nil
	r.FixedTestFailures = 0
	r.Provider = implementationProvider(d, r)
	if err := e.checkpoint(x.ctx, d, r, "implement"); err != nil {
		return fail(e.saveRejectedStart(r, err))
	}
	if err := e.setup(x.ctx, x.repository, r); err != nil {
		excludeGeneration(d, r)
		return stop(e.finish(r, "held", "question", "実行環境を準備できません: "+err.Error(), map[string]any{"environment": true}))
	}
	callDescriptor := d
	callDescriptor.conflictResolution = x.conflict
	if newIncompleteRun || r.IncompleteRun != nil && (r.IncompleteRun.Generation != r.Generation || r.IncompleteRun.Run != r.ReviewReworkRuns) {
		beginIncompleteRun(r, time.Now())
	}
	markImplementationStarted(d, r)
	if err := e.checkpoint(x.ctx, d, r, "implement"); err != nil {
		return fail(err)
	}
	answer, callErr := e.callAI(x.ctx, callDescriptor, r, prompt, false, nil)
	if x.ctx.Err() == nil {
		r.OpenRun, r.OpenRunGeneration = 0, 0
	}
	var response struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(answer.JSON, &response)
	savedReport := callErr == nil && x.ctx.Err() == nil && response.Status == "completed"
	if savedReport {
		if err := e.receiveImplementationReport(r, answer.JSON, "implement_report"); err != nil {
			return fail(err)
		}
	}
	if err := e.flushUsage(x.ctx, r); err != nil {
		return fail(err)
	}
	if err := x.ctx.Err(); err != nil {
		return fail(err)
	}
	if callErr != nil {
		return stop(e.finish(r, "failed", "", callErr.Error(), nil))
	}
	if !savedReport {
		if err := e.receiveImplementationReport(r, answer.JSON, "implement_report"); err != nil {
			return fail(err)
		}
	}
	return proceed()
}

// testStage runs the fixed test for a tree whose implementation is already
// reported. While a merge turn is held the test runs on the followed tree.
func (x *executionRun) testStage() stageOutcome {
	e, r := x.e, x.r
	prompt := implementationPrompt(x.d, x.issue.Title, x.issue.Body, r)
	var terminal *intake.State
	var err error
	if x.heldTurn != nil && r.Stage == "test" {
		terminal, err = e.followedTest(x.ctx, x.parentCtx, x.d, x.repository, r, x.workspace, x.issue.Title, prompt, x.fence, x.releaseTurn)
	} else {
		terminal, err = e.implementationTests(x.ctx, x.d, x.repository, r, prompt, e.testTargetPreparer(x.ctx, x.workspace, r, x.issue.Title, x.fence))
	}
	if err != nil {
		return fail(err)
	} else if terminal != nil {
		return stopAt(terminal)
	}
	if err := e.checkpoint(x.ctx, x.d, r, "review"); err != nil {
		return fail(err)
	}
	return repeat()
}
