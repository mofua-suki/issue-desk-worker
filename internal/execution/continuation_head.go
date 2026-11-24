package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// finalApprovalPublication is selected only from a complete, matching intake
// contract. It is deliberately separate from permission-approval resumption.
type finalApprovalPublication struct {
	Generation *int   `json:"generation"`
	Branch     string `json:"branch"`
	PR         *int64 `json:"pr"`
	HeadSHA    string `json:"headSha"`
	Verified   *bool  `json:"verified"`
}

func selectFinalApprovalPublication(d Descriptor) (finalApprovalPublication, bool) {
	var p finalApprovalPublication
	var continuation struct {
		Publication json.RawMessage `json:"publication"`
	}
	if json.Unmarshal(d.View["continuation"], &continuation) != nil || json.Unmarshal(continuation.Publication, &p) != nil {
		return p, false
	}
	for _, key := range []string{"retryFromBase", "reimplementExpected"} {
		var instruction struct {
			Generation *int `json:"generation"`
		}
		if json.Unmarshal(d.View[key], &instruction) == nil && instruction.Generation != nil && *instruction.Generation == d.Generation {
			return p, false
		}
	}
	return p, p.Verified != nil && *p.Verified && p.Generation != nil && *p.Generation == d.Generation &&
		p.PR != nil && *p.PR > 0 && *p.PR == d.PR && p.Branch != "" && p.Branch == d.Branch &&
		gitwork.ValidBranchShape(p.Branch) && gitwork.ValidSHA(p.HeadSHA) && p.HeadSHA == d.HeadSHA
}

// An empty cause permits continuation; every other result requires a hold before
// changing the approved head. The workspace's origin ref must have been fetched.
func (e *Executor) checkFinalApprovalPublication(ctx context.Context, d Descriptor, p finalApprovalPublication, w *gitwork.Workspace) string {
	pull, err := e.Gitea.Pull(ctx, d.Repo, *p.PR)
	if err != nil {
		return "publication_pr_unavailable"
	}
	switch {
	case pull.Number != *p.PR:
		return "publication_pr_number_mismatch"
	case pull.Head.Repo.FullName != d.Repo:
		return "publication_head_repo_mismatch"
	case pull.Base.Repo.FullName != d.Repo:
		return "publication_base_repo_mismatch"
	case pull.Base.Ref != w.Default:
		return "publication_base_mismatch"
	case pull.Head.Ref != p.Branch:
		return "publication_branch_mismatch"
	case pull.Head.SHA != p.HeadSHA:
		return "publication_pr_head_mismatch"
	}
	out, err := w.Config.Run(ctx, w.Tree, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+p.Branch+"^{commit}")
	if err != nil || out.Code != 0 || !gitwork.ValidSHA(strings.TrimSpace(out.Out)) {
		return "publication_branch_unavailable"
	}
	if strings.TrimSpace(out.Out) != p.HeadSHA {
		return "publication_branch_head_mismatch"
	}
	return ""
}

// Only an explicit, current-generation retry can discard the old branch's head.
// A head delivered for the retry branch is its continuation and must be checked,
// including when this PC has just recovered the retry's applied marker.
func continuationHeadRequired(d Descriptor, r *journal.Record) bool {
	oldRetryHead := abandonedRetryDescriptor(d, r)
	started := field[startedExecution](d.View, "workerExecution")
	resuming := r.Runs > 0 || started.Runs > 0 || r.Stage == "stopped" && !stoppedBeforeStartMayResume(d, r) || r.Stage == "held" || r.Stage == "failed"
	return r.Head != "" || !oldRetryHead && (d.HeadSHA != "" || d.Generation > 0 || resuming || d.Branch != "" && d.Branch != r.Branch)
}

