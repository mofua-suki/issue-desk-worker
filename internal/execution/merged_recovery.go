package execution

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// Recovery only observes an already completed merge; it does not issue a new merge or relax its gates.
func (e *Executor) matchingMergedPull(ctx context.Context, d Descriptor, r *journal.Record, base string) (bool, error) {
	if r == nil || r.PR < 1 || r.Head == "" {
		return false, nil
	}
	pull, err := e.Gitea.Pull(ctx, d.Repo, r.PR)
	if err != nil {
		return false, err
	}
	if !pull.Merged {
		return false, nil
	}
	if r.TargetBase != "" {
		base = r.TargetBase
	}
	if !samePull(pull, d.Repo, r.Branch, base, r.Head) || pull.Number != r.PR || pull.MergeCommitSHA != r.Head {
		return false, errors.New("既存のマージ済みPRの宛先・枝・head・merge_commitが一致しません")
	}
	return true, nil
}

// resumeIntakeMerge recognizes the checkpoint emitted immediately before the
// normal merge stage. A publication checkpoint can also describe a PR that is
// still waiting for approval, and a requeued generation can retain old PR fields.
func (e *Executor) resumeIntakeMerge(ctx context.Context, job intake.Job, d Descriptor, saved *journal.Record, base string, fence func(context.Context) error) (intake.State, bool, error) {
	started := field[struct {
		Stage      string `json:"stage"`
		Generation *int   `json:"generation"`
	}](d.View, "workerExecution")
	if d.Repo != job.Repo || !gitwork.ValidBranchShape(base) || !gitwork.ValidBranchShape(d.Branch) || !gitwork.ValidSHA(d.HeadSHA) || d.PR < 1 {
		return intake.State{}, false, nil
	}
	sameRecovery := saved != nil && saved.Generation == d.Generation && saved.PR == d.PR && saved.Head == d.HeadSHA && saved.Branch == d.Branch
	currentMerge := started.Stage == "merge" && started.Generation != nil && *started.Generation == d.Generation
	planSHA := field[string](d.View, "executionPlanSHA")
	// The current checkpoint identifies the target before its evidence is validated.
	preferIntake := currentMerge && (!sameRecovery || saved.ExecutionPlanSHA != planSHA)
	if saved != nil && !saved.IntakeMergeRecovery && saved.Generation == d.Generation && !preferIntake {
		return intake.State{}, false, nil
	}
	if !sameRecovery && !currentMerge {
		return intake.State{}, false, nil
	}
	r := &journal.Record{
		ID: d.ID, Repo: d.Repo, Issue: d.Issue, Generation: d.Generation, Epoch: job.LeaseEpoch,
		Stage: "merge", Branch: d.Branch, PR: d.PR, Head: d.HeadSHA, TargetBase: base, Provider: d.Provider,
		IntakeMergeRecovery: true,
	}
	// A local journal cannot establish that intake's checkpoint belongs to this retry.
	retry := retryBaseNeeded(d, r)
	_, finalApproval := selectFinalApprovalPublication(d)
	if (retry && (!currentMerge || r.Branch != retryBaseBranch(d))) || finalApproval {
		if preferIntake {
			return closedIssueState(), true, nil
		}
		return intake.State{}, false, nil
	}
	merged, err := e.matchingMergedPull(ctx, d, r, base)
	if err == nil && !merged && preferIntake {
		return closedIssueState(), true, nil
	}
	if err != nil || !merged {
		return intake.State{}, err != nil, err
	}
	if err := fence(ctx); err != nil {
		return intake.State{}, true, err
	}
	if err := ctx.Err(); err != nil {
		return intake.State{}, true, err
	}
	if retry {
		generation := d.Generation
		r.RetryBaseGeneration = &generation
	}
	saveExecutionAdmission(r, d)
	r.Result = intakeImplementationReport(d)
	r.ExecutionPlan, r.ExecutionPlanSHA = d.View["executionPlan"], planSHA
	trackFailureRecord(ctx, r)
	view := map[string]any{"releasePending": false, "publishPending": false}
	if err := restoreMergedEvidence(d, r); err != nil {
		out, err := e.holdMergedExecution(r, view, err)
		return out, true, err
	}
	out, err := e.finishConfirmedMerge(ctx, r, fence, "受付の記録とマージ済みPRを照合しました", view)
	return out, true, err
}

