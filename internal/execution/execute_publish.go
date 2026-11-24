package execution

import (
	"context"
	"errors"

	"issue-desk-worker/internal/gitwork"
)

// publishStage publishes the reviewed head as a PR and holds it for approvals
// or a manual merge, or moves on to the merge stage.
func (x *executionRun) publishStage() stageOutcome {
	// The stage does not change the scope, so a copy of the descriptor is current.
	e, d, r, workspace := x.e, x.d, x.r, x.workspace
	snapshot, err := workspace.SnapshotReusing(x.ctx, x.reusableSnapshot())
	if err != nil {
		return fail(err)
	}
	x.snapshot = snapshot
	if err := validateSnapshot(snapshot); err != nil {
		return x.holdOrFinish("failed", "", err.Error())
	}
	if snapshot.Head != r.Head || snapshot.Base != r.Base {
		return x.returnTo("test")
	}
	if reason := e.reviewTestEvidence(x.ctx, r, snapshot.Head); reason != "" {
		if x.ctx.Err() != nil {
			return fail(x.ctx.Err())
		}
		clearFollowedResults(r)
		return x.returnTo("test")
	}
	if r.ReviewSkipped {
		_, valid := loadReviewBaseline(e.Config.RuntimeDir, r)
		hash, comparable := reviewDiffHash(snapshot.Diff)
		if !valid || !reviewBaselineMatches(r, d, x.requestHash) || !comparable || r.ReviewBaseline.Decision != "approved" || hash != r.ReviewBaseline.Hash || digestReviewJSON(r.Review) != r.ReviewBaseline.ReviewHash {
			r.ReviewSkipped = false
			return x.returnTo("review")
		}
	}
	if err := e.pollCommandsFor(x.ctx, x.d.ID); err != nil {
		return fail(err)
	}
	if x.ctx.Err() != nil {
		return fail(x.ctx.Err())
	}
	if err := x.fence(x.ctx); err != nil {
		return fail(err)
	}
	if err := e.publish(x.ctx, d, r, workspace, x.issue.Title); err != nil {
		var hold *releaseHold
		if x.finalPublication && errors.As(err, &hold) {
			return stop(e.finish(r, "held", hold.reason, hold.summary, hold.view))
		}
		if errors.Is(err, errTargetBranchDiverged) {
			return stop(e.holdTargetBranch(x.ctx, r, x.fence, err))
		}
		return fail(err)
	}
	if err := e.postFollowupProposals(x.ctx, r); err != nil {
		return fail(err)
	}
	if merged, readErr := e.matchingMergedPull(x.ctx, d, r, workspace.Default); readErr != nil {
		return fail(readErr)
	} else if merged {
		return stop(e.finishMerged(x.ctx, r, x.fence, "公開済みPRの宛先とマージ済みコミットを確認しました", nil))
	}
	permissions, permissionErr := e.permissionFiles(snapshot, d.Repo)
	if permissionErr != nil {
		return stop(e.finish(r, "held", "review_held", permissionErr.Error(), nil))
	}
	if len(permissions) > 0 && !permissionApprovedContent(x.ctx, workspace, d, r.Head, r.PR, permissions) {
		return stop(e.finish(r, "held", "review_held", "権限の変更は利用者の承認を待っています", map[string]any{"permissionHold": map[string]any{"v": 1, "head": r.Head, "base": r.Base, "pr": r.PR, "files": permissions}, "releasePending": true}))
	}
	if sqlHold(snapshot) && field[struct {
		Head string `json:"head"`
	}](d.View, "deletionApproved").Head != r.Head {
		return stop(e.finish(r, "held", "deletions", "データ削除SQLの承認を待っています", map[string]any{"deletionApproval": map[string]any{"head": r.Head, "items": snapshot.Paths, "total": len(snapshot.Paths)}, "releasePending": true}))
	}
	if !x.repository.AutoMerge {
		return stop(e.finish(r, "held", "review_held", "PRを公開しました。手動マージを待っています", map[string]any{"releasePending": true}))
	}
	if err := e.checkpoint(x.ctx, d, r, "merge"); err != nil {
		return fail(err)
	}
	return repeat()
}

// mergeStage merges the published PR within a merge turn. A default branch
// that moved during the turn sends the followed tree back to the test stage
// while the turn stays held.
func (x *executionRun) mergeStage() stageOutcome {
	e, r := x.e, x.r
	merged, changed, err := e.mergeWithTurn(x.ctx, x.d, r, x.workspace, x.fence, &x.heldTurn, x.reusableSnapshot())
	if x.heldTurn != nil && x.heldTurn.ctx.Err() != nil {
		err = x.heldTurn.ctx.Err()
	}
	if err != nil || merged || !changed {
		if x.releaseTurn() && err == nil {
			err = context.Canceled
		}
	} else if x.heldTurn != nil {
		x.ctx = x.heldTurn.ctx
	}
	if x.finalPublication && errors.Is(err, gitwork.ErrConflict) && x.ctx.Err() == nil {
		return stop(e.holdFinalPublication(r, "既定ブランチとの競合を解消する必要があります"))
	}
	if errors.Is(err, gitwork.ErrConflict) && x.ctx.Err() == nil && !x.finalPublication {
		handled, conflictErr := e.resumeMergeConflict(x.ctx, x.d, r, x.workspace, &x.issue.Body)
		if conflictErr != nil {
			return fail(conflictErr)
		}
		if handled {
			return repeat()
		}
	}
	if err != nil {
		var hold *releaseHold
		if errors.As(err, &hold) {
			if x.finalPublication {
				hold.summary = finalPublicationReason(hold.summary)
				if hold.view == nil {
					hold.view = map[string]any{}
				}
				hold.view["finalApprovalHold"] = true
			}
			return stop(e.finish(r, "held", hold.reason, hold.summary, hold.view))
		}
		return fail(err)
	}
	if changed {
		return x.returnTo("test")
	}
	if !merged {
		return stop(e.finish(r, "held", "review_held", "マージの条件を確認できません", nil))
	}
	if err := e.flushUsage(x.ctx, r); err != nil {
		return fail(err)
	}
	return stop(e.finishMerged(x.ctx, r, x.fence, "固定テストと独立レビューを確認し、PRをマージしました", map[string]any{"releasePending": false, "publishPending": false}))
}

// returnTo checkpoints an earlier stage and starts the next loop iteration.
func (x *executionRun) returnTo(stage string) stageOutcome {
	if err := x.e.checkpoint(x.ctx, x.d, x.r, stage); err != nil {
		return fail(err)
	}
	return repeat()
}