// Only a locally witnessed first-lease stop with consistent intake evidence can
// bypass the stopped-stage requirement. Other sources are checked separately.
func stoppedBeforeStartMayResume(d Descriptor, r *journal.Record) bool {
	started := field[startedExecution](d.View, "workerExecution")
	var result struct {
		Implementation json.RawMessage `json:"implementationResult"`
		Previous       json.RawMessage `json:"previousImplementationResult"`
		Review         json.RawMessage `json:"review"`
		PreviousReview json.RawMessage `json:"previousReview"`
	}
	if hasJSON(d.Result) && json.Unmarshal(d.Result, &result) != nil {
		return false
	}
	if hasJSON(result.Implementation) || hasJSON(result.Previous) || hasJSON(result.Review) || hasJSON(result.PreviousReview) {
		return false
	}
	return r.StoppedBeforeStart && r.Stage == "stopped" && r.FinalReported &&
		d.Generation == 0 && r.Generation == d.Generation && r.Epoch == 2 &&
		r.StartHistory != nil && r.StartHistory.Known && r.StartHistory.Epoch == r.Epoch &&
		continuationNotStarted(d, r) && r.Runs == 0 && started.Runs == 0 &&
		r.OpenRun == 0 && started.OpenRun == 0 && r.InterruptedRuns == 0 && started.InterruptedRuns == 0 &&
		r.PR == 0 && !r.PRCreationAttempted && d.PR == 0 && r.Head == "" && d.HeadSHA == "" &&
		d.Branch == "" && r.Branch == fmt.Sprintf("codex/issue-%d-job-%d", d.Issue, d.ID) && r.Tree == "" && r.ContinuationBranch == nil &&
		len(r.StartedGenerations) == 0 && len(started.StartedGenerations) == 0 &&
		r.ReportCompletionRuns == 0 && started.ReportCompletionRuns == 0 && r.UnansweredRuns == 0 && started.UnansweredRuns == 0 &&
		r.ReviewReworkRuns == 0 && started.ReviewReworkRuns == 0 &&
		!hasJSON(r.Result) && !hasJSON(r.Review) && !hasJSON(r.Test)
}

// Import evidence without reclaiming an interrupted start or recomputing counts.
// A report carried by intake proves what was reported, never that the tree exists.
func seedContinuationEvidence(d Descriptor, r *journal.Record) {
	seedPreviousResults(r, d)
	var delivered struct {
		Test json.RawMessage `json:"fixedTest"`
	}
	if !hasJSON(r.Test) && json.Unmarshal(d.Result, &delivered) == nil && usableCarried(delivered.Test) {
		r.Test = delivered.Test
	}
	if !retryBaseApplied(d, r) {
		r.ReviewHolds = max(r.ReviewHolds, field[int](d.View, "reviewHolds"))
	}
	started := field[startedExecution](d.View, "workerExecution")
	carryStartedRuns(r, started)
	r.InterruptedRuns = max(r.InterruptedRuns, started.InterruptedRuns)
	if r.OpenRun == 0 {
		r.OpenRun, r.OpenRunGeneration = started.OpenRun, started.OpenRunGeneration
	}
	classifyLocalExclusions(r)
	r.ExcludedGenerations = mergeGenerations(r.ExcludedGenerations, started.ExcludedGenerations)
	wasExcluded := slices.Contains(r.ContinuationExcludedGenerations, d.Generation) || slices.Contains(started.ContinuationExcludedGenerations, d.Generation)
	mergeStartEvidence(r, started)
	if wasExcluded && !slices.Contains(r.ContinuationExcludedGenerations, d.Generation) && slices.Contains(r.StartedGenerations, d.Generation) && r.Generation == d.Generation {
		r.Runs = max(r.Runs, implementationRunCount(d, r)+nonGenerationRuns(r))
	}
	clearUncertainCurrentExclusion(d, r)
	r.ResumedGenerations = mergeGenerations(r.ResumedGenerations, started.ResumedGenerations)
	r.ResumesSeen = max(r.ResumesSeen, started.ResumesSeen)
	r.WeightedUsage = max(r.WeightedUsage, field[float64](d.View, "implementationWeightedUsage"))
	// workerExecution itself also carries weightedUsage in current Workers.
	var checkpoint struct {
		WeightedUsage float64 `json:"weightedUsage"`
	}
	_ = json.Unmarshal(d.View["workerExecution"], &checkpoint)
	r.WeightedUsage = max(r.WeightedUsage, checkpoint.WeightedUsage)
	if r.Head == "" && gitwork.ValidSHA(d.HeadSHA) {
		r.Head = d.HeadSHA
	}
}

