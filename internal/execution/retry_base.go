package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

func retryBaseApplied(d Descriptor, r *journal.Record) bool {
	return r.RetryBaseGeneration != nil && *r.RetryBaseGeneration == d.Generation
}
func retryBaseNeeded(d Descriptor, r *journal.Record) bool {
	if r == nil || (r.Stage == "done" && hasJSON(r.FinalState)) || retryBaseApplied(d, r) {
		return false
	}
	var value struct {
		Generation *int `json:"generation"`
	}
	return json.Unmarshal(d.View["retryFromBase"], &value) == nil && value.Generation != nil && *value.Generation == d.Generation
}
func retryBaseBranch(d Descriptor) string {
	return fmt.Sprintf("codex/issue-%d-job-%d-retry-%d", d.Issue, d.ID, d.Generation)
}

func (e *Executor) resetRetryBase(ctx context.Context, d Descriptor, r *journal.Record) error {
	discarded, err := e.retryDiscardedSources(d, r)
	if err != nil {
		return err
	}
	branch := retryBaseBranch(d)
	c := e.gitConfig()
	holds := 0
	if err := gitwork.PrepareRetry(ctx, c, d.Repo, d.ID, branch); errors.Is(err, gitwork.ErrRetryBranchExists) {
		// Another PC already worked on this generation, so its review rounds still count.
		holds = field[int](d.View, "reviewHolds")
	} else if err != nil {
		return err
	}
	r.Branch = branch
	r.ContinuationBranch = nil
	r.PR = 0
	r.PRCreationAttempted = false
	r.Head = ""
	r.Base = ""
	r.Tree = ""
	r.Session = ""
	r.Result = nil
	r.ReportCompletion = nil
	r.ReviewBaseline = nil
	r.CarriedHeldReview = nil
	r.ReviewSkipped = false
	rememberReview(r)
	r.Review = nil
	r.Test = nil
	r.FinalState = nil
	r.FinalReported = false
	r.ControllerAllowed = nil
	r.Stage = "implement"
	r.ReviewHead = ""
	r.ReviewHolds = holds
	generation := d.Generation
	r.RetryBaseGeneration = &generation
	r.DiscardedContinuationSources = discarded
	return e.Store.Save(r)
}
