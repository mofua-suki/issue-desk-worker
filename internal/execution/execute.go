package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"os"
	"strings"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func (e *Executor) execute(ctx context.Context, job intake.Job, fence func(context.Context) error) (state intake.State, executionErr error) {
	var d Descriptor
	narrowAttempts := make(map[string]bool)
	t, err := e.loadExecutionTarget(ctx, job, &d)
	if err != nil {
		return intake.State{}, err
	}
	repository, saved, info, issue := t.repository, t.saved, t.info, t.issue
	if issue.State == "closed" && !hasJSON(issue.PullRequest) {
		if out, done, err := e.resumeNoDiffClose(ctx, job, d, saved, info, issue, fence); done {
			return out, err
		}
		if out, done, err := e.resumeIntakeMerge(ctx, job, d, saved, info.DefaultBranch, fence); done {
			return out, err
		}
	}
	if out, done, err := e.resumeSavedMerge(ctx, job, d, saved, info.DefaultBranch, fence); done {
		return out, err
	}
	if issue.State != "open" || hasJSON(issue.PullRequest) {
		return closedIssueState(), nil
	}

	if saved == nil && retryBaseNeeded(d, &journal.Record{}) {
		saved = &journal.Record{ID: d.ID, Repo: d.Repo, Issue: d.Issue, Generation: d.Generation, Stage: "implement", Provider: d.Provider}
	}
	var retrySaved *journal.Record
	if saved != nil && retryBaseNeeded(d, saved) {
		if err := fence(ctx); err != nil {
			return intake.State{}, err
		}
		if err := e.resetRetryBase(ctx, d, saved); err != nil {
			return intake.State{}, err
		}
		retrySaved = saved
	}

	if !info.Permissions.Push || info.Empty {
		return intake.State{}, errors.New("リポジトリの既定ブランチまたは書込権限を確認できません")
	}
	r, freshJournal, err := e.loadJobRecord(d, retrySaved)
	if err != nil {
		return intake.State{}, err
	}
	defer func() {
		if !errors.Is(executionErr, gitwork.ErrMergeBase) {
			return
		}
		if ctx.Err() != nil {
			state, executionErr = intake.State{}, ctx.Err()
			return
		}
		if err := fence(ctx); err != nil {
			state, executionErr = intake.State{}, err
			return
		}
		state, executionErr = e.finish(r, "held", "review_held", gitwork.ErrMergeBase.Error(), nil)
	}()
	if r.TargetBase == "" {
		r.TargetBase = info.DefaultBranch
	}
	if r.Repo != d.Repo || r.Issue != d.Issue {
		return intake.State{}, errors.New("保存済み仕事の対象が一致しません")
	}
	r.IntakeMergeRecovery = false
	pendingExecutionReport := r.ExecutionReportPending && r.ExecutionPlanSHA == field[string](d.View, "executionPlanSHA")
	if (r.Stage == "done" || !r.FinalReported) && hasJSON(r.FinalState) && r.Generation == d.Generation && !pendingExecutionReport {
		var out intake.State
		if json.Unmarshal(r.FinalState, &out) != nil {
			return intake.State{}, errors.New("完了記録が不正です")
		}
		if err := boundFinalResult(job.LeaseEpoch, &out); err != nil {
			return intake.State{}, err
		}
		return out, nil
	}
	if r.Branch == "" {
		r.Branch = fmt.Sprintf("codex/issue-%d-job-%d", d.Issue, d.ID)
	}
	observeStartHistory(r, d.Generation, job.LeaseEpoch, e.Config.Host, freshJournal)
	r.Epoch = job.LeaseEpoch
	r.ExecutionTier = d.Tier
	saveExecutionAdmission(r, d)
	r.ExecutionPlan, r.ExecutionPlanSHA = d.View["executionPlan"], field[string](d.View, "executionPlanSHA")
	trackFailureRecord(ctx, r)
	if d.Provider == "" {
		return e.finish(r, "held", "question", "受付の実行先が指定されていません", nil)
	}
	publication, finalPublication := selectFinalApprovalPublication(d)
	var workspace *gitwork.Workspace
	{
		var held *intake.State
		if finalPublication {
			workspace, held, err = e.openFinalPublication(ctx, d, publication, r, info.DefaultBranch, fence)
		} else {
			workspace, held, err = e.openRequiredContinuationWorkspace(ctx, d, r, info.DefaultBranch, fence)
		}
		if err != nil {
			return intake.State{}, err
		}
		if held != nil {
			return *held, nil
		}
	}
	if err := e.syncAntigravityInputs(d, r); err != nil {
		return intake.State{}, err
	}
	if r.GenerationTrigger == nil || r.GenerationTrigger.Generation != d.Generation {
		r.GenerationTrigger = newGenerationTrigger(d, r)
	}
	newGeneration := r.Generation != d.Generation
	if newGeneration {
		startGeneration(r, d.Generation, pendingExecutionReport)
	}
	requestHash := reviewRequestHash(d, issue.Title, issue.Body)
	if finalPublication {
		if out := e.seedFinalPublication(ctx, &d, r, workspace, publication, requestHash); out.action == stageStop {
			return out.state, out.err
		}
	}

	interrupted, generationRuns := e.carryReportedProgress(d, r, requestHash, freshJournal, newGeneration)
	if err := e.flushUsage(ctx, r); err != nil {
		return intake.State{}, err
	}
	// Head-free initial work and an explicit old-head retry keep their existing
	// ordering. Only a required continuation must precede the mutations above.
	if workspace == nil {
		var held *intake.State
		workspace, held, err = e.openContinuationWorkspace(ctx, d, r, info.DefaultBranch, fence)
		if err != nil {
			return intake.State{}, err
		}
		if held != nil {
			return *held, nil
		}
	}
	if out := e.applyExecutionPlan(ctx, &d, r, workspace, issue, fence); out.action == stageStop {
		return out.state, out.err
	}
	if err := e.recheckSavedEvidence(ctx, d, r, workspace, finalPublication); err != nil {
		return intake.State{}, err
	}

	x := &executionRun{
		e: e, ctx: ctx, parentCtx: ctx, fence: fence,
		d: d, r: r, issue: issue, repository: repository, workspace: workspace,
		publication: publication, finalPublication: finalPublication,
		requestHash: requestHash, narrowAttempts: narrowAttempts,
		generationRuns: generationRuns, interrupted: interrupted,
	}
	defer func() {
		for _, name := range x.removeOnReturn {
			os.Remove(name)
		}
	}()
	return x.runStages()
}