func continuationHeadCause(ctx context.Context, w *gitwork.Workspace, required string) string {
	if !gitwork.ValidSHA(required) {
		return "invalid_sha"
	}
	// Do not infer cleanliness from a failed status command. Dirty trees still go
	// through the existing pending-change handling once the commit is included.
	if err := w.CheckTargetClean(ctx); err != nil && !errors.Is(err, gitwork.ErrWorkspaceUncommitted) {
		return "status_unavailable"
	}
	out, err := w.Config.Run(ctx, w.Tree, "cat-file", "-t", required)
	if err != nil || out.Code != 0 {
		return "commit_unavailable"
	}
	if strings.TrimSpace(out.Out) != "commit" {
		return "not_commit"
	}
	head, err := w.Head(ctx)
	if err != nil || !gitwork.ValidSHA(head) {
		return "head_unavailable"
	}
	if head == required {
		return ""
	}
	out, err = w.Config.Run(ctx, w.Tree, "merge-base", "--is-ancestor", required, head)
	if err != nil || out.Code != 0 && out.Code != 1 {
		return "comparison_unavailable"
	}
	if out.Code == 1 {
		return "not_included"
	}
	return ""
}

func (e *Executor) holdContinuationHead(ctx context.Context, d Descriptor, r *journal.Record, fence func(context.Context) error, cause string, diagnostics ...continuationDiagnostic) (*intake.State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := fence(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if continuationNotStarted(d, r) {
		if !slices.Contains(r.ExcludedGenerations, d.Generation) {
			oldCount, oldRuns := implementationRunCount(d, r), r.Runs
			excludeGeneration(d, r)
			r.OtherExcludedGenerations = slices.DeleteFunc(r.OtherExcludedGenerations, func(g int) bool { return g == d.Generation })
			r.ContinuationExcludedGenerations = mergeGenerations(r.ContinuationExcludedGenerations, []int{d.Generation})
			r.Runs = oldRuns
			if r.Generation == d.Generation && oldRuns-nonGenerationRuns(r) >= oldCount {
				r.Runs -= oldCount - implementationRunCount(d, r)
			}
		}
	}
	// Only fixed source names, classifications and validated hashes cross this boundary.
	diag := continuationDiagnostic{source: "unavailable"}
	if len(diagnostics) > 0 {
		diag = diagnostics[0]
	}
	if !gitwork.ValidSHA(diag.head) {
		diag.head = ""
	}
	head := diag.head
	if head == "" {
		head = "未確認"
	}
	targetBranchDirty := cause == "target_branch_dirty"
	if targetBranchDirty {
		cause = "target_dirty"
	}
	missing, resume := continuationExplanation(cause)
	summary := fmt.Sprintf("継続を保留: cause=%s、照合元=%s、確認対象head=%s、不足内容=%s、再開条件=%s", cause, diag.source, head, missing, resume)
	required := ""
	if gitwork.ValidSHA(d.HeadSHA) {
		required = d.HeadSHA
	}
	view := map[string]any{"continuationHead": map[string]any{"requiredHead": required, "checkedHead": diag.head, "cause": cause, "source": diag.source}}
	if cause == "target_diverged" || cause == "target_dirty" && !targetBranchDirty {
		summary = errTargetBranchDiverged.Error() + "。" + summary
	}
	if cause == "target_dirty" {
		if targetBranchDirty {
			summary = errTargetBranchDirty.Error() + "。" + summary
			view["releasePending"] = true
		}
		view["targetBranchRelation"] = "behind"
	} else if cause == "target_diverged" {
		view["targetBranchRelation"] = "diverged"
	}
	state, err := e.finish(r, "held", "review_held", summary, view)
	return &state, err
}

// A missing-tree hold is not an implementation answer. After intake confirms
// delivery, resume the exact suspended stage instead of starting implementation
// again (especially when a report was already received).
func restoreContinuationStage(r *journal.Record) bool {
	if r.Stage != "held" || !r.FinalReported {
		return false
	}
	var previous intake.State
	var view map[string]json.RawMessage
	if json.Unmarshal(r.FinalState, &previous) != nil || json.Unmarshal(previous.View, &view) != nil || !hasJSON(view["continuationHead"]) {
		return false
	}
	stage := field[struct {
		Stage string `json:"stage"`
	}](view, "workerExecution").Stage
	switch stage {
	case "implement", "implement_report", "test", "test_report", "review", "publish", "merge":
		r.Stage = stage
		return true
	}
	return false
}

// This boundary precedes generation resets, interrupted-run recovery, usage
// flushing and every implementation/report/test/review entry. Open retains the
// existing remote fetch; no checkout/reset or access to another PC is added.
func (e *Executor) openContinuationWorkspace(ctx context.Context, d Descriptor, r *journal.Record, base string, fence func(context.Context) error) (*gitwork.Workspace, *intake.State, error) {
	return e.openContinuationSources(ctx, d, r, base, fence, nil)
}

func (e *Executor) openContinuationSources(ctx context.Context, d Descriptor, r *journal.Record, base string, fence func(context.Context) error, sources []journal.Record) (*gitwork.Workspace, *intake.State, error) {
	required := continuationHeadRequired(d, r) || len(sources) > 0 || continuationPRRequired(d, r)
	if abandonedRetryDescriptor(d, r) {
		// Intake may still carry the abandoned branch. The retry's own journal
		// head remains required even when these old descriptor fields are ignored.
		d.HeadSHA, d.Branch, d.PR = "", "", 0
	} else if explicitContinuationRetry(d, r) && d.Branch == "" && d.PR > 0 {
		// A PR without a delivered branch is not evidence that the old branch
		// was intended. Require it to identify this retry, or hold on mismatch.
		d.Branch = r.Branch
	}
	restored := false
	if required {
		restored = restoreContinuationStage(r)
		seedContinuationEvidence(d, r)
	}
	o := &continuationOpen{e: e, ctx: ctx, d: d, r: r, base: base, fence: fence, sources: sources, required: required, diag: continuationDiagnostic{source: "unavailable"}}
	for _, step := range []func() (bool, *intake.State, error){o.openWorkspace, o.followTarget, o.checkHeads} {
		if stop, state, err := step(); stop {
			return nil, state, err
		}
	}
	if restored {
		r.FinalState = nil
	}
	return o.workspace, nil, nil
}

// continuationOpen is the state of one openContinuationSources call. Each step
// returns stop with the result to return: a hold's state, or an error.
type continuationOpen struct {
	e         *Executor
	ctx       context.Context
	d         Descriptor
	r         *journal.Record
	base      string
	fence     func(context.Context) error
	sources   []journal.Record
	required  bool
	diag      continuationDiagnostic
	workspace *gitwork.Workspace
}

// hold holds the continuation with cause and the current diagnostic.
func (o *continuationOpen) hold(cause string) (bool, *intake.State, error) {
	state, err := o.e.holdContinuationHead(o.ctx, o.d, o.r, o.fence, cause, o.diag)
	return true, state, err
}

// failed holds a required continuation with cause, and returns err otherwise.
func (o *continuationOpen) failed(cause string, err error) (bool, *intake.State, error) {
	if o.required {
		return o.hold(cause)
	}
	return true, nil, err
}

// openWorkspace opens the workspace and, for a required continuation, checks
// the descriptor head and the source jobs.
func (o *continuationOpen) openWorkspace() (bool, *intake.State, error) {
	e, ctx, d, r := o.e, o.ctx, o.d, o.r
	workspace, err := gitwork.Open(ctx, e.gitConfig(), d.Repo, o.base, r.Branch, d.ID)
	if err != nil {
		return o.failed("workspace_unavailable", err)
	}
	o.workspace = workspace
	r.Tree = workspace.Tree
	if !o.required {
		return false, nil, nil
	}
	if d.HeadSHA != "" && !gitwork.ValidSHA(d.HeadSHA) {
		o.diag = continuationDiagnostic{source: "descriptor"}
		return o.hold("invalid_sha")
	}
	cause, followed, err := e.checkContinuationSources(ctx, d, r, workspace, o.sources, o.fence, &o.diag)
	if err != nil {
		return true, nil, err
	} else if cause != "" {
		if cause == "target_dirty" {
			var relation *targetBranchError
			if branchErr := checkTargetBranch(ctx, workspace, r.PR != 0 || r.PRCreationAttempted); errors.As(branchErr, &relation) && relation.relation == "behind" {
				cause = "target_branch_dirty"
			}
		}
		return o.hold(cause)
	}
	// A followed source may introduce files forbidden by Open's tree checks.
	if followed {
		o.workspace, err = gitwork.Open(ctx, workspace.Config, d.Repo, o.base, r.Branch, d.ID)
		if err != nil {
			return o.hold("workspace_unavailable")
		}
	}
	return false, nil, nil
}

// holdTargetDirty holds a target branch that is behind with local changes.
func (o *continuationOpen) holdTargetDirty() (bool, *intake.State, error) {
	if o.required {
		remote, err := o.workspace.Config.Run(o.ctx, o.workspace.Tree, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+o.workspace.Branch+"^{commit}")
		if err == nil && remote.Code == 0 {
			o.diag.head = strings.TrimSpace(remote.Out)
		}
		return o.hold("target_branch_dirty")
	}
	state, err := o.e.holdTargetBranch(o.ctx, o.r, o.fence, &targetBranchError{relation: "behind", dirty: true})
	return true, &state, err
}

// followTarget fast-forwards a target branch that is behind its remote, after
// invalidating the results of the old tree. A diverged branch is held.
func (o *continuationOpen) followTarget() (bool, *intake.State, error) {
	e, ctx, d, r := o.e, o.ctx, o.d, o.r
	o.diag = continuationDiagnostic{source: "remote"}
	err := checkTargetBranch(ctx, o.workspace, r.PR != 0 || r.PRCreationAttempted)
	if err == nil {
		return false, nil, nil
	}
	var relation *targetBranchError
	if !errors.As(err, &relation) {
		return o.failed("target_comparison_unavailable", err)
	}
	if relation.relation != "behind" {
		if o.required {
			return o.hold("target_diverged")
		}
		state, err := e.holdTargetBranch(ctx, r, o.fence, relation)
		return true, &state, err
	}
	cleanErr := o.workspace.CheckTargetClean(ctx)
	if cleanErr != nil {
		if err := ctx.Err(); err != nil {
			return true, nil, err
		}
		if errors.Is(cleanErr, gitwork.ErrWorkspaceUncommitted) {
			return o.holdTargetDirty()
		}
		return o.failed("status_unavailable", cleanErr)
	}
	if err := o.fence(ctx); err != nil {
		return true, nil, err
	}
	if err := ctx.Err(); err != nil {
		return true, nil, err
	}
	// The normal resume selection used to precede target FF. Resolve it here
	// only for that transition; a failed head check must not clear evidence.
	if r.Stage == "held" || r.Stage == "failed" || r.Stage == "stopped" {
		var tested TestResult
		var reviewed Review
		_ = json.Unmarshal(r.Test, &tested)
		_ = json.Unmarshal(r.Review, &reviewed)
		if tested.Passed && r.Head != "" {
			r.Stage = "review"
			if reviewed.Approved() {
				r.Stage = "publish"
			}
		}
	}
	// Preserve the old review as evidence while invalidating its approval for
	// the followed tree. Save before FF, so a crash cannot reuse stale tests.
	keepPreviousResults(r)
	clearFollowedResults(r)
	// A head-free resume can still carry review evidence while at implement.
	// The approval applies to the old tree even in that stage.
	if hasJSON(r.Review) {
		rememberReview(r)
		r.Review = nil
	}
	if r.Stage == "review" || r.Stage == "publish" || r.Stage == "merge" {
		r.Stage = "test"
	}
	if err := e.Store.Save(r); err != nil {
		return true, nil, err
	}
	if err := o.workspace.FastForwardTarget(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return true, nil, ctxErr
		}
		if errors.Is(err, gitwork.ErrWorkspaceUncommitted) {
			return o.holdTargetDirty()
		}
		return o.failed("target_follow_unavailable", err)
	}
	o.workspace, err = gitwork.Open(ctx, o.workspace.Config, d.Repo, o.base, r.Branch, d.ID)
	if err != nil {
		return o.failed("workspace_unavailable", err)
	}
	if err := checkTargetBranch(ctx, o.workspace, true); err != nil {
		if o.required {
			return o.hold("target_comparison_unavailable")
		}
		if errors.Is(err, errTargetBranchDiverged) {
			state, err := e.holdTargetBranch(ctx, r, o.fence, err)
			return true, &state, err
		}
		return true, nil, err
	}
	return false, nil, nil
}

