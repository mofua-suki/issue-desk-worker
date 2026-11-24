package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

// reviewStage validates the tested tree and its change scope, then runs the
// independent review, or reuses a held or skipped review for the same diff.
func (x *executionRun) reviewStage() stageOutcome {
	e, r := x.e, x.r
	snapshot, err := x.workspace.SnapshotReusing(x.ctx, x.reusableSnapshot())
	if err != nil {
		return fail(err)
	}
	x.snapshot = snapshot
	r.Base = snapshot.Base
	r.Head = snapshot.Head
	if reason := e.reviewTestEvidence(x.ctx, r, snapshot.Head); reason != "" {
		if x.ctx.Err() != nil {
			return fail(x.ctx.Err())
		}
		var invalid TestResult
		if json.Unmarshal(r.Test, &invalid) == nil {
			rejectTestEvidence(&invalid, reason)
			r.Test = encode(invalid)
		}
		return x.holdOrFinish("held", "review_held", reason)
	}
	if err := x.workspace.ValidateFiles(snapshot.Paths); err != nil {
		return x.holdOrFinish("failed", "", err.Error())
	}
	if err := validateSnapshot(snapshot); err != nil {
		return x.holdOrFinish("failed", "", err.Error())
	}
	if out := x.settleReviewScope(snapshot); out.action != stageProceed {
		return out
	}
	diffFile, err := x.writeReviewDiff(snapshot)
	if err != nil {
		return fail(err)
	}
	kind, extra, files := e.prepareReview(x.ctx, r, x.workspace, snapshot, diffFile, x.d, x.requestHash)
	if r.ReviewUsageRecovery != nil && kind != "skipped" {
		// A recovery record is single-use; a changed target must not revive it later.
		r.ReviewBaseline = nil
		r.ReviewUsageRecovery = nil
		if err := e.Store.Save(r); err != nil {
			return fail(err)
		}
	}
	if len(files) > 1 {
		x.removeOnReturn = append(x.removeOnReturn, files[1])
	}
	carried := kind == "full" && r.ReviewBaseline == nil && carriedHeldMatches(r, x.d, x.requestHash, snapshot.Diff)
	if carried {
		// No local baseline exists; the carried review is bound to this diff.
		r.Review = firstJSON(r.Review, r.PreviousReview)
		restoreDeferredDeliveryResults(r, r.Review)
		kind = "held"
	}
	switch kind {
	case "held":
		return x.holdUnchangedReview(carried)
	case "skipped":
		return x.skipReview(snapshot)
	}
	prompt := reviewPrompt(x.d, x.issue.Title, x.issue.Body, r, snapshot, diffFile, kind)
	answer, callErr := e.callAI(withReviewDiffSize(x.ctx, snapshot.Paths, snapshot.Diff), x.d, r, prompt+extra, true, files)
	var recoveredReview Review
	var recoveredNormalized json.RawMessage
	var recoveredRepeated []string
	savedRecovery := false
	if callErr == nil && !x.finalPublication && len(snapshot.Paths) > 0 && x.ctx.Err() == nil {
		review, normalized, repeated := decideReview(r, answer.JSON)
		_, comparable := reviewDiffHash(snapshot.Diff)
		if review.Decision == "approve" && review.Approved() && comparable && x.workspace.Clean(x.ctx) {
			if err := x.fence(x.ctx); err != nil {
				return fail(err)
			}
			if err := x.ctx.Err(); err != nil {
				return fail(err)
			}
			current, err := x.workspace.SnapshotReusing(x.ctx, snapshot)
			if err != nil {
				return fail(err)
			}
			if !x.workspace.Clean(x.ctx) || current.Head != snapshot.Head || current.Base != snapshot.Base || current.Diff != snapshot.Diff {
				return fail(errors.New("レビュー後に対象が変わりました"))
			}
			r.Review = normalized
			r.ReviewUsageRecovery = &journal.ReviewUsageRecovery{Head: snapshot.Head, Base: snapshot.Base, PatchHash: digestReview(snapshot.Diff)}
			if err := e.saveReviewBaseline(r, snapshot, review, normalized, kind, x.requestHash); err != nil {
				return fail(err)
			}
			recoveredReview, recoveredNormalized, recoveredRepeated, savedRecovery = review, normalized, repeated, true
		}
	}
	if err := e.flushUsage(x.ctx, r); err != nil {
		return fail(err)
	}
	if err := x.ctx.Err(); err != nil {
		return fail(err)
	}
	if callErr != nil {
		if x.finalPublication {
			return stop(e.holdFinalPublication(r, "独立レビューを完了できません: "+callErr.Error()))
		}
		var exhausted *transientExhausted
		if errors.As(callErr, &exhausted) {
			return stop(e.finish(r, "failed", "", callErr.Error(), nil))
		}
		return stop(e.finish(r, "held", "review_held", callErr.Error(), nil))
	}
	finalHead := x.finalPublication && r.Head == x.publication.HeadSHA
	review, normalized, repeated := recoveredReview, recoveredNormalized, recoveredRepeated
	if !savedRecovery {
		review, normalized, repeated = decideReview(r, answer.JSON, finalHead)
	}
	if finalHead {
		review, normalized = removeRecordedFinalApproval(review, normalized, repeated)
	}
	r.Review = normalized
	if !x.workspace.Clean(x.ctx) {
		return x.holdOrFinish("held", "review_held", "読み取り専用レビュー中に作業ツリーが変わりました")
	}
	if !savedRecovery {
		if err := e.saveReviewBaseline(r, snapshot, review, normalized, kind, x.requestHash); err != nil {
			return fail(err)
		}
	}
	if !review.Approved() {
		return x.reworkAfterReview(review, repeated)
	}
	if len(snapshot.Paths) == 0 {
		if err := x.fence(x.ctx); err != nil {
			return fail(err)
		}
		if err := e.postFollowupProposals(x.ctx, r); err != nil {
			return fail(err)
		}
		return x.closeSatisfiedIssue()
	}
	r.ReviewUsageRecovery = nil
	if err := e.checkpoint(x.ctx, x.d, r, "publish"); err != nil {
		return fail(err)
	}
	return repeat()
}