func closedIssueState() intake.State {
	return intake.State{Status: "stopped", Result: encode(map[string]string{"summary": "Issueが開いていないため実行しません"})}
}

// loadJobRecord reads the job's journal (after a retry reset, the record that
// reset saved), or starts a new one. fresh is true for a new record.
func (e *Executor) loadJobRecord(d Descriptor, retrySaved *journal.Record) (r *journal.Record, fresh bool, err error) {
	if retrySaved != nil {
		r, err = e.Store.LoadAfterSave(retrySaved)
	} else {
		r, err = e.Store.Load(d.ID)
	}
	if err != nil {
		return nil, false, err
	}
	if r == nil {
		return &journal.Record{ID: d.ID, Repo: d.Repo, Issue: d.Issue, Generation: d.Generation, Stage: "implement", Branch: d.Branch, PR: d.PR, Provider: d.Provider}, true, nil
	}
	return r, false, nil
}

// executionTarget is what a job runs against, read before anything changes.
type executionTarget struct {
	repository Repository
	saved      *journal.Record
	info       gitea.Repository
	issue      gitea.Issue
}

// loadExecutionTarget decodes the descriptor into d and reads the repository
// settings, the saved journal and the repository and Issue on Gitea.
func (e *Executor) loadExecutionTarget(ctx context.Context, job intake.Job, d *Descriptor) (executionTarget, error) {
	var t executionTarget
	if err := descriptorProblem(job, d); err != nil {
		return t, err
	}
	if err := e.attachInstructions(d); err != nil {
		return t, err
	}
	repository, err := e.Config.Repository(d.Repo)
	if err != nil {
		return t, err
	}
	d.Provider = e.Config.ProviderFor(d.Provider, repository)
	saved, err := e.Store.Load(d.ID)
	if err != nil {
		return t, err
	}
	info, err := e.Gitea.Repo(ctx, d.Repo)
	if err != nil {
		return t, err
	}
	issue, err := e.Gitea.Issue(ctx, d.Repo, d.Issue)
	if err != nil {
		return t, err
	}
	if saved != nil && (saved.Repo != d.Repo || saved.Issue != d.Issue) {
		return t, errors.New("保存済み仕事の対象が一致しません")
	}
	return executionTarget{repository: repository, saved: saved, info: info, issue: issue}, nil
}