// checkHeads checks that the workspace contains the descriptor, journal and
// remote heads of a required continuation.
func (o *continuationOpen) checkHeads() (bool, *intake.State, error) {
	if !o.required {
		return false, nil, nil
	}
	ctx, d, r, workspace := o.ctx, o.d, o.r, o.workspace
	for _, item := range []continuationDiagnostic{{"descriptor", d.HeadSHA}, {"journal", r.Head}} {
		head := item.head
		if head != "" {
			o.diag = item
			if cause := continuationHeadCause(ctx, workspace, head); cause != "" {
				return o.hold(cause)
			}
		}
	}
	remote, err := workspace.Config.Run(ctx, workspace.Tree, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+workspace.Branch+"^{commit}")
	if err != nil || remote.Code != 0 && remote.Code != 1 {
		o.diag = continuationDiagnostic{source: "remote"}
		return o.hold("comparison_unavailable")
	}
	if remote.Code == 0 {
		o.diag = continuationDiagnostic{"remote", strings.TrimSpace(remote.Out)}
		if cause := continuationHeadCause(ctx, workspace, strings.TrimSpace(remote.Out)); cause != "" {
			return o.hold(cause)
		}
	} else if d.HeadSHA == "" && r.Head == "" && len(o.sources) == 0 && !continuationPRRequired(d, r) && (d.Branch == "" || d.Branch == r.Branch) {
		return o.hold("commit_unavailable")
	}
	return false, nil, nil
}