// settleReviewScope asks the intake to widen the job scope to every changed
// path outside it, or narrows the scope to the changed paths. A wait for an
// overlapping scope releases the job.
func (x *executionRun) settleReviewScope(snapshot gitwork.Snapshot) stageOutcome {
	e, r, workspace := x.e, x.r, x.workspace
	outside := []string{}
	for _, name := range snapshot.Paths {
		if !scopeContains(x.d.Scope, name) {
			outside = append(outside, name)
		}
	}
	if len(outside) > 0 {
		if err := x.fence(x.ctx); err != nil {
			return fail(err)
		}
		if x.finalPublication {
			if err := e.checkPublishedTarget(x.ctx, x.d, workspace); err != nil {
				var h *releaseHold
				if errors.As(err, &h) {
					return stop(e.finish(r, "held", h.reason, h.summary, h.view))
				}
				return fail(err)
			}
		} else if err := workspace.Push(x.ctx); err != nil {
			return fail(err)
		}
		var widened struct {
			Result string     `json:"result"`
			Job    Descriptor `json:"job"`
		}
		if err := e.API.Call(x.ctx, "POST", fmt.Sprintf("/api/v1/jobs/%d/scope", x.d.ID), map[string]any{"epoch": r.Epoch, "add": outside}, &widened); err != nil {
			return fail(err)
		}
		if widened.Job.ID != x.d.ID {
			return fail(errors.New("変更範囲の応答が対象ジョブと一致しません"))
		}
		if widened.Result == "granted" {
			x.d.Scope = widened.Job.Scope
			return repeat()
		}
		if widened.Result != "wait" {
			return fail(errors.New("変更範囲の応答が不正です"))
		}
		_ = e.recordScopeRelease(r) // Diagnostic failure must not prevent saving the released result.
		state, saveErr := e.finish(r, "held", "scope_wait", "同じリポジトリの実行中の変更範囲との重複が解消するまで待っています", nil)
		if saveErr != nil {
			return stop(state, releasePersistenceFailure(r.ID, r.Epoch))
		}
		r.FinalReported = true
		if err := e.Store.Save(r); err != nil {
			return stop(state, releasePersistenceFailure(r.ID, r.Epoch))
		}
		return stop(state, intake.ErrJobReleased)
	}
	pushForScope := workspace.Push
	if x.finalPublication {
		pushForScope = func(ctx context.Context) error { return e.checkPublishedTarget(ctx, x.d, workspace) }
	}
	if err := e.narrowScope(x.ctx, &x.d, r.Epoch, snapshot.Paths, x.fence, pushForScope, r, x.narrowAttempts); err != nil {
		var h *releaseHold
		if x.finalPublication && errors.As(err, &h) {
			return stop(e.finish(r, "held", h.reason, h.summary, h.view))
		}
		return fail(err)
	}
	return proceed()
}