// resumeSavedMerge finishes a saved job whose PR is already merged. done is
// false when the job must run normally.
func (e *Executor) resumeSavedMerge(ctx context.Context, job intake.Job, d Descriptor, saved *journal.Record, defaultBranch string, fence func(context.Context) error) (intake.State, bool, error) {
	_, publicationCandidate := selectFinalApprovalPublication(d)
	if saved == nil || saved.IntakeMergeRecovery || !(saved.Generation == d.Generation || saved.ExecutionReportPending && saved.ExecutionPlanSHA == field[string](d.View, "executionPlanSHA")) || retryBaseNeeded(d, saved) {
		return intake.State{}, false, nil
	}
	merged, readErr := e.matchingMergedPull(ctx, d, saved, defaultBranch)
	if readErr != nil && !publicationCandidate {
		return intake.State{}, true, readErr
	}
	if !merged {
		return intake.State{}, false, nil
	}
	if saved.Stage == "done" && hasJSON(saved.FinalState) && !saved.ExecutionReportPending {
		var out intake.State
		if json.Unmarshal(saved.FinalState, &out) != nil {
			return intake.State{}, true, errors.New("保存済み完了記録が不正です")
		}
		if err := boundFinalResult(job.LeaseEpoch, &out); err != nil {
			return intake.State{}, true, err
		}
		return out, true, nil
	}
	saved.Epoch = job.LeaseEpoch
	saved.Generation = d.Generation
	saveExecutionAdmission(saved, d)
	trackFailureRecord(ctx, saved)
	out, err := e.finishMerged(ctx, saved, fence, "保存済みPRの宛先とマージ済みコミットを確認しました", nil)
	return out, true, err
}

// carryReportedProgress brings results, review holds, implementation runs
// and usage reported by the intake (or another PC) into r, and returns the
// interrupted run and the runs of this generation.
func (e *Executor) carryReportedProgress(d Descriptor, r *journal.Record, requestHash string, freshJournal, newGeneration bool) (bool, int) {
	dropStaleReviewBaseline(r, d, requestHash)
	if !continuationHeadRequired(d, r) {
		seedPreviousResults(r, d)
	}
	carryReviewCounts(r, d)
	e.restoreCarriedHeldReview(r, d, requestHash, freshJournal)
	resumeStoppedStage(r)
	if reported := field[int](d.View, "reviewHolds"); !retryBaseApplied(d, r) && reported > r.ReviewHolds {
		r.ReviewHolds = reported
	}
	// Read prior counts from the local journal or a Worker checkpoint on another PC.
	// syncImplementationRuns then recomputes Runs from the generation formula.
	started := field[startedExecution](d.View, "workerExecution")
	carryStartedRuns(r, started)
	interrupted := reclaimInterruptedRun(d, r, started)
	if reported := field[float64](d.View, "implementationWeightedUsage"); reported > r.WeightedUsage {
		r.WeightedUsage = reported
	}
	return interrupted, syncImplementationRuns(d, r, started, newGeneration)
}

// closeSatisfiedIssue closes an Issue the default branch already satisfies.
func (x *executionRun) closeSatisfiedIssue() stageOutcome {
	e, ctx, d := x.e, x.ctx, x.d
	if err := x.prepareNoDiffClose(); err != nil {
		return fail(err)
	}
	if err := e.Gitea.Request(ctx, "PATCH", fmt.Sprintf("/repos/%s/issues/%d", d.Repo, d.Issue), map[string]string{"state": "closed"}, nil); err != nil {
		return fail(err)
	}
	return stop(e.finishMerged(ctx, x.r, x.fence, "独立レビューで既定ブランチが依頼を満たすことを確認しました", nil))
}

const conflictInstruction = "\n既定ブランチとの競合を両側の目的に従って解消してください。競合箇所を無条件に片方へ寄せないでください。"

func withConflictInstruction(body string) string {
	if strings.Contains(body, conflictInstruction) {
		return body
	}
	return body + conflictInstruction
}