type executionReceipt struct {
	Requirement string `json:"requirement"`
	Generation  *int   `json:"generation"`
	RecordID    string `json:"recordID"`
	PlanSHA     string `json:"planSHA"`
	Head        string `json:"head"`
	Status      string `json:"status"`
	Source      string `json:"source"`
	Detail      string `json:"detail"`
}

func restoreMergedEvidence(d Descriptor, r *journal.Record) error {
	plan, err := parseExecutionPlan(d)
	if err != nil {
		return err
	}
	if plan == nil {
		if r.ExecutionPlanSHA != "" {
			return errors.New("引継ぎ先で受入条件の実行計画を確認できません")
		}
		return nil
	}
	digest, err := hex.DecodeString(r.ExecutionPlanSHA)
	if err != nil || len(digest) != 32 {
		return errors.New("引継ぎ先で受入条件の実行計画を確認できません")
	}

	var receipts []executionReceipt
	var test json.RawMessage
	seen := make(map[string]bool)
	for _, requirement := range plan.Requirements {
		if seen[requirement.ID] {
			return errors.New("引継ぎ先の受入条件IDが重複しています")
		}
		seen[requirement.ID] = true
		if requirement.EvidenceKind == "external_check" {
			continue
		}
		if receipts == nil && (json.Unmarshal(d.View["executionReceipts"], &receipts) != nil || len(receipts) == 0) {
			return errors.New("引継ぎ先で受付の受入条件証跡を確認できません")
		}
		matches := 0
		for _, receipt := range receipts {
			if receipt.Requirement != requirement.ID || receipt.Generation == nil || *receipt.Generation != r.Generation || receipt.PlanSHA != r.ExecutionPlanSHA || receipt.Head != r.Head {
				continue
			}
			if receipt.Source != requirement.Owner || receipt.Status != "passed" || strings.TrimSpace(receipt.RecordID) == "" || strings.TrimSpace(receipt.Detail) == "" {
				return errors.New("引継ぎ先で受入条件の担当または合格根拠を確認できません: " + requirement.ID)
			}
			matches++
		}
		if matches != 1 {
			return errors.New("引継ぎ先で対象headの受入条件証跡を一意に確認できません: " + requirement.ID)
		}
		if requirement.EvidenceKind == "fixed_test" && test == nil {
			test, err = mergedTestRecord(d, r)
			if err != nil {
				return err
			}
		}
	}
	r.Test = test
	return nil
}

// The receipt belongs to the test-producing lease, which can precede this lease.
func mergedTestRecord(d Descriptor, r *journal.Record) (json.RawMessage, error) {
	var record struct {
		Host         string          `json:"host"`
		Generation   *int            `json:"generation"`
		Epoch        int64           `json:"epoch"`
		At           time.Time       `json:"at"`
		Test         json.RawMessage `json:"test"`
		ReportedHead string          `json:"reportedHead"`
	}
	var tested TestResult
	if json.Unmarshal(d.View["executionTestRecord"], &record) != nil ||
		strings.TrimSpace(record.Host) == "" || record.Generation == nil || *record.Generation != r.Generation ||
		record.Epoch < 1 || record.Epoch > r.Epoch || record.At.IsZero() || record.ReportedHead != r.Head ||
		json.Unmarshal(record.Test, &tested) != nil || !tested.Passed || testEvidenceReason(tested, r.Head) != "" {
		return nil, errors.New("引継ぎ先で対象headの固定テスト証跡を確認できません")
	}
	return record.Test, nil
}