// writeReviewDiff stores the diff the reviewer reads. The file is removed when
// execute returns.
func (x *executionRun) writeReviewDiff(snapshot gitwork.Snapshot) (string, error) {
	reviewDir := filepath.Join(x.e.Config.RuntimeDir, "workspaces", ".review", fmt.Sprint(x.d.ID))
	if err := os.MkdirAll(reviewDir, 0700); err != nil {
		return "", err
	}
	diffFile := filepath.Join(reviewDir, "diff-"+providers.UUID()+".patch")
	if err := os.WriteFile(diffFile, []byte(snapshot.Diff), 0444); err != nil {
		return "", err
	}
	x.removeOnReturn = append(x.removeOnReturn, diffFile)
	return diffFile, nil
}

// holdUnchangedReview holds a diff that did not change since a review sent it back.
func (x *executionRun) holdUnchangedReview(carried bool) stageOutcome {
	r := x.r
	if !carried {
		restoreRetainedBaseline(r)
	}
	r.ReviewSkipped = false
	var review Review
	_ = json.Unmarshal(r.Review, &review) // The baseline was validated by prepareReview.
	var findings []string
	for i, finding := range review.Findings {
		if i >= len(review.Severities) || review.Severities[i] != "minor" || i >= len(review.Kinds) || (review.Kinds[i] != "acceptance" && review.Kinds[i] != "other" && review.Kinds[i] != "regression") {
			findings = append(findings, finding)
		}
	}
	return x.holdOrFinish("held", "review_held", "前回の差し戻しから差分が変わっていません。"+review.holdSummary(findings...))
}

// skipReview reuses an approved review for an unchanged diff.
func (x *executionRun) skipReview(snapshot gitwork.Snapshot) stageOutcome {
	r := x.r
	restoreRetainedBaseline(r)
	r.ReviewUsageRecovery = nil
	r.ReviewSkipped = true
	r.AddReviewRound(journal.ReviewRound{Kind: "skipped", Base: snapshot.Base, Head: snapshot.Head})
	if len(snapshot.Paths) == 0 {
		if err := x.e.Store.Save(r); err != nil {
			return fail(err)
		}
		if err := x.fence(x.ctx); err != nil {
			return fail(err)
		}
		return x.closeSatisfiedIssue()
	}
	if err := x.e.checkpoint(x.ctx, x.d, r, "publish"); err != nil {
		return fail(err)
	}
	return repeat()
}

// reworkAfterReview sends a rejected review back to implementation, or holds
// it when it needs a user decision or exceeds the run limit.
func (x *executionRun) reworkAfterReview(review Review, repeated []string) stageOutcome {
	e, r := x.e, x.r
	if x.releaseTurn() {
		return fail(context.Canceled)
	}
	if x.finalPublication {
		return stop(e.holdFinalPublication(r, "独立レビューが差し戻されました: "+review.holdSummary(repeated...)))
	}
	if err := e.sendReviewNotes(x.ctx, r, x.fence, false); err != nil {
		return fail(err)
	}
	if r.ReviewHead != r.Head {
		r.ReviewHolds++
		r.ReviewHead = r.Head
	}
	if review.Breaking || len(review.UserJudgments) > 0 || len(review.Uncertainties) > 0 || len(repeated) > 0 {
		return stop(e.publishHeld(x.ctx, x.d, r, x.workspace, x.fence, "review_held", review.holdSummary(repeated...)))
	}
	if !review.TooLarge {
		if _, err := e.noteAntigravityFailure(x.ctx, x.d, r, "review", strings.Join(review.Findings, "\n")); err != nil {
			return fail(err)
		}
	}
	// A rework does not advance the generation, so it is counted on its own before
	// it starts. Saved with the checkpoint, the count survives a restart or a move.
	// A generation not started yet is counted by its own start instead.
	if generationStarted(r, x.generationRuns) {
		if total := x.generationRuns + nonGenerationRuns(r) + 1; total > e.implementationRunLimit(x.d) {
			// Report the refused total: an over-limit approval raises the limit from this value.
			r.Runs = total
			return stop(e.finish(r, "held", "question", "実装回数の上限に達しました", map[string]any{"implementationRunLimit": true}))
		}
		r.ReviewReworkRuns++
		r.Runs = x.generationRuns + nonGenerationRuns(r)
		x.reworked = true
	}
	r.Session = ""
	if err := e.checkpoint(x.ctx, x.d, r, "implement"); err != nil {
		return fail(err)
	}
	return repeat()
}
