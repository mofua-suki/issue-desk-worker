package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

var errTargetBranchDiverged = errors.New("対象枝の先頭が作業ツリーの先頭に含まれていません")
var errTargetBranchDirty = errors.New("作業ツリーに未コミットの変更があるため対象枝へ追従できません")

type targetBranchError struct {
	relation string
	dirty    bool
}

func (e *targetBranchError) Error() string { return errTargetBranchDiverged.Error() }
func (e *targetBranchError) Unwrap() error { return errTargetBranchDiverged }

func (e *Executor) holdTargetBranch(ctx context.Context, r *journal.Record, fence func(context.Context) error, cause error) (intake.State, error) {
	if err := ctx.Err(); err != nil {
		return intake.State{}, err
	}
	if err := fence(ctx); err != nil {
		return intake.State{}, err
	}
	if err := ctx.Err(); err != nil {
		return intake.State{}, err
	}
	var branchErr *targetBranchError
	if !errors.As(cause, &branchErr) {
		return intake.State{}, cause
	}
	reason := errTargetBranchDiverged.Error()
	if branchErr.dirty {
		reason = errTargetBranchDirty.Error()
	}
	return e.finish(r, "held", "review_held", reason, map[string]any{"releasePending": true, "targetBranchRelation": branchErr.relation})
}

// An acknowledged head-free dirty hold resumes its original stage. Treating
// the hold itself as prior work would invent a continuation source.
func restoreTargetDirtyStage(r *journal.Record) {
	if r.Stage != "held" || !r.FinalReported {
		return
	}
	var state intake.State
	var view map[string]json.RawMessage
	var result struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal(r.FinalState, &state) != nil || state.Status != "held" || state.HoldReason != "review_held" || json.Unmarshal(state.View, &view) != nil || json.Unmarshal(state.Result, &result) != nil || !strings.HasPrefix(result.Summary, errTargetBranchDirty.Error()) || hasJSON(view["continuationHead"]) {
		return
	}
	if field[string](view, "targetBranchRelation") != "behind" || !field[bool](view, "releasePending") {
		return
	}
	stage := field[struct {
		Stage string `json:"stage"`
	}](view, "workerExecution").Stage
	switch stage {
	case "implement", "implement_report", "test", "test_report", "review", "publish", "merge":
		r.Stage = stage
		r.FinalState = nil
	}
}

func checkTargetBranch(ctx context.Context, w *gitwork.Workspace, required bool) error {
	relation, err := w.TargetRelation(ctx, required)
	if err != nil {
		return err
	}
	if relation != "included" {
		return &targetBranchError{relation: relation}
	}
	return nil
}