// carriedReportReusable accepts only a completed report whose edit requests
// are already part of the delivered tree; replaying deletions or dependency
// additions on another PC is not comparable, so those use the existing path.
func carriedReportReusable(raw json.RawMessage) bool {
	if !hasJSON(raw) || providers.ValidateReport(raw, true) != nil {
		return false
	}
	if _, _, err := transferReport(raw); err != nil {
		return false
	}
	var report Implementation
	requests, _, err := editRequestHash(raw)
	if err != nil || json.Unmarshal(raw, &report) != nil {
		return false
	}
	return report.Status == "completed" && len(report.Questions) == 0 && len(requests.DeleteFiles) == 0 && len(requests.Dependencies) == 0
}

// clearFollowedResults drops results that no longer describe the followed tree.
// A review sent back to implementation stays, because its findings are the instruction.
func clearFollowedResults(r *journal.Record) {
	r.Test = nil
	if r.Stage != "implement" && r.Stage != "implement_report" {
		rememberReview(r)
		r.Review = nil
	}
}

// startGeneration clears the results of an earlier generation and restarts
// implementation. A pending execution report survives the reset.
func startGeneration(r *journal.Record, generation int, pendingExecutionReport bool) {
	if !pendingExecutionReport {
		r.ExecutionReport = nil
		r.ExecutionReportResult = nil
		r.ExecutionReportHead = ""
		r.ExecutionReportPending = false
	}
	keepPreviousResults(r)
	r.Generation = generation
	r.ReviewBaseline = nil
	r.CarriedHeldReview = nil
	r.ReviewSkipped = false
	r.NotesAttempted = nil
	r.OpenRun, r.OpenRunGeneration = 0, 0
	r.OpenRunEpoch, r.RejectedStart = 0, nil
	r.ReportCompletion = nil
	r.Session = ""
	r.Result = nil
	r.Review = nil
	r.Test = nil
	r.FinalState = nil
	r.ControllerAllowed = nil
	r.Stage = "implement"
}

// dropStaleReviewBaseline discards a review baseline taken for another request
// and sends a publish or merge back to review.
func dropStaleReviewBaseline(r *journal.Record, d Descriptor, requestHash string) {
	if r.ReviewBaseline == nil || reviewBaselineMatches(r, d, requestHash) {
		return
	}
	keepPreviousResults(r)
	r.ReviewBaseline = nil
	r.ReviewSkipped = false
	r.Review = nil
	if r.Stage == "publish" || r.Stage == "merge" {
		r.Stage = "review"
	}
}

// resumeStoppedStage restarts a held, failed or stopped run from the furthest
// stage its saved test and review still support.
func resumeStoppedStage(r *journal.Record) {
	if r.Stage != "held" && r.Stage != "failed" && r.Stage != "stopped" {
		return
	}
	var review Review
	_ = json.Unmarshal(r.Review, &review)
	var test TestResult
	_ = json.Unmarshal(r.Test, &test)
	if test.Passed && r.Head != "" {
		if review.Approved() {
			r.Stage = "publish"
		} else {
			r.Stage = "review"
		}
	} else {
		r.Stage = "implement"
	}
	r.FinalState = nil
}

// seedFinalPublication binds the run to the approved final-publication head and
// resumes it from the furthest stage the saved test and review still support.
func (e *Executor) seedFinalPublication(ctx context.Context, d *Descriptor, r *journal.Record, workspace *gitwork.Workspace, publication finalApprovalPublication, requestHash string) stageOutcome {
	d.finalApprovalFixedTest = true
	seedFinalPublicationReport(*d, r)
	seedContinuationEvidence(*d, r)
	snapshot, snapshotErr := workspace.Snapshot(ctx)
	if snapshotErr != nil {
		if errors.Is(snapshotErr, gitwork.ErrMergeBase) || ctx.Err() != nil {
			return fail(snapshotErr)
		}
		return stop(e.holdFinalPublication(r, "公開対象の差分を確認できません"))
	}
	r.Head, r.Base, r.PR, r.Branch = publication.HeadSHA, snapshot.Base, *publication.PR, publication.Branch
	e.seedFinalPublicationTest(ctx, *d, r)
	if !reusableTestEvidence(r) || e.reviewTestEvidence(ctx, r, r.Head) != "" {
		rememberReview(r)
		r.Test, r.Review = nil, nil
		r.Stage = "test"
	} else if !finalPublicationReviewUsable(e, *d, r, requestHash, snapshot.Diff) {
		rememberReview(r)
		r.Review = nil
		r.Stage = "review"
	} else {
		r.Stage = "publish"
	}
	r.FinalState = nil
	return proceed()
}