// continuationDiagnostic contains only a fixed provenance label and a candidate hash.
type continuationDiagnostic struct{ source, head string }

func continuationExplanation(cause string) (string, string) {
	switch cause {
	case "invalid_sha":
		return "照合元のheadが不正で確認できません", "正しいheadを提供してください"
	case "commit_unavailable", "not_commit", "source_missing":
		return "必要なコミットまたは照合元を確認できません", "必要な成果物と照合可能なheadを届けてください"
	case "not_included", "source_ambiguous":
		return "作業ツリーへの必要コミットの包含を確認できません", "必要な成果物を届け、包含できる状態にしてください"
	case "source_records_unavailable":
		return "以前のjobのjournalを読めません", "journalを読める状態にしてください"
	case "source_pr_unavailable", "source_pr_mismatch":
		return "継続元PRの取得または対応を確認できません", "継続元PRと枝の対応を確認してください"
	case "source_branch_unavailable", "source_branch_invalid":
		return "継続元の枝を確認できません", "正しい枝とその成果物を確認できる状態にしてください"
	case "target_dirty", "target_diverged":
		return "作業ツリーを安全に追従できません", "変更や分岐を解消して再開してください"
	default:
		return "照合に必要な情報または操作を確認できません", "照合元と作業ツリーを確認できる状態にして再開してください"
	}
}
