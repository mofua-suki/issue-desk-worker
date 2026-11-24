package execution

import (
	"context"
	"errors"
	"fmt"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// executionRun is the state one execute call shares between its stages after
// the job has been admitted and its workspace opened. Each stage handler reads
// and updates it, and returns a stageOutcome that tells the loop what to do next.
type executionRun struct {
	e *Executor
	// ctx is replaced by a held merge turn's context and restored to parentCtx
	// when the turn is released, so handlers always read it from the run.
	ctx, parentCtx context.Context
	fence          func(context.Context) error
	// d is updated by scope changes during the review stage.
	d                Descriptor
	r                *journal.Record
	issue            gitea.Issue
	repository       Repository
	workspace        *gitwork.Workspace
	snapshot         gitwork.Snapshot
	publication      finalApprovalPublication
	finalPublication bool
	requestHash      string
	narrowAttempts   map[string]bool
	generationRuns   int
	interrupted      bool
	// reworked is set once a review in this execution sent the change back. Every later
	// start is then automatic, so the usage limit is checked whatever the generation count says.
	reworked bool
	// conflict is set for one loop iteration when the implementation must
	// resolve a pending merge.
	conflict bool
	heldTurn *mergeTurn
	// removeOnReturn lists review inputs removed when execute returns.
	removeOnReturn []string
}

// Final approval always obtains a fresh snapshot at each entry.
func (x *executionRun) reusableSnapshot() gitwork.Snapshot {
	if x.finalPublication {
		return gitwork.Snapshot{}
	}
	return x.snapshot
}

type stageAction int

const (
	// stageProceed lets a pre-stage check fall through to the stage handler.
	stageProceed stageAction = iota
	// stageRepeat starts the next loop iteration with the (possibly changed) stage.
	stageRepeat
	// stageStop returns state and err from execute.
	stageStop
)

type stageOutcome struct {
	action stageAction
	state  intake.State
	err    error
}

func proceed() stageOutcome { return stageOutcome{action: stageProceed} }
func repeat() stageOutcome  { return stageOutcome{action: stageRepeat} }

func stop(state intake.State, err error) stageOutcome {
	return stageOutcome{action: stageStop, state: state, err: err}
}

func fail(err error) stageOutcome { return stop(intake.State{}, err) }

func stopAt(state *intake.State) stageOutcome { return stop(*state, nil) }

// releaseTurn gives up a held merge turn and restores the execution context.
// It reports whether the turn had already been lost.
func (x *executionRun) releaseTurn() bool {
	if x.heldTurn != nil {
		lost := x.heldTurn.release()
		x.heldTurn = nil
		x.ctx = x.parentCtx
		return lost
	}
	return false
}

// runStages drives the journal stage until a stage returns a final state.
func (x *executionRun) runStages() (intake.State, error) {
	defer x.releaseTurn()
	for {
		x.conflict = false
		if x.ctx.Err() != nil {
			return intake.State{}, x.ctx.Err()
		}
		if out := x.beforeStage(); out.action == stageStop {
			return out.state, out.err
		} else if out.action == stageRepeat {
			continue
		}
		var out stageOutcome
		switch x.r.Stage {
		case "implement", "implement_report":
			out = x.implementStage()
		case "test", "test_report":
			out = x.testStage()
		case "review":
			out = x.reviewStage()
		case "publish":
			out = x.publishStage()
		case "merge":
			out = x.mergeStage()
		default:
			return intake.State{}, errors.New("保存済みの実行工程が不正です")
		}
		if out.action == stageStop {
			return out.state, out.err
		}
	}
}

// beforeStage runs the checks every iteration needs before its stage handler.
func (x *executionRun) beforeStage() stageOutcome {
	for _, check := range []func() stageOutcome{
		x.resumePendingMerge,
		x.checkTransferableReport,
		x.checkReleaseReviewRequest,
		x.checkFinalPublicationTree,
		x.followDefaultBranch,
		x.applyCarriedHeldReview,
	} {
		if out := check(); out.action != stageProceed {
			return out
		}
	}
	return proceed()
}

// resumePendingMerge hands an interrupted merge back to the conflict resolver.
func (x *executionRun) resumePendingMerge() stageOutcome {
	if x.r.Stage != "merge" || x.finalPublication {
		return proceed()
	}
	handled, err := x.e.resumeMergeConflict(x.ctx, x.d, x.r, x.workspace, &x.issue.Body)
	if err != nil {
		return fail(err)
	}
	if handled {
		return repeat()
	}
	return proceed()
}

// checkTransferableReport makes sure a repaired or resumed result is checked
// before any review path can publish it. An already merged pull takes
// precedence over a hold.
func (x *executionRun) checkTransferableReport() stageOutcome {
	e, r := x.e, x.r
	if r.Stage != "review" && r.Stage != "publish" && r.Stage != "merge" {
		return proceed()
	}
	if x.finalPublication && !hasJSON(r.Result) {
		return stop(e.holdFinalPublication(r, "実装報告を承認対象headに結び付けられません"))
	}
	if _, _, err := transferReport(r.Result); err != nil {
		merged, readErr := e.matchingMergedPull(x.ctx, x.d, r, x.workspace.Default)
		if readErr != nil {
			return fail(readErr)
		}
		if merged {
			return stop(e.finishMerged(x.ctx, r, x.fence, "公開済みPRの宛先とマージ済みコミットを確認しました", nil))
		}
		return stop(e.finish(r, "held", "review_held", "受入条件の移送を報告できません: "+err.Error(), nil))
	}
	return proceed()
}

// checkReleaseReviewRequest sends older planned releases with no review
// identity back through independent review.
func (x *executionRun) checkReleaseReviewRequest() stageOutcome {
	r := x.r
	if (r.Stage != "publish" && r.Stage != "merge") || !hasJSON(r.ExecutionPlan) || reviewBaselineMatches(r, x.d, x.requestHash) {
		return proceed()
	}
	rememberReview(r)
	r.Review = nil
	r.ReviewBaseline = nil
	r.ReviewSkipped = false
	r.CarriedHeldReview = nil
	return x.returnTo("review")
}

func (x *executionRun) checkFinalPublicationTree() stageOutcome {
	if !x.finalPublication {
		return proceed()
	}
	if reason := finalPublicationTreeCause(x.ctx, x.workspace, x.publication.HeadSHA); reason != "" {
		return stop(x.e.holdFinalPublication(x.r, reason))
	}
	return proceed()
}

// followDefaultBranch commits pending edits and follows the default branch
// before implementation, test and review. A pending merge belongs to the
// implementation conflict resolver.
func (x *executionRun) followDefaultBranch() stageOutcome {
	e, r, workspace := x.e, x.r, x.workspace
	if x.finalPublication || (r.Stage != "implement" && r.Stage != "implement_report" && r.Stage != "test" && r.Stage != "test_report" && r.Stage != "review") {
		return proceed()
	}
	unmerged, err := workspace.Unmerged(x.ctx)
	if err != nil {
		return fail(err)
	}
	if len(unmerged) > 0 {
		if r.Stage != "implement" {
			return fail(errors.New("実装段以外に未解決の競合があります"))
		}
		x.conflict = true
		r.ControllerAllowed = unmerged
		x.issue.Body = withConflictInstruction(x.issue.Body)
		return proceed()
	}
	dirty := !workspace.Clean(x.ctx)
	if dirty {
		if err := e.validatePending(x.ctx, workspace); err != nil {
			return stop(e.finish(r, "failed", "", err.Error(), nil))
		}
		if err := workspace.Stage(x.ctx); err != nil {
			return fail(err)
		}
		if _, err := workspace.Commit(x.ctx, x.issue.Title); err != nil {
			return fail(err)
		}
	}
	changed, followErr := workspace.Follow(x.ctx)
	if followErr != nil {
		if x.ctx.Err() != nil {
			return fail(x.ctx.Err())
		}
		if errors.Is(followErr, gitwork.ErrConflict) {
			conflicts, listErr := workspace.Unmerged(x.ctx)
			if listErr == nil && len(conflicts) > 0 {
				if x.releaseTurn() {
					return fail(context.Canceled)
				}
				if err := x.fence(x.ctx); err != nil {
					return fail(err)
				}
				r.ControllerAllowed = conflicts
				clearFollowedResults(r)
				x.issue.Body = withConflictInstruction(x.issue.Body)
				if err := e.checkpoint(x.ctx, x.d, r, "implement"); err != nil {
					return fail(err)
				}
				return repeat()
			}
		}
		return fail(fmt.Errorf("既定ブランチを取り込めません: %w", followErr))
	}
	head, base, err := workspace.RevisionState(x.ctx)
	if err != nil {
		return fail(fmt.Errorf("追従後の作業ツリーを確認できません: %w", err))
	}
	if dirty || changed || (r.Head != "" && (head != r.Head || base != r.Base)) {
		clearFollowedResults(r)
		if r.Stage == "review" {
			r.Head, r.Base = head, base
			if err := e.checkpoint(x.ctx, x.d, r, "test"); err != nil {
				return fail(err)
			}
			return repeat()
		}
	}
	r.Head, r.Base = head, base
	return proceed()
}

// applyCarriedHeldReview applies a held review carried from another PC before
// an implementation start is recorded. The delivered tree still passes the
// fixed test and the review-stage evidence checks; only the AI calls are withheld.
func (x *executionRun) applyCarriedHeldReview() stageOutcome {
	e, d, r := x.e, x.d, x.r
	if r.Stage != "implement" || x.conflict || r.CarriedHeldReview == nil {
		return proceed()
	}
	snapshot, err := x.workspace.Snapshot(x.ctx)
	if err != nil {
		return fail(err)
	}
	// The fixed test needs the prepared environment, and a later fallback to
	// the normal review needs the provider; both are otherwise set at the
	// implementation start. A setup failure takes the existing path.
	if !hasJSON(r.Result) && carriedReportReusable(r.PreviousResult) && carriedHeldMatches(r, d, x.requestHash, snapshot.Diff) && e.setup(x.ctx, x.repository, r) == nil {
		r.Provider = implementationProvider(d, r)
		r.Result = r.PreviousResult
		r.Head, r.Base = snapshot.Head, snapshot.Base
		if err := e.checkpoint(x.ctx, d, r, "test"); err != nil {
			return fail(err)
		}
		return repeat()
	}
	return proceed()
}

// holdOrFinish holds a final publication with summary, or otherwise ends the
// run with status and reason.
func (x *executionRun) holdOrFinish(status, reason, summary string) stageOutcome {
	if x.finalPublication {
		return stop(x.e.holdFinalPublication(x.r, summary))
	}
	return stop(x.e.finish(x.r, status, reason, summary, nil))
}