// applyExecutionPlan holds a run whose fixed-test requirements need missing
// capabilities, and hands a plan without code changes to the controller or the
// report-only path.
func (e *Executor) applyExecutionPlan(ctx context.Context, d *Descriptor, r *journal.Record, workspace *gitwork.Workspace, issue gitea.Issue, fence func(context.Context) error) stageOutcome {
	plan, err := parseExecutionPlan(*d)
	if err != nil {
		return stop(e.finish(r, "held", "review_held", err.Error(), nil))
	}
	if plan == nil {
		return proceed()
	}
	var environment map[string]any
	missing := []string{}
	for _, requirement := range plan.Requirements {
		if requirement.EvidenceKind != "fixed_test" {
			continue
		}
		for _, capability := range requirement.Capabilities {
			if environment == nil {
				environment = e.executionCapabilities()
			}
			if environment[capability] != true {
				missing = append(missing, capability)
			}
		}
	}
	if len(missing) > 0 {
		excludeGeneration(*d, r)
		return stop(e.finish(r, "held", "review_held", "実装開始前に必要能力を確認できません: "+strings.Join(missing, "、")+"。コントローラーが資料・実行先を確認してください。", map[string]any{"executionWait": "capability", "executionMissingCapabilities": missing}))
	}
	code, report := false, false
	for _, requirement := range plan.Requirements {
		code = code || requirement.EvidenceKind == "fixed_test"
		report = report || requirement.EvidenceKind == "report_comment"
	}
	if !code {
		snapshot, err := workspace.Snapshot(ctx)
		if err != nil {
			return fail(err)
		}
		r.Head, r.Base = snapshot.Head, snapshot.Base
		if !report {
			excludeGeneration(*d, r)
			return stop(e.finish(r, "held", "review_held", "外部確認の担当はコントローラーです。実装を開始せず、保存済みの条件・資料・確認手段を引き継ぎます。", map[string]any{"executionWait": "controller"}))
		}
		return stop(e.executeReportPlan(ctx, *d, r, issue.Title, issue.Body, plan, fence))
	}
	return proceed()
}

// recheckSavedEvidence sends a resumed review, publish or merge back to the
// test stage when the saved test no longer describes the committed tree.
func (e *Executor) recheckSavedEvidence(ctx context.Context, d Descriptor, r *journal.Record, workspace *gitwork.Workspace, finalPublication bool) error {
	// A persisted pass is reusable only for the current committed target and
	// base. Re-run the test before consulting an old review when it is absent.
	checkSavedEvidence := r.Stage == "review" || r.Stage == "publish" || r.Stage == "merge"
	if r.Stage == "merge" && !finalPublication {
		unmerged, conflictErr := workspace.Unmerged(ctx)
		if conflictErr != nil {
			return conflictErr
		}
		if len(unmerged) > 0 {
			checkSavedEvidence = false
		}
	}
	if checkSavedEvidence {
		snapshot, snapshotErr := workspace.Snapshot(ctx)
		if snapshotErr != nil {
			return snapshotErr
		}
		if snapshot.Head != r.Head || snapshot.Base != r.Base || !reusableTestEvidence(r) || e.reviewTestEvidence(ctx, r, snapshot.Head) != "" {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			clearFollowedResults(r)
			if err := e.checkpoint(ctx, d, r, "test"); err != nil {
				return err
			}
		}
	}
	return nil
}

// descriptorProblem decodes the job's descriptor into d and names the check it
// fails, without echoing the descriptor's values.
func descriptorProblem(job intake.Job, d *Descriptor) error {
	if err := json.Unmarshal(job.Raw, d); err != nil {
		return cause.New("仕事の記述子が不正です（JSONとして読めません）", err)
	}
	switch {
	case d.ID != job.ID:
		return fmt.Errorf("仕事の記述子が不正です（仕事番号が貸し出しの%dと違います）", job.ID)
	case d.Issue < 1:
		return errors.New("仕事の記述子が不正です（Issue番号がありません）")
	case job.LeaseEpoch < 1:
		return errors.New("仕事の記述子が不正です（貸し出しのepochがありません）")
	}
	return nil
}
