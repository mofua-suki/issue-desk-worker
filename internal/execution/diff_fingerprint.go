package execution

import (
	"context"
	"time"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// addDiffFingerprint lets the intake carry an approval across a head that
// only took in the default branch. The field is omitted whenever it cannot be
// computed, including a missing or ambiguous merge base.
func (e *Executor) addDiffFingerprint(ctx context.Context, r *journal.Record, state *intake.State) {
	state.DiffFingerprint = ""
	if r == nil || r.Tree == "" || !gitwork.ValidSHA(state.HeadSHA) {
		return
	}
	// The current default branch, so a later merge of it does not change the
	// merge base; the recorded base only stands in for older journals.
	base := r.Base
	if r.TargetBase != "" && gitwork.ValidBranchShape(r.TargetBase) {
		base = "refs/remotes/origin/" + r.TargetBase
	}
	if base == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	c := e.gitConfig()
	if fingerprint, err := gitwork.DiffFingerprint(ctx, c, r.Tree, base, state.HeadSHA); err == nil {
		state.DiffFingerprint = fingerprint
	}
}
